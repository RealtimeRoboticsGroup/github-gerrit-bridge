package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

var (
	crValues = map[string]string{"-2": "", "-1": "", " 0": "", "+1": "", "+2": ""}
	vValues  = map[string]string{"-1": "", " 0": "", "+1": ""}
	alice    = gerrit.AccountInfo{AccountID: 1, Name: "Alice"}
	bob      = gerrit.AccountInfo{AccountID: 2, Name: "Bob"}
	ci       = gerrit.AccountInfo{AccountID: 3, Name: "Buildkite"}
)

func change(cr, v []gerrit.ApprovalInfo) *gerrit.ChangeInfo {
	return &gerrit.ChangeInfo{Labels: map[string]gerrit.LabelInfo{
		"Code-Review": {All: cr, Values: crValues},
		"Verified":    {All: v, Values: vValues},
	}}
}

func byContext(ss []github.Status) map[string]github.Status {
	m := map[string]github.Status{}
	for _, s := range ss {
		m[s.Context] = s
	}
	return m
}

func TestStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ch      *gerrit.ChangeInfo
		run     ciRun
		trusted bool
		cr, v   string // state: description
		vURL    string
	}{
		{"nothing yet, trusted uploader", change(nil, nil), ciRun{}, true,
			"pending: No votes yet", "pending: Waiting for CI", "https://g/c/5"},
		{"untrusted uploader", change(nil, nil), ciRun{}, false,
			"pending: No votes yet", "pending: CI not started: uploader is not in Verified Users. A trusted reviewer can comment retest (here or in Gerrit).", "https://g/c/5"},
		{"build running", change(nil, []gerrit.ApprovalInfo{{AccountInfo: ci, Value: 0}}), ciRun{"Started", "https://bk/1"}, true,
			"pending: No votes yet", "pending: CI running", "https://bk/1"},
		{"approved and verified", change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2}, {AccountInfo: bob, Value: 1}},
			[]gerrit.ApprovalInfo{{AccountInfo: ci, Value: 1}}), ciRun{"Succeeded", "https://bk/2"}, true,
			"success: +2 Alice, +1 Bob", "success: +1 Buildkite", "https://bk/2"},
		{"blocked and failed", change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2}, {AccountInfo: bob, Value: -2}},
			[]gerrit.ApprovalInfo{{AccountInfo: ci, Value: -1}}), ciRun{"Failed", "https://bk/3"}, true,
			"failure: -2 Bob", "failure: -1 Buildkite", "https://bk/3"},
		{"weak votes only", change([]gerrit.ApprovalInfo{{AccountInfo: bob, Value: -1}}, nil), ciRun{"Failed", "https://bk/4"}, true,
			"pending: -1 Bob", "pending: CI failed; waiting for the vote", "https://bk/4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := byContext(desiredStatuses(tc.ch, "https://g/c/5", []string{"Verified"}, tc.run, tc.trusted, "Verified Users"))
			if s := got["gerrit/Code-Review"]; s.State+": "+s.Description != tc.cr || s.TargetURL != "https://g/c/5" {
				t.Errorf("Code-Review = %+v, want %q", s, tc.cr)
			}
			if s := got["gerrit/Verified"]; s.State+": "+s.Description != tc.v || s.TargetURL != tc.vURL {
				t.Errorf("Verified = %+v, want %q at %s", s, tc.v, tc.vURL)
			}
		})
	}
}

func TestSubmittableStatus(t *testing.T) {
	ch := change(nil, nil)
	ch.SubmitRequirements = []gerrit.SubmitRequirementResult{
		{Name: "Code-Review", Status: "UNSATISFIED"}, {Name: "Verified", Status: "SATISFIED"}, {Name: "No-Unresolved-Comments", Status: "UNSATISFIED"},
	}
	s := byContext(desiredStatuses(ch, "u", nil, ciRun{}, true, ""))[submittableStatus]
	if s.State != "pending" || s.Description != "Needs Code-Review, No-Unresolved-Comments" {
		t.Errorf("submittable = %+v", s)
	}
	ch.Submittable = true
	if s := byContext(desiredStatuses(ch, "u", nil, ciRun{}, true, ""))[submittableStatus]; s.State != "success" {
		t.Errorf("submittable = %+v", s)
	}
}

