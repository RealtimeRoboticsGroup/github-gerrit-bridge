package reconcile

import (
	"context"
	"log/slog"
	"slices"
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
func (f *checksFakes) CreateStatus(_ context.Context, _ string, st github.Status) error {
	f.set = append(f.set, st.Context)
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
	// A newer upload is its uploader's doing.
	if who := statusCause(f.ch, gerrit.RevisionInfo{Created: "2026-10-19 13:00:00.000000000", Uploader: mallory}, "gerrit/Code-Review"); who.AccountID != 9 {
		t.Errorf("upload's status caused by %+v", who)
	}
}