func TestLatestCIRunOnlyCurrentPatchSet(t *testing.T) {
	ci := gerrit.AccountInfo{Username: "buildkite"}
	ch := &gerrit.ChangeInfo{Messages: []gerrit.ChangeMessage{
		{RevisionNumber: 1, Author: ci, Message: "Build Failed: https://bk/1"},
		{RevisionNumber: 2, Author: ci, Message: "Patch Set 2:\n\nBuild Started: https://bk/2"},
		{RevisionNumber: 2, Author: ci, Message: "Patch Set 2: Verified+1\n\nBuild Succeeded: https://bk/2"},
		{RevisionNumber: 2, Message: "Patch Set 2: Code-Review+2"},
		// Anyone can write CI's words, in Gerrit or (mirrored) on GitHub.
		{RevisionNumber: 2, Author: gerrit.AccountInfo{Username: "mallory"}, Message: "Build Failed: https://evil/"},
		{RevisionNumber: 2, Author: gerrit.AccountInfo{Username: "buildkite"}, Tag: mirrorTag, Message: "Build Failed: https://evil/"},
	}}
	if run := latestCIRun(ch, 2, DefaultCIMessage, "buildkite"); run != (ciRun{"Succeeded", "https://bk/2"}) {
		t.Errorf("run = %+v", run)
	}
	// Without a CI account, nobody's message is trusted for the link.
	if run := latestCIRun(ch, 2, DefaultCIMessage, ""); run != (ciRun{}) {
		t.Errorf("without a CI account: %+v", run)
	}
	if run := latestCIRun(ch, 3, DefaultCIMessage, "buildkite"); run != (ciRun{}) {
		t.Errorf("run on ps 3 = %+v", run)
	}
}

func TestLatestBuild(t *testing.T) {
	builds := []buildkite.Build{
		{Number: 4, State: "failed", WebURL: "u4"},
		{Number: 6, State: "running", WebURL: "u6"},
		{Number: 5, State: "passed", WebURL: "u5"},
	}
	if run := latestBuild(builds); run != (ciRun{"Started", "u6"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	builds[1].State = "canceled"
	if run := latestBuild(builds); run != (ciRun{"Failed", "u6"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	if run := latestBuild(builds[2:]); run != (ciRun{"Succeeded", "u5"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	if run := latestBuild(nil); run != (ciRun{}) {
		t.Errorf("latestBuild(nil) = %+v", run)
	}
}

// checksFakes are what Checks.Sync reads and writes.
type checksFakes struct {
	ch    *gerrit.ChangeInfo
	set   []string // contexts written
	shas  []string // and on which commits
	have  []github.Status
	group []gerrit.AccountInfo
}

func (f *checksFakes) GetChange(context.Context, int, ...string) (*gerrit.ChangeInfo, error) {
	return f.ch, nil
}
func (f *checksFakes) GroupMembers(context.Context, string) ([]gerrit.AccountInfo, error) {
	return f.group, nil
}
func (f *checksFakes) ChangeURL(string, int) string { return "https://g/c/5" }
func (f *checksFakes) Statuses(context.Context, string) ([]github.Status, error) {
	return f.have, nil
}
func (f *checksFakes) CreateStatus(_ context.Context, sha string, st github.Status) error {
	f.set = append(f.set, st.Context)
	f.shas = append(f.shas, sha)
	f.have = append(f.have, st)
	return nil
}
func (f *checksFakes) CreateIssueComment(context.Context, int, string) (*github.IssueComment, error) {
	return &github.IssueComment{}, nil
}

// The statuses someone's votes and uploads change count against their
// budget (fourth audit: anyone registered can vote on every open change,
// over and over); CI's, and the trusted group's, do not.
func TestStatusesWithinTheirCausesBudget(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	store := &state.Store{Repo: s.mirror(ctx)}
	s.save(ctx, store, 5, &state.Change{State: state.ChangeState{Change: 5, PR: 50},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "g1", GitHubSHA: "h1"}}})
	mallory := gerrit.AccountInfo{AccountID: 9, Name: "Mallory", Username: "mallory"}
	bk := gerrit.AccountInfo{AccountID: 3, Name: "Buildkite", Username: "buildkite"}
	f := &checksFakes{ch: &gerrit.ChangeInfo{Status: "NEW", CurrentRevision: "g1",
		Revisions: map[string]gerrit.RevisionInfo{"g1": {Number: 1, Created: "2026-10-19 10:00:00.000000000", Uploader: alice}},
		Labels: map[string]gerrit.LabelInfo{
			"Code-Review": {Values: crValues, All: []gerrit.ApprovalInfo{{AccountInfo: mallory, Value: -1, Date: "2026-10-19 11:45:00.000000000"}}},
			"Verified":    {Values: vValues, All: []gerrit.ApprovalInfo{{AccountInfo: bk, Value: 1, Date: "2026-10-19 11:30:00.000000000"}}},
		}}}
	c := &Checks{Project: "p", Gerrit: f, GitHub: f, Store: store, CILabels: []string{"Verified"}, CIAccount: "buildkite",
		StatusBudget: &Budget{N: 1, Per: time.Hour}, Logger: slog.Default()}
	c.StatusBudget.Take(ctx, bk.AccountID) // CI's would be spent, if it had one
	if err := c.Sync(ctx, 5); err != nil {
		t.Fatal(err)
	}
	// Mallory's vote is the newest on Code-Review and on the change.
	if !slices.Equal(f.set, []string{"gerrit/Code-Review", "gerrit/Verified"}) {
		t.Errorf("set %v, want Code-Review (mallory's 1) and Verified (CI's, free), not submittability", f.set)
	}
	// An hour on, it is written too; and a trusted voter's are free.
	c.StatusBudget.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	f.ch.Labels["Code-Review"] = gerrit.LabelInfo{Values: crValues, All: []gerrit.ApprovalInfo{{AccountInfo: mallory, Value: -1, Date: "2026-10-19 11:45:00.000000000"},
		{AccountInfo: bob, Value: 2, Date: "2026-10-19 12:00:00.000000000"}}}
	c.StatusBudget.Trusted = func(_ context.Context, a int) (bool, bool) { return a == bob.AccountID, true }
	f.set = nil
	if err := c.Sync(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.set, []string{"gerrit/Code-Review", "gerrit/submittable"}) {
		t.Errorf("set %v, want Code-Review and submittability, bob's", f.set)
	}
	// The bridge's upload (a GitHub push) is not charged: one budget for
	// everyone without a Gerrit account would let one hold up all.
	c.BridgeAccount = 9
	c.StatusBudget.now = func() time.Time { return time.Now().Add(4 * time.Hour) }
	c.StatusBudget.Take(ctx, 9)
	f.ch.Labels["Code-Review"] = gerrit.LabelInfo{Values: crValues, All: []gerrit.ApprovalInfo{{AccountInfo: mallory, Value: 1, Date: "2026-10-19 14:00:00.000000000"}}}
	f.set = nil
	if err := c.Sync(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.set, "gerrit/Code-Review") {
		t.Errorf("set %v: the bridge-caused status waited", f.set)
	}
	c.BridgeAccount = 0
	// A newer upload is its uploader's doing.
	if who := statusCause(f.ch, gerrit.RevisionInfo{Created: "2026-10-19 13:00:00.000000000", Uploader: mallory}, "gerrit/Code-Review"); who.AccountID != 9 {
		t.Errorf("upload's status caused by %+v", who)
	}
}

// A patch set the bridge does not push to GitHub (someone else's, on a
// GitHub-born PR) leaves the PR's head an older patch set's: its statuses
// say they are out of date, not the older votes, until a newer patch set
// is on GitHub. gerrit/sync is the importer's.
func TestStatusesOfAnOlderPatchSetSayTheyAreOutOfDate(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	store := &state.Store{Repo: s.mirror(ctx)}
	s.save(ctx, store, 5, &state.Change{State: state.ChangeState{Change: 5, PR: 50},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "g1", GitHubSHA: "h1"}}})
	f := &checksFakes{ch: &gerrit.ChangeInfo{Status: "NEW", CurrentRevision: "g2",
		Revisions: map[string]gerrit.RevisionInfo{
			"g1": {Number: 1, Created: "2026-10-19 10:00:00.000000000", Uploader: alice},
			"g2": {Number: 2, Created: "2026-10-19 12:00:00.000000000", Uploader: bob}},
		Labels: map[string]gerrit.LabelInfo{"Code-Review": {Values: crValues}, "Verified": {Values: vValues}}},
		have: []github.Status{
			{Context: "gerrit/Code-Review", State: "success", Description: "+2 Alice"},
			{Context: "gerrit/Verified", State: "success", Description: "+1 buildkite"},
			{Context: submittableStatus, State: "success", Description: "Ready to submit"},
			{Context: SyncStatus, State: "success", Description: "In sync with Gerrit change 5, patch set 1"},
			{Context: "ci/other", State: "success", Description: "someone else's"},
		}}
	c := &Checks{Project: "p", Gerrit: f, GitHub: f, Store: store, CILabels: []string{"Verified"}, CIAccount: "buildkite", Logger: slog.Default(),
		StatusBudget: &Budget{N: 1, Per: time.Hour}}
	// Patch set 2's upload makes them: bob's budget, spent.
	c.StatusBudget.Take(ctx, bob.AccountID)
	if err := c.Sync(ctx, 5); err != nil || len(f.set) != 0 {
		t.Fatalf("set %v, err %v; want them to wait for bob's budget", f.set, err)
	}
	c.StatusBudget = nil
	if err := c.Sync(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.set, []string{"gerrit/Code-Review", "gerrit/Verified", submittableStatus}) || !slices.Equal(f.shas, []string{"h1", "h1", "h1"}) {
		t.Fatalf("set %v on %v, want the gerrit/ statuses but gerrit/sync on h1", f.set, f.shas)
	}
	for _, st := range f.have[5:] {
		if st.State != "pending" || st.Description != "Out of date: Gerrit's patch set 2 is not on GitHub; see its votes there" || st.TargetURL != "https://g/c/5" {
			t.Errorf("%s = %+v", st.Context, st)
		}
	}
	// Said once: not again on the next sync.
	f.have, f.set, f.shas = f.have[3:], nil, nil
	if err := c.Sync(ctx, 5); err != nil || len(f.set) != 0 {
		t.Fatalf("set again: %v, err %v", f.set, err)
	}
	// Once patch set 2 is on GitHub, its own go there.
	st, err := store.Load(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	st.Revisions = append(st.Revisions, state.Revision{PatchSet: 2, GerritSHA: "g2", GitHubSHA: "h2"})
	if err := store.Save(ctx, 5, st, "patch set 2 on GitHub"); err != nil {
		t.Fatal(err)
	}
	f.have = nil
	if err := c.Sync(ctx, 5); err != nil || len(f.shas) == 0 || slices.ContainsFunc(f.shas, func(sha string) bool { return sha != "h2" }) {
		t.Fatalf("set %v on %v, err %v; want patch set 2's on h2", f.set, f.shas, err)
	}
}

// The bridge will not run as a member of CI's trusted group, nor without
// knowing it is not one.
func TestBridgeAccountMustNotBeTrusted(t *testing.T) {
	ctx := context.Background()
	f := &checksFakes{group: []gerrit.AccountInfo{{AccountID: 5}}}
	c := &Checks{Gerrit: f, TrustedGroup: "Verified Users", Logger: slog.Default()}
	if err := c.CheckUntrusted(ctx, 9); err != nil {
		t.Errorf("an untrusted bridge refused: %v", err)
	}
	c.trusted = nil
	if err := c.CheckUntrusted(ctx, 5); err == nil {
		t.Error("a trusted bridge started")
	}
	c = &Checks{Gerrit: failingGroups{f}, TrustedGroup: "Verified Users", Logger: slog.Default()}
	if err := c.CheckUntrusted(ctx, 9); err == nil {
		t.Error("started without knowing")
	}
}

// Nor once someone adds it, or a group it is in, while it runs (sixth
// audit), as soon as the next check, which reads the group afresh, not
// from the 5-minute answer IsTrusted keeps (seventh audit). A group it
// cannot read waits, for maxUnread checks.
func TestBridgeStopsOnceTrusted(t *testing.T) {
	f := &checksFakes{group: []gerrit.AccountInfo{{AccountID: 5}}}
	c := &Checks{Gerrit: f, TrustedGroup: "Verified Users", Logger: slog.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.WatchUntrusted(ctx, 9, 5*time.Millisecond); err != nil {
		t.Errorf("an untrusted bridge stopped: %v", err)
	}
	f.group = append(f.group, gerrit.AccountInfo{AccountID: 9})
	if trusted, _ := c.IsTrusted(ctx, 9); trusted {
		t.Fatal("precondition: IsTrusted's answer is the cached one")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WatchUntrusted(ctx, 9, 5*time.Millisecond); err == nil || ctx.Err() != nil {
		t.Errorf("a bridge made trusted kept running (err %v)", err)
	}
	// A group it cannot read for now only waits; for good, it stops.
	c = &Checks{Gerrit: failingGroups{f}, TrustedGroup: "Verified Users", Logger: slog.Default()}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.WatchUntrusted(ctx, 9, 5*time.Millisecond); err != nil {
		t.Errorf("stopped on a group it could not read for a moment: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WatchUntrusted(ctx, 9, 5*time.Millisecond); err == nil || ctx.Err() != nil {
		t.Errorf("kept running on a group it could not read %d times (err %v)", maxUnread+1, err)
	}
}

type failingGroups struct{ *checksFakes }

func (failingGroups) GroupMembers(context.Context, string) ([]gerrit.AccountInfo, error) {
	return nil, errors.New("no such group")
}

// The watch goes on after the trusted group could not be read for a
// while and then could, without the bridge in it: it ended there, and the
// bridge ran unwatched (eighth audit).
func TestWatchGoesOnAfterAnOutage(t *testing.T) {
	g := &flakyGroup{fail: maxUnread + 1, members: []gerrit.AccountInfo{{AccountID: 5}}}
	c := &Checks{Gerrit: g, TrustedGroup: "Verified Users", Logger: slog.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WatchUntrusted(ctx, 9, 5*time.Millisecond) }()
	select {
	case err := <-done:
		if err == nil && ctx.Err() == nil {
			t.Errorf("WatchUntrusted returned nil after the group came back: the bridge is no longer watched")
		}
		return
	case <-time.After(500 * time.Millisecond):
	}
	// Still watching: the bridge joins the group.
	g.mu.Lock()
	g.members = append(g.members, gerrit.AccountInfo{AccountID: 9})
	g.mu.Unlock()
	if err := <-done; err == nil {
		t.Errorf("bridge joined the trusted group and was not stopped")
	}
}

type flakyGroup struct {
	checksFakes
	mu      sync.Mutex
	fail    int
	calls   int
	members []gerrit.AccountInfo
}

func (f *flakyGroup) GroupMembers(context.Context, string) ([]gerrit.AccountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fail {
		return nil, errors.New("503")
	}
	return slices.Clone(f.members), nil
}

// Anyone can set their full name. One that reads as another vote
// ("x, +2 Austin Schuh (AustinSchuh)") must not forge a voter in the
// status: votes are listed by username, which Gerrit restricts.
func TestStatusNamesCannotForgeAVoter(t *testing.T) {
	jr := gerrit.AccountInfo{AccountID: 1, Name: "Junior Dev", Username: "jr"}
	mallory := gerrit.AccountInfo{AccountID: 2, Name: "x, +2 Austin Schuh (AustinSchuh)", Username: "mallory"}
	ch := change([]gerrit.ApprovalInfo{{AccountInfo: jr, Value: 2}, {AccountInfo: mallory, Value: 1}}, nil)
	s := byContext(desiredStatuses(ch, "u", nil, ciRun{}, true, ""))[statusPrefix+"Code-Review"]
	if s.Description != "+2 jr, +1 mallory" {
		t.Errorf("description = %q", s.Description)
	}
}
