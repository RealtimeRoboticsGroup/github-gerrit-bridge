package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// followSites is sites whose main is the same commit on both sides, and a
// Code where GitHub owns the branches.
func followSites(t *testing.T) (*sites, *Code, string) {
	t.Helper()
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, _ := s.code(ctx)
	c.GitHubOwned = true
	return s, c, base
}

// Design §3.1: what merges on GitHub reaches Gerrit's branch.
func TestFollowCopiesGitHubsBranch(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	merged := s.commit(base, "b", "b\n", "Squashed on GitHub (#7)\n\nChange-Id: I0000000000000000000000000000000000000001")
	s.push("github.git", merged, "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != merged {
		t.Errorf("Gerrit's main = %s, want GitHub's %s", got, merged)
	}
	// In sync: nothing more to do.
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
}

// The owner's decision: a force-push on GitHub is followed, not refused.
func TestFollowForcePush(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	ahead := s.commit(base, "b", "b\n", "On both")
	s.push("github.git", ahead, "refs/heads/main")
	s.push("gerrit.git", ahead, "refs/heads/main")
	rewritten := s.commit(base, "c", "c\n", "Force-pushed over it")
	s.push("github.git", rewritten, "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != rewritten {
		t.Errorf("Gerrit's main = %s, want GitHub's force-pushed %s", got, rewritten)
	}
}

// Gerrit's branches are its own where Gerrit owns them.
func TestFollowOnlyWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.GitHubOwned = false
	s.push("github.git", s.commit(base, "b", "b\n", "On GitHub"), "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != base {
		t.Errorf("Gerrit's main = %s, want it left at %s", got, base)
	}
}

// Only branches both sides have: none is made or removed in Gerrit, and
// the bridge's own GitHub branches are never target branches.
func TestFollowOnlyBranchesBothHave(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	other := s.commit(base, "b", "b\n", "Elsewhere")
	s.push("github.git", other, "refs/heads/feature")
	s.push("gerrit.git", other, "refs/heads/release")
	s.push("gerrit.git", base, "refs/heads/gerrit/x")
	s.push("github.git", other, "refs/heads/gerrit/x")
	for _, b := range []string{"feature", "release", "gerrit/x"} {
		if err := c.Follow(ctx, b); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	if got := s.gitOut("gerrit.git", "rev-parse", "--verify", "--quiet", "refs/heads/feature"); got != "" {
		t.Errorf("GitHub's feature branch was made in Gerrit: %s", got)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != other {
		t.Errorf("Gerrit's release = %s, want it kept (GitHub has none)", got)
	}
	if got := s.ref("gerrit.git", "refs/heads/gerrit/x"); got != base {
		t.Errorf("a bridge branch was followed: %s", got)
	}
}

func TestFollowDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.DryRun = true
	s.push("github.git", s.commit(base, "b", "b\n", "On GitHub"), "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != base {
		t.Errorf("dry run moved Gerrit's main to %s", got)
	}
}

// A branch's reconcile follows GitHub first, so what it plans is from
// what merged there.
func TestReconcileFollowsGitHubFirst(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	merged := s.commit(base, "b", "b\n", "Merged on GitHub")
	s.push("github.git", merged, "refs/heads/main")
	if err := c.Reconcile(ctx, Key{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != merged {
		t.Errorf("Gerrit's main = %s, want GitHub's %s", got, merged)
	}
}

// Nothing replicates a Gerrit merge to GitHub where GitHub owns the
// branch, so no PR is reshaped for one.
func TestPrepareSubmitLeavesPRsWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	store := c.Store
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	g1 := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: I1\n")
	h1 := s.commit(g1, "one.txt", "1b\n", "Patchset 2")
	g1b := s.commit(base, "one.txt", "1b\n", "One\n\nChange-Id: I1\n")
	ci := s.change(1, "I1", g1b, "One\n\nChange-Id: I1\n", "NEW")
	ci.Revisions[g1b] = gerritRev(ci.Revisions[g1b], 2)
	s.push("gerrit.git", g1b, ci.Revisions[g1b].Ref)
	s.together[1] = []int{1}
	s.push("github.git", h1, "refs/heads/gerrit/I1")
	pr := s.pr(10, "gerrit/I1", h1, "gerrit-base/x", "bot")
	pr.Stack = &github.PRStack{Number: 30}
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/I1", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 2, GerritSHA: g1b, GitHubSHA: h1}}})
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/I1"); got != h1 {
		t.Errorf("gerrit/I1 = %s, want its history head %s left", got, h1)
	}
	if len(s.unstacks) != 0 || s.pulls[10].Base.Ref != "gerrit-base/x" {
		t.Errorf("unstacked %v, base %s: want the PR left as it is", s.unstacks, s.pulls[10].Base.Ref)
	}
}

// The plan learns who owns the branch from what the reconcile observed.
func TestObserveSaysGitHubOwns(t *testing.T) {
	ctx := context.Background()
	_, c, _ := followSites(t)
	for _, owned := range []bool{true, false} {
		c.GitHubOwned = owned
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if snap.GitHubOwned != owned {
			t.Errorf("GitHubOwned %v: snapshot says %v", owned, snap.GitHubOwned)
		}
	}
}

// mergedSites is followSites with change 1 open in Gerrit, and its PR #10
// merged on GitHub as merged, a commit whose message is msg.
func mergedSites(t *testing.T, msg string) (*sites, *Code, string) {
	t.Helper()
	ctx := context.Background()
	s, c, base := followSites(t)
	rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	s.may[1] = []string{"bridge"}
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
		HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}})
	merged := s.commit(base, "one.txt", "1\n", msg)
	s.push("github.git", merged, "refs/heads/main")
	pr := s.pr(10, "gerrit/"+cid(1), rev, "main", "bot")
	pr.State, pr.Merged, pr.MergeCommitSHA, pr.MergedBy = "closed", true, merged, &github.User{Login: "alice"}
	return s, c, merged
}

// mirrorStates follows GitHub's main, observes it and mirrors the PRs'
// states, as a reconcile's first pass does.
func mirrorStates(t *testing.T, c *Code, follow bool) {
	t.Helper()
	ctx := context.Background()
	if follow {
		if err := c.Follow(ctx, "main"); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.mirrorPRStates(ctx, snap, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
}

// Design §3.1: a PR merged on GitHub whose commit Gerrit did not take as
// the change's (its Change-Id edited out, or co-author lines after it)
// leaves the change open in Gerrit: the bridge marks it merged at that
// commit (Gerrit's check-and-fix), saying where it merged.
func TestMergedOnGitHubIsMarkedMerged(t *testing.T) {
	s, c, merged := mergedSites(t, "One (#10)\n\nChange-Id: "+cid(1)+"\n\n---------\n\nCo-authored-by: Bob <bob@example.com>\n")
	s.maintainServer = true
	mirrorStates(t, c, true)
	if ci := s.changes[1]; ci.Status != "MERGED" || ci.CurrentRevision != merged {
		t.Fatalf("change 1 is %s at %s, want MERGED at %s", ci.Status, ci.CurrentRevision, merged)
	}
	want := "Merged on GitHub by @\u200dalice in PR #10, as " + merged + " on main."
	if msgs := s.reviewMsgs[1]; len(msgs) == 0 || msgs[len(msgs)-1] != want {
		t.Errorf("notices %q, want %q", msgs, want)
	}
}

// Without Maintain Server, Gerrit will not: the change is abandoned, the
// note saying where it merged.
func TestMergedOnGitHubIsAbandonedIfGerritWillNot(t *testing.T) {
	s, c, merged := mergedSites(t, "One (#10)\n\nThe trailer was edited out.\n")
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "ABANDONED" {
		t.Fatalf("change 1 is %s, want ABANDONED", got)
	}
	want := "Merged on GitHub by @\u200dalice in PR #10, as " + merged + " on main, but Gerrit could not show it merged."
	if !strings.HasPrefix(s.actionMsgs[1], want) {
		t.Errorf("message %q, want it to start %q", s.actionMsgs[1], want)
	}
}

// A PR merged into another branch (a stacked PR into the one below) does
// not merge as its change: abandoned, saying where it went, at once.
//
// Not where Gerrit owns the branches: anyone with write access may press
// merge on a bridge branch's PR, and that is no say-so to abandon a
// Gerrit change (thirteenth review). There it stays open, and a notice
// says so, once. Nor when GitHub says the bridge merged it: its own push
// (a reordered chain) did, not anyone.
func TestMergedOnGitHubIntoAnotherBranchIsAbandoned(t *testing.T) {
	for _, tc := range []struct {
		owned     bool
		by        string
		abandoned bool
	}{{true, "alice", true}, {false, "alice", false}, {true, "bot", false}, {false, "bot", false}} {
		s, c, _ := mergedSites(t, "One (#10)\n")
		c.GitHubOwned = tc.owned
		s.maintainServer = true
		s.pulls[10].Base.Ref = "gerrit/I2"
		s.pulls[10].MergedBy = &github.User{Login: tc.by}
		for range 2 {
			mirrorStates(t, c, false)
		}
		got := s.changes[1].Status
		if (got == "ABANDONED") != tc.abandoned || (tc.abandoned && !strings.Contains(s.actionMsgs[1], "PR #10 into gerrit/I2, not main")) {
			t.Errorf("%+v: change 1 is %s: %q", tc, got, s.actionMsgs[1])
		}
		notices := 0
		for _, body := range s.comments[10] {
			if strings.Contains(body, "Gerrit owns this repository's branches") {
				notices++
			}
		}
		for _, body := range s.comments[10] {
			if strings.Contains(body, "by the bridge's own push") {
				notices++
			}
		}
		// Said once: a stranger's merge where Gerrit owns the branches,
		// or the bridge's own push either way.
		if want := map[bool]int{true: 1}[(!tc.owned && tc.by != "bot") || tc.by == "bot"]; notices != want {
			t.Errorf("%+v: %d notices, want %d", tc, notices, want)
		}
	}
}

// Not before the merge is on Gerrit's branch (Gerrit may yet close it as
// merged), nor where Gerrit owns the branch.
func TestMergedOnGitHubWaitsForItsMerge(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("not followed yet: change 1 is %s", got)
	}
	if err := c.Follow(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	c.GitHubOwned = false
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("Gerrit owns main: change 1 is %s", got)
	}
	c.GitHubOwned = true
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "ABANDONED" {
		t.Errorf("merged and followed: change 1 is %s", got)
	}
}

// GitHub's word for the merge commit reaches git only as a full SHA: a
// ref name there would name whatever it names.
func TestMergedOnGitHubNeedsAMergeCommitSHA(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	s.pulls[10].MergeCommitSHA = "refs/remotes/gerrit/heads/main"
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("change 1 is %s, abandoned for a merge commit named by ref", got)
	}
}

// A reconcile abandons it and then plans from what Gerrit says now: the
// merged PR is left alone, not reopened for a change it thought open.
func TestMergedOnGitHubReconcile(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	if err := c.Reconcile(context.Background(), Key{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got, pr := s.changes[1].Status, s.pulls[10]; got != "ABANDONED" || pr.State != "closed" || !pr.Merged {
		t.Errorf("change 1 is %s, PR #10 %s merged %v; want abandoned, and the PR merged", got, pr.State, pr.Merged)
	}
}

// Where GitHub owns the branches, who may have Gerrit patch sets pushed to
// a GitHub-born PR's branch is who may push to the repository themselves
// (Code.Publishable), not CI's trusted group: the importer agrees with
// markForeign, and never restores (Gerrit wins) the code of a trusted
// member whose GitHub account may not push.
func TestImporterForeignIsByPublishable(t *testing.T) {
	for _, tc := range []struct {
		publishable, owner bool
		restored           bool
	}{{false, false, false}, {true, false, true}, {false, true, false}} {
		publishable := tc.publishable
		s, im, up, m1, g, ids := gerritBornWorld(t, false)
		ctx := context.Background()
		im.Trusted = func(_ context.Context, account int) (bool, bool) { return account == 9, true }
		im.GitHubOwned = true
		im.Publishable = func(_ context.Context, account int) (bool, bool) { return publishable && account == 9, true }
		uploader := gerrit.AccountInfo{AccountID: 9, Username: "trusted"}
		if tc.owner {
			// The owner's own: no pass on the repository's own branch.
			uploader = s.accounts["austin"]
		}
		st, _ := im.Store.Load(ctx, 1)
		st.State.Origin, st.State.HeadBranch = "github", "feature"
		if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
			t.Fatal(err)
		}
		ci := s.changes[1]
		ci.Owner = s.accounts["austin"]
		first := ci.Revisions[g[0]]
		first.Kind, first.Uploader = "REWORK", s.accounts["austin"]
		ci.Revisions[g[0]] = first
		code := s.commit(g[0], "one.txt", "theirs\n", "Their message\n\nChange-Id: "+ids[0]+"\n")
		s.push("gerrit.git", code, "refs/changes/01/1/2")
		ci.Revisions[code] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2",
			Uploader: uploader, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, code))}
		ci.CurrentRevision = code
		s.pulls[10].Head.Ref, s.pulls[10].User.Login = "feature", "austin"
		h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
		s.push("github.git", h, "refs/heads/feature")
		s.pulls[10].Head.SHA = h
		s.webhookPush(im.Pushers, 10, "austin")
		if err := im.Import(ctx, "main"); err != nil {
			t.Fatal(err)
		}
		restored := false
		for _, u := range up.uploads {
			if c := strings.Fields(u)[0]; strings.HasPrefix(u, " ") && s.git("m.git", "rev-parse", c+"^{tree}") == s.git("work", "rev-parse", code+"^{tree}") {
				restored = true
			}
		}
		if restored != tc.restored {
			t.Errorf("%+v: their code restored %v (uploads %v)", tc, restored, up.uploads)
		}
	}
}

// markForeign asks the same where GitHub owns the branches, and nobody
// is publishable without a way to tell.
func TestForeignIsByPublishableWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	nobody := func(context.Context, int) (bool, bool) { return false, true }
	for _, tc := range []struct {
		owned       bool
		publishable func(context.Context, int) (bool, bool)
		uploader    int    // 5 is the owner
		head        string // the PR's
		foreign     bool
		// recorded: the Gerrit commit recorded as one GitHub has (r1 is
		// the current patch set, imported from the PR).
		recorded string
	}{
		{false, nil, 7, "feature", false, ""}, // CI's trusted group decides
		{true, nobody, 7, "feature", true, ""},
		{true, func(context.Context, int) (bool, bool) { return true, true }, 7, "feature", false, ""},
		{true, nil, 7, "feature", true, ""},
		// The owner's own upload: no pass on the repository's branch
		// (they may have lost write access); on their fork's, theirs.
		{false, nil, 5, "feature", false, ""},
		{true, nobody, 5, "feature", true, ""},
		{true, nobody, 5, "someone/r:feature", false, ""},
		// Imported from the PR itself, by an unlinked author: GitHub has
		// it, so the PR still gets its description and Change-Id.
		{true, nobody, 5, "feature", false, "r1"},
		{true, nobody, 7, "feature", false, "r1"},
		{false, nil, 9, "feature", false, "r1"},  // both directions (thirteenth review)
		{true, nobody, 7, "feature", true, "r0"}, // an older patch set's, not this one
	} {
		s := newSites(t)
		c, _ := s.code(ctx)
		c.Below = noneBelow{} // made-up commits
		c.DryRun = true       // no notices
		c.GitHubOwned, c.Publishable = tc.owned, tc.publishable
		c.Trusted = func(_ context.Context, account int) (bool, bool) { return account == 7, true }
		snap := &Snapshot{Changes: map[int]*GerritChange{1: {Number: 1, Status: "NEW", Revision: "r1", PatchSet: 1,
			OwnerID: 5, UploaderID: tc.uploader, Kind: "REWORK"}},
			State: map[int]*state.Change{1: {State: state.ChangeState{Change: 1, Origin: "github", HeadBranch: tc.head}}}}
		if tc.recorded != "" {
			snap.State[1].Revisions = []state.Revision{{PatchSet: 1, GerritSHA: tc.recorded, GitHubSHA: "g1"}}
		}
		if err := c.markForeign(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[1] != tc.foreign {
			t.Errorf("%+v: foreign %v, want %v", tc, snap.Foreign[1], tc.foreign)
		}
	}
}

// A stranger's patch set someone pushed to the PR's branch is on GitHub,
// but no import or push of the bridge's: still foreign, so the PR keeps
// its author's description (twelfth review).
func TestFoundOnGitHubIsStillForeign(t *testing.T) {
	ctx := context.Background()
	for _, found := range []bool{false, true} {
		s := newSites(t)
		c, _ := s.code(ctx)
		c.Below = noneBelow{} // made-up commits
		c.DryRun = true
		c.GitHubOwned = true
		c.Publishable = func(context.Context, int) (bool, bool) { return false, true }
		snap := &Snapshot{Changes: map[int]*GerritChange{1: {Number: 1, Status: "NEW", Revision: "r1", PatchSet: 2,
			OwnerID: 5, UploaderID: 7, Kind: "REWORK"}},
			State: map[int]*state.Change{1: {State: state.ChangeState{Change: 1, Origin: "github", HeadBranch: "feature"},
				Revisions: []state.Revision{{PatchSet: 2, GerritSHA: "r1", GitHubSHA: "r1", Found: found}}}}}
		if err := c.markForeign(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[1] != found {
			t.Errorf("found %v: foreign %v", found, snap.Foreign[1])
		}
	}
}

// Design §3.1: where GitHub owns the branches, a Gerrit-born change
// reaches GitHub only if whoever uploaded it, and made its code, has a
// linked GitHub account that may push there: a pushed branch runs the
// repository's workflows with its secrets. The rest are withheld, with a
// notice once per patch set saying how to fix it.
func TestGerritBornNeedsPublishableUploaders(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, owned bool, publishable func(context.Context, int) (bool, bool)) (*sites, *Code, *gerrit.ChangeInfo) {
		s, c, base := followSites(t)
		c.GitHubOwned, c.Publishable, c.LinkURL = owned, publishable, "https://bridge.example.com/github/link"
		rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
		ci := s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
		r := ci.Revisions[rev]
		// Its owner too: owning the change is no pass.
		r.Uploader = gerrit.AccountInfo{AccountID: 7, Username: "seven"}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		return s, c, ci
	}
	only := func(who int, known bool) func(context.Context, int) (bool, bool) {
		return func(_ context.Context, a int) (bool, bool) { return a == who, known }
	}
	withheld := func(t *testing.T, c *Code) bool {
		t.Helper()
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Withheld[1]
	}

	s, c, _ := setup(t, true, only(8, true))
	if !withheld(t, c) || !withheld(t, c) {
		t.Fatal("an unlinked uploader's change is not withheld")
	}
	if msgs := s.reviewMsgs[1]; len(msgs) != 1 || !strings.Contains(msgs[0], "Link yours at https://bridge.example.com/github/link") {
		t.Errorf("notices %q, want one saying where to link", msgs)
	}
	if _, c, _ := setup(t, true, only(7, true)); withheld(t, c) {
		t.Error("a publishable uploader's change is withheld")
	}
	if _, c, _ := setup(t, false, only(8, true)); withheld(t, c) {
		t.Error("withheld where Gerrit owns the branches")
	}
	if s, c, _ := setup(t, true, only(7, false)); !withheld(t, c) || len(s.reviewMsgs[1]) != 0 {
		t.Errorf("cannot tell: withheld %v, notices %q; want withheld quietly", withheld(t, c), s.reviewMsgs[1])
	}
	if _, c, _ := setup(t, true, nil); !withheld(t, c) {
		t.Error("no way to tell who may push, and not withheld")
	}

	// Someone publishable editing the message of an unlinked upload: the
	// code is still the unlinked one's.
	s, c, ci := setup(t, true, only(8, true))
	first := ci.Revisions[ci.CurrentRevision]
	first.Kind = "REWORK"
	ci.Revisions[ci.CurrentRevision] = first
	edit := s.commit(strings.TrimSpace(s.gitOut("work", "rev-parse", ci.CurrentRevision+"^")), "one.txt", "1\n", "One, better\n\nChange-Id: "+cid(1)+"\n")
	s.push("gerrit.git", edit, "refs/changes/01/1/2")
	ci.Revisions[edit] = gerrit.RevisionInfo{Number: 2, Kind: "NO_CODE_CHANGE", Ref: "refs/changes/01/1/2",
		Uploader: gerrit.AccountInfo{AccountID: 8, Username: "eight"}, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, edit))}
	ci.CurrentRevision = edit
	if !withheld(t, c) {
		t.Error("a publishable member's message edit of an unlinked upload is not withheld")
	}
}

// Where GitHub owns the branches, a stranger's withheld patch set of a
// Gerrit-born change is not restored over a push to its PR ("Gerrit
// wins"): restored, it would be the bridge's upload, which the gate lets
// through to GitHub. A publishable uploader's is restored, as before.
func TestGerritWinsNeverRestoresUnpublishedCode(t *testing.T) {
	// noPub: GitHub-owned, but no Publishable: nobody, never CI's trusted
	// group (thirteenth review: the importer took it for Gerrit-owned).
	// unsure: GitHub could not say whether the uploader may push: not
	// restored (fourteenth review: ok without known passed).
	for _, tc := range []struct{ publishable, owner, noPub, unsure bool }{{false, false, false, false}, {true, false, false, false},
		{false, true, false, false}, {true, false, true, false}, {true, false, false, true}} {
		publishable := tc.publishable
		s, im, up, m1, g, ids := gerritBornWorld(t, false)
		ctx := context.Background()
		im.GitHubOwned = true
		im.Publishable = func(_ context.Context, account int) (bool, bool) {
			return account == 5 || (publishable && account == 9), true
		}
		if tc.noPub {
			im.Publishable = nil
			im.Trusted = func(context.Context, int) (bool, bool) { return true, true }
			publishable = false
		}
		if tc.unsure {
			im.Publishable = func(_ context.Context, account int) (bool, bool) { return account == 5 || account == 9, account != 9 }
			publishable = false
		}
		ci := s.changes[1]
		ci.Owner = s.accounts["austin"]
		if tc.owner { // owning the change is no pass
			ci.Owner = gerrit.AccountInfo{AccountID: 9, Username: "stranger"}
		}
		first := ci.Revisions[g[0]]
		first.Kind = "REWORK"
		ci.Revisions[g[0]] = first
		code := s.commit(g[0], "one.txt", "theirs\n", "Their message\n\nChange-Id: "+ids[0]+"\n")
		s.push("gerrit.git", code, "refs/changes/01/1/2")
		ci.Revisions[code] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2",
			Uploader: gerrit.AccountInfo{AccountID: 9, Username: "stranger"}, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, code))}
		ci.CurrentRevision = code
		// Someone with write access pushes to the PR's branch meanwhile.
		h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
		s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
		s.pulls[10].Head.SHA = h
		s.webhookPush(im.Pushers, 10, "austin")
		if err := im.Import(ctx, "main"); err != nil {
			t.Fatal(err)
		}
		restored := false
		for _, u := range up.uploads {
			if c := strings.Fields(u)[0]; strings.HasPrefix(u, " ") && s.git("m.git", "rev-parse", c+"^{tree}") == s.git("work", "rev-parse", code+"^{tree}") {
				restored = true
			}
		}
		if restored != publishable {
			t.Errorf("stranger publishable %v, owner %v: their code restored %v (uploads %v)", publishable, tc.owner, restored, up.uploads)
		}
	}
}

// Gerrit refusing GitHub's branch (a commit validator, say) does not stop
// the rest of the branch: changes are still mirrored, and the reconcile
// fails, to be tried again.
func TestRefusedFollowStillMirrorsTheBranch(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(context.Context, int) (bool, bool) { return true, true }
	hook := filepath.Join(s.dir, "gerrit.git", "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do [ \"$ref\" = refs/heads/main ] && { echo 'missing Signed-off-by' >&2; exit 1; }; done\nexit 0\n"), 0o755)
	s.push("github.git", s.commit(base, "b", "b\n", "Merged on GitHub"), "refs/heads/main")
	rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 7, Username: "seven"}
	r.Commit.Parents = []struct {
		Commit string `json:"commit"`
	}{{base}}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	if err := c.Reconcile(ctx, Key{Branch: "main"}); err == nil || !strings.Contains(err.Error(), "following GitHub's main") {
		t.Errorf("err = %v, want the refused follow", err)
	}
	if st, _ := c.Store.Load(ctx, 1); st.State.PR == 0 {
		t.Error("the change got no PR: the refused follow stopped the branch")
	}
}

// Only the default branch follows a force-push. Anyone with write access
// can make a branch on GitHub: one named like a branch Gerrit has (an
// old release) does not replace it; once it builds on Gerrit's, it is
// followed.
func TestFollowOtherBranchesOnlyForward(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	release := s.commit(base, "r", "r\n", "Release")
	s.push("gerrit.git", release, "refs/heads/release")
	other := s.commit(base, "x", "x\n", "Someone's branch")
	s.push("github.git", other, "refs/heads/release")
	if err := c.Follow(ctx, "release"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != release {
		t.Errorf("Gerrit's release = %s, replaced by an unrelated GitHub branch", got)
	}
	forward := s.commit(release, "r2", "r2\n", "Release fix")
	s.push("github.git", forward, "refs/heads/release")
	if err := c.Follow(ctx, "release"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != forward {
		t.Errorf("Gerrit's release = %s, want GitHub's %s", got, forward)
	}
}

// A reconcile's look at the branch does it: a publishable member's change
// on top of an unlinked one's is withheld with it.
func TestObserveWithholdsChildrenOfWithheld(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	add := func(n int, parent string, who int) string {
		rev := s.commit(parent, fmt.Sprint(n), "x\n", fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)))
		ci := s.change(n, cid(n), rev, fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)), "NEW")
		r := ci.Revisions[rev]
		r.Uploader = gerrit.AccountInfo{AccountID: who}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		return rev
	}
	// A chain on an unlinked user's change, two deep; a linked writer's
	// change on the branch, and one on it.
	add(3, add(2, add(1, base, 7), 8), 8)
	add(5, add(4, base, 8), 8)
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]bool{1: true, 2: true, 3: true}; !reflect.DeepEqual(snap.Withheld, want) {
		t.Errorf("withheld %v, want %v", snap.Withheld, want)
	}
	// Said once per patch set.
	if msgs := s.reviewMsgs[3]; len(msgs) != 1 || !strings.Contains(msgs[0], "has linked a GitHub account") || strings.Contains(msgs[0], "fork") {
		t.Errorf("change 3's notices %q", msgs)
	}
	if _, err := c.observe(ctx, "main", nil); err != nil {
		t.Fatal(err)
	}
	if msgs := s.reviewMsgs[3]; len(msgs) != 1 {
		t.Errorf("notice again: %q", msgs)
	}
}

// The copy is leased on what Gerrit had: moved meanwhile, it is refused,
// and Gerrit's ref is left as it is.
func TestCopyRefIsLeased(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	moved := s.commit(base, "m", "m\n", "Moved meanwhile")
	s.push("gerrit.git", moved, "refs/heads/main")
	theirs := s.commit(base, "g", "g\n", "GitHub's")
	s.push("github.git", theirs, "refs/heads/main")
	s.git("m.git", "fetch", "-q", filepath.Join(s.dir, "github.git"), "+refs/heads/main:refs/remotes/github/heads/main")
	if err := c.copyRef(ctx, "refs/heads/main", base, theirs); err == nil {
		t.Error("copied over a ref that moved")
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != moved {
		t.Errorf("Gerrit's main = %s, want %s left", got, moved)
	}
}

// A dry run closes nothing.
func TestMergedOnGitHubDryRun(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	s.maintainServer = true
	if err := c.Follow(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	c.DryRun = true
	mirrorStates(t, c, false)
	s.pulls[10].Base.Ref = "gerrit/I2"
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("dry run: change 1 is %s", got)
	}
}

// Gerrit refusing to abandon (no Abandon grant) is logged, not a failure
// of the branch each time.
func TestMergedOnGitHubAbandonRefused(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	s.may[1] = nil
	s.pulls[10].Base.Ref = "gerrit/I2"
	mirrorStates(t, c, false) // fails the test on an error
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("change 1 is %s", got)
	}
}

// The linked-writer gate is for Gerrit-born changes: a GitHub-born one's
// Gerrit patch sets are markForeign's (their notice says so), not
// withheld as unlinked.
func TestGitHubBornIsNotWithheldAsUnlinked(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(context.Context, int) (bool, bool) { return false, true }
	rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: c.BridgeAccount + 7}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
		HeadBranch: "someone/r:feature", Lifecycle: "NEW"}})
	s.pr(10, "feature", rev, "main", "someone").Head.Repo.FullName = "someone/r"
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Withheld[1] {
		t.Error("a GitHub-born change withheld as unlinked")
	}
}

// A linked writer's change brings GitHub nothing nobody mirrored vouches
// for: on an unlinked user's outdated, abandoned or private patch set, or
// merging one in, it is withheld too (twelfth review).
func TestObserveWithholdsWhatNobodyVouchesFor(t *testing.T) {
	for _, how := range []string{"new patch set", "abandoned", "private", "merged in", "none", "on a mirrored one", "on an in-repo import", "on a fork's import", "on one recorded, not on GitHub", "on a writer's outdated one", "on the bridge's import"} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		add := func(n int, rev string, who int) *gerrit.ChangeInfo {
			msg := fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n))
			ci := s.change(n, cid(n), rev, msg, "NEW")
			r := ci.Revisions[rev]
			r.Uploader = gerrit.AccountInfo{AccountID: who}
			ci.Revisions[rev], ci.Owner = r, r.Uploader
			return ci
		}
		mal := s.commit(base, "m", "m\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
		if how == "none" {
			mal = base // the writer's change is on the branch
		} else if how == "on the bridge's import" {
			// An unlinked author's PR, imported as the bridge: theirs,
			// not the bridge's to vouch for.
			c.BridgeAccount = 1
			add(1, mal, 1)
		} else if how == "on a mirrored one" || how == "on a writer's outdated one" {
			add(1, mal, 8) // a linked writer's too
		} else {
			add(1, mal, 7)
		}
		var bob string
		if how == "merged in" {
			// On the branch, merging in mal: only mal can withhold it.
			tree := s.git("work", "rev-parse", mal+"^{tree}")
			bob = s.git("work", "commit-tree", tree, "-p", base, "-p", mal, "-m", fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)))
		} else {
			bob = s.commit(mal, "b", "b\n", fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)))
		}
		add(2, bob, 8)
		switch how {
		case "on an in-repo import", "on a fork's import", "on one recorded, not on GitHub":
			// Imported from an unlinked author's PR (GitHub has it, on
			// its branch), then outdated.
			gh := map[bool]string{true: "f00d"}[how != "on one recorded, not on GitHub"]
			head := map[bool]string{true: "someone/r:feature", false: "feature"}[how == "on a fork's import"]
			s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
				HeadBranch: head, Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: mal, GitHubSHA: gh}}})
			fallthrough
		case "new patch set", "on a writer's outdated one":
			// Patch set 2, the first still there, outdated.
			ci := s.changes[1]
			rev := s.commit(base, "m", "m2\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
			s.push("gerrit.git", rev, "refs/changes/01/1/2")
			ci.Revisions[rev] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2", Uploader: ci.Owner,
				Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, rev))}
			ci.CurrentRevision = rev
		case "abandoned":
			s.changes[1].Status = "ABANDONED"
		case "private":
			s.changes[1].IsPrivate = true
		}
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		// A linked writer's outdated patch set is vouched for; so is an
		// unlinked author's PR GitHub has on one of the repository's own
		// branches, where it ran with its secrets already (thirteenth
		// review); a fork's, whose code never did, is not.
		if snap.Withheld[2] != (how != "none" && how != "on a mirrored one" && how != "on a writer's outdated one" && how != "on an in-repo import") {
			t.Errorf("%s: withheld %v", how, snap.Withheld)
		}
	}
}

// The same rule, in both directions, for a Gerrit patch set pushed to a
// GitHub-born PR's branch: a trusted member's on a stranger's patch set
// is not pushed, for the code below it is the stranger's (twelfth
// review: as CI's parentsTrusted).
func TestForeignCodeBelowIsForeign(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, stranger := range []bool{false, true} {
			ctx := context.Background()
			s, c, base := followSites(t)
			c.GitHubOwned = owned
			c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
			c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
			below := 8
			if stranger {
				below = 9
			}
			low := s.commit(base, "low", "l\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
			ci := s.change(1, cid(1), low, fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)), "NEW")
			r := ci.Revisions[low]
			r.Uploader = gerrit.AccountInfo{AccountID: below}
			ci.Revisions[low], ci.Owner = r, r.Uploader
			top := s.commit(low, "top", "t\n", fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)))
			ci = s.change(2, cid(2), top, fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)), "NEW")
			r = ci.Revisions[top]
			r.Uploader = gerrit.AccountInfo{AccountID: 8}
			ci.Revisions[top], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
			s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
				HeadBranch: "feature", Lifecycle: "NEW"}})
			snap, err := c.observe(ctx, "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Foreign[2] != stranger {
				t.Errorf("GitHub owns %v, a stranger's below %v: foreign %v", owned, stranger, snap.Foreign[2])
			}
			// The notice names who qualifies in this direction.
			want := map[bool]string{false: "in CI's trusted group", true: "linked a GitHub account that may push to the repository (owning the change counts only on the PR author's own fork)"}[owned]
			if msgs := s.reviewMsgs[2]; stranger && (len(msgs) != 1 || !strings.Contains(msgs[0], want) ||
				!strings.Contains(msgs[0], "Here, not: change 1's patch set 1 below it")) {
				t.Errorf("GitHub owns %v: notices %q, want %q", owned, msgs, want)
			}
		}
	}
}

// chainReader is a belowReader of made-up commits: c<i>'s parent is
// c<i+1>, each a patch set of its own change by uploader, but merged
// (the commit it merged as) where merged says; base is in the branch.
type chainReader struct {
	n, uploader int
	merged      map[string]bool
	mergedAs    map[string]string // a merged change's commit, if not this one
}

func (r chainReader) parentsOf(context.Context, string) ([]string, error) { return nil, nil }
func (r chainReader) inBranch(_ context.Context, _, sha string) (bool, error) {
	return sha == fmt.Sprint("c", r.n), nil
}
func (r chainReader) onRepoBranch(context.Context, int, string, string) (bool, error) {
	return false, nil
}
func (r chainReader) changesOf(_ context.Context, sha string) ([]gerrit.ChangeInfo, error) {
	var i int
	fmt.Sscanf(sha, "c%d", &i)
	rv := gerrit.RevisionInfo{Number: 1, Kind: "REWORK", Uploader: gerrit.AccountInfo{AccountID: r.uploader}}
	rv.Commit.Parents = append(rv.Commit.Parents, struct {
		Commit string `json:"commit"`
	}{fmt.Sprint("c", i+1)})
	ci := gerrit.ChangeInfo{Number: i, Status: "NEW", CurrentRevision: sha, Revisions: map[string]gerrit.RevisionInfo{sha: rv}}
	if r.merged[sha] {
		ci.Status = "MERGED"
	}
	if r.mergedAs[sha] != "" {
		// Merged as another commit: this one is an older patch set.
		ci.Status, ci.CurrentRevision = "MERGED", r.mergedAs[sha]
	}
	return []gerrit.ChangeInfo{ci}, nil
}

// As gerrit-buildkite's: at most maxBelow commits below are read, and the
// commit a change merged as is trusted whoever uploaded it.
func TestParentsTrustedBounds(t *testing.T) {
	ctx := context.Background()
	trusted := func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	for _, tc := range []struct {
		n, uploader int
		merged      map[string]bool
		want        bool
	}{
		{maxBelow - 1, 8, nil, true}, // c0..c48 patch sets, c49 the branch: 50 read
		{maxBelow, 8, nil, false},    // one more than it reads
		{3, 9, nil, false},           // a stranger's
		{3, 9, map[string]bool{"c0": true, "c1": true, "c2": true}, true}, // merged as
	} {
		ok, _, _, err := parentsTrusted(ctx, chainReader{n: tc.n, uploader: tc.uploader, merged: tc.merged}, "main", "", []string{"c0"}, nil, trusted)
		if ok != tc.want || err != nil {
			t.Errorf("%+v: %v %v", tc, ok, err)
		}
	}
	// Who could not be told about (GitHub could not say whether they may
	// push) is not known: withheld, with nothing said (thirteenth review).
	unsure := func(_ context.Context, a int) (bool, bool) { return false, a != 9 }
	if ok, known, _, _ := parentsTrusted(ctx, chainReader{n: 3, uploader: 9}, "main", "", []string{"c0"}, nil, unsure); ok || known {
		t.Errorf("not known below: ok %v, known %v", ok, known)
	}
	// Only the commit it merged as: a stranger's older patch set of a
	// merged change is theirs still (thirteenth review).
	r := chainReader{n: 3, uploader: 9, mergedAs: map[string]string{"c0": "other", "c1": "other", "c2": "other"}}
	if ok, _, _, _ := parentsTrusted(ctx, r, "main", "", []string{"c0"}, nil, trusted); ok {
		t.Error("an older patch set of a merged change trusted as the merge")
	}
}

// Who may push is chosen by the mode, not by what is set: GitHub-owned
// with no Publishable is nobody, never CI's trusted group.
func TestPushTrustIsByMode(t *testing.T) {
	ctx := context.Background()
	yes := func(context.Context, int) (bool, bool) { return true, true }
	for _, tc := range []struct {
		owned                bool
		publishable, trusted func(context.Context, int) (bool, bool)
		want                 bool
	}{
		{false, nil, yes, true},
		{false, func(context.Context, int) (bool, bool) { return false, true }, yes, true}, // Gerrit-owned: the trusted group
		{true, nil, yes, false},
		{true, yes, nil, true},
	} {
		if ok, _ := pushTrust(tc.owned, tc.publishable, tc.trusted)(ctx, 9); ok != tc.want {
			t.Errorf("%+v: %v", tc, ok)
		}
	}
}

// The budgets' exemption is the mode's push rule, read when it is asked
// (Code's fields may be set after it is taken).
func TestPushTrustExported(t *testing.T) {
	ctx := context.Background()
	c := &Code{GitHubOwned: true}
	exempt := c.PushTrust()
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	c.Trusted = func(context.Context, int) (bool, bool) { return true, true }
	if ok, _ := exempt(ctx, 8); !ok {
		t.Error("a linked writer is not exempt")
	}
	if ok, _ := exempt(ctx, 9); ok {
		t.Error("CI's trusted group is exempt where GitHub owns the branches")
	}
}

// A patch set uploaded on its uploader's behalf by someone Gerrit does not
// name is nobody's, however trusted the uploader (gerrit-buildkite's
// RealUnknown), read from Gerrit or from a look at the branch.
func TestUnknownRealUploaderIsNobodys(t *testing.T) {
	ctx := context.Background()
	trusted := func(context.Context, int) (bool, bool) { return true, true }
	rv := gerrit.RevisionInfo{Number: 1, Kind: "REWORK", Uploader: gerrit.AccountInfo{AccountID: 8}, RealUploader: &gerrit.AccountInfo{}}
	rv.Commit.Parents = append(rv.Commit.Parents, struct {
		Commit string `json:"commit"`
	}{"p"})
	if ok, _ := authorsCode(ctx, trusted, 0, 0, map[int]gerrit.RevisionInfo{1: rv}, 1); ok {
		t.Error("trusted with an unknown real uploader")
	}
	rv.RealUploader = nil
	if ok, _ := authorsCode(ctx, trusted, 0, 0, map[int]gerrit.RevisionInfo{1: rv}, 1); !ok {
		t.Error("not trusted with no real uploader at all")
	}
	// The look's shortcut for a first patch set reads it instead.
	s, c, base := followSites(t)
	c.Publishable = trusted
	rev := s.commit(base, "x", "x\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
	ci := s.change(1, cid(1), rev, fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)), "NEW")
	r := ci.Revisions[rev]
	r.Uploader, r.RealUploader = gerrit.AccountInfo{AccountID: 8}, &gerrit.AccountInfo{}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Withheld[1] {
		t.Errorf("withheld %v", snap.Withheld)
	}
}

// countingReader counts what a belowReader is asked.
type countingReader struct {
	r                  belowReader
	in, changes, state int
}

func (c *countingReader) parentsOf(ctx context.Context, rev string) ([]string, error) {
	return c.r.parentsOf(ctx, rev)
}
func (c *countingReader) inBranch(ctx context.Context, b, sha string) (bool, error) {
	c.in++
	return c.r.inBranch(ctx, b, sha)
}
func (c *countingReader) changesOf(ctx context.Context, sha string) ([]gerrit.ChangeInfo, error) {
	c.changes++
	return c.r.changesOf(ctx, sha)
}
func (c *countingReader) onRepoBranch(ctx context.Context, n int, sha, into string) (bool, error) {
	c.state++
	return c.r.onRepoBranch(ctx, n, sha, into)
}

// A linked writer's chain costs a read per commit in one look, not a walk
// per change (thirteenth review: 53 changes asked Gerrit ~2,800 times);
// and a chain longer than the walk's bound, all vouched for, is not
// withheld once its lower changes are on GitHub.
func TestParentWalkCost(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	cr := &countingReader{r: c.belowReader()}
	c.Below = cr
	const n = 20
	parent := base
	for i := 1; i <= n; i++ {
		msg := fmt.Sprintf("C%d\n\nChange-Id: %s\n", i, cid(i))
		rev := s.commit(parent, fmt.Sprint(i), "x\n", msg)
		ci := s.change(i, cid(i), rev, msg, "NEW")
		r := ci.Revisions[rev]
		r.Uploader = gerrit.AccountInfo{AccountID: 8}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		parent = rev
	}
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Withheld) != 0 {
		t.Errorf("withheld %v", snap.Withheld)
	}
	if cr.in > n+1 || cr.changes > n {
		t.Errorf("%d changes: %d inBranch, %d changesOf in one look", n, cr.in, cr.changes)
	}
}

// What is not trusted below is named in the notice; a commit the bridge
// cannot see as anyone's patch set is not known, and named too.
func TestParentWalkSaysWhy(t *testing.T) {
	ctx := context.Background()
	r := chainReader{n: 3, uploader: 9}
	trusted := func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	ok, known, why, err := parentsTrusted(ctx, r, "main", "", []string{"c0"}, nil, trusted)
	if ok || !known || err != nil || !strings.Contains(why, "change 0's patch set 1 below it") {
		t.Errorf("a stranger's: %v %v %q %v", ok, known, why, err)
	}
	ok, known, why, err = parentsTrusted(ctx, chainReader{n: maxBelow + 5, uploader: 8}, "main", "", []string{"c0"}, nil, trusted)
	if ok || err != nil || !strings.Contains(why, "more than 50 commits") {
		t.Errorf("too deep: %v %v %q %v", ok, known, why, err)
	}
	ok, known, why, err = parentsTrusted(ctx, unseenReader{}, "main", "", []string{"c0"}, nil, trusted)
	if ok || known || err != nil || !strings.Contains(why, "nor a patch set the bridge can see") {
		t.Errorf("unseen: %v %v %q %v", ok, known, why, err)
	}
}

type unseenReader struct{ noneBelow }

func (unseenReader) inBranch(context.Context, string, string) (bool, error) { return false, nil }

// failingReader cannot read Gerrit.
type failingReader struct{ noneBelow }

func (failingReader) parentsOf(context.Context, string) ([]string, error) { return []string{"p"}, nil }
func (failingReader) inBranch(context.Context, string, string) (bool, error) {
	return false, errors.New("gerrit is down")
}

// The importer does not take Gerrit not answering for a no, which would
// let the push win over the patch set Gerrit should: the import fails, to
// be tried again (thirteenth review).
func TestImporterRetriesWhenTheWalkFails(t *testing.T) {
	for _, origin := range []string{"gerrit", "github"} {
		testImporterRetries(t, origin)
	}
}

func testImporterRetries(t *testing.T, origin string) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	if origin == "github" {
		// A GitHub-born PR, its branch the repository's own.
		st, _ := im.Store.Load(ctx, 1)
		st.State.Origin, st.State.HeadBranch = "github", "feature"
		if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
			t.Fatal(err)
		}
	}
	head := map[string]string{"gerrit": "gerrit/" + ids[0], "github": "feature"}[origin]
	im.GitHubOwned = true
	im.Publishable = func(context.Context, int) (bool, bool) { return true, true }
	im.Below = failingReader{}
	ci := s.changes[1]
	ci.Owner = s.accounts["austin"]
	first := ci.Revisions[g[0]]
	first.Kind = "REWORK"
	ci.Revisions[g[0]] = first
	// On the change's own first patch set, which GitHub has, and merging
	// in main: that second parent the walk asks Gerrit about.
	own := s.commit(g[0], "one.txt", "theirs\n", "Their message")
	code := s.git("work", "commit-tree", s.git("work", "rev-parse", own+"^{tree}"), "-p", g[0], "-p", m1, "-m", "Their message\n\nChange-Id: "+ids[0])
	s.push("gerrit.git", code, "refs/changes/01/1/2")
	ci.Revisions[code] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2",
		Uploader: gerrit.AccountInfo{AccountID: 9}, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, code))}
	ci.CurrentRevision = code
	h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
	s.push("github.git", h, "refs/heads/"+head)
	s.pulls[10].Head.Ref, s.pulls[10].Head.SHA, s.pulls[10].User.Login = head, h, "austin"
	s.webhookPush(im.Pushers, 10, "austin")
	// That PR's import fails (logged, the rest of the branch going on):
	// nothing is uploaded over Gerrit's patch set until it can be told.
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 0 {
		t.Errorf("%s-born: uploaded %v while Gerrit could not say whose code it is", origin, up.uploads)
	}
}

// Who may push below could not be told (GitHub did not answer): the
// change is withheld until it can be, and no notice blames anyone.
func TestObserveWithholdsUnknownBelowQuietly(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, a != 7 }
	add := func(n int, parent string, who int) string {
		rev := s.commit(parent, fmt.Sprint(n), "x\n", fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)))
		ci := s.change(n, cid(n), rev, fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)), "NEW")
		r := ci.Revisions[rev]
		r.Uploader = gerrit.AccountInfo{AccountID: who}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		return rev
	}
	add(2, add(1, base, 7), 8)
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Withheld[2] || len(s.reviewMsgs[2]) != 0 {
		t.Errorf("withheld %v, notices %q", snap.Withheld, s.reviewMsgs[2])
	}
}

// oddReader answers a commit query with a change that does not have the
// commit (Gerrit does not, but the walk must not take it as vouched for).
type oddReader struct{ noneBelow }

func (oddReader) inBranch(context.Context, string, string) (bool, error) { return false, nil }
func (oddReader) changesOf(context.Context, string) ([]gerrit.ChangeInfo, error) {
	return []gerrit.ChangeInfo{{Number: 1, Status: "NEW", CurrentRevision: "other"}}, nil
}

func TestParentsTrustedRefusesAnOddAnswer(t *testing.T) {
	trusted := func(context.Context, int) (bool, bool) { return true, true }
	if ok, _, _, _ := parentsTrusted(context.Background(), oddReader{}, "main", "", []string{"c0"}, nil, trusted); ok {
		t.Error("trusted a commit its change does not have")
	}
}

// A Gerrit commit found on the PR's branch (someone pushed it there) is
// not this change's own below a patch set: its code is checked like any
// other's (thirteenth review: only a test with no parents covered it).
func TestFoundBelowIsCheckedLikeAnyCode(t *testing.T) {
	for _, found := range []bool{false, true} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.GitHubOwned = false
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		low := s.commit(base, "low", "l\n", "C1\n\nChange-Id: "+cid(1)+"\n")
		ci := s.change(1, cid(1), low, "C1\n\nChange-Id: "+cid(1)+"\n", "NEW")
		r := ci.Revisions[low]
		r.Uploader = gerrit.AccountInfo{AccountID: 9} // a stranger's
		ci.Revisions[low], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
		top := s.commit(low, "top", "t\n", "C1\n\nChange-Id: "+cid(1)+"\n")
		s.push("gerrit.git", top, "refs/changes/01/1/2")
		ci.Revisions[top] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2", Uploader: gerrit.AccountInfo{AccountID: 8},
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, top))}
		ci.CurrentRevision = top
		// The stranger's patch set is on GitHub: imported (theirs to the
		// branch's history), or found there (pushed by someone).
		s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
			HeadBranch: "feature", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low, Found: found}}})
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[1] != found {
			t.Errorf("below it found on GitHub %v: foreign %v", found, snap.Foreign[1])
		}
	}
}

// The mirror's reader: a branch is the one asked about, not main; and a
// commit is a SHA, not a name for one (rev-list would take a ref).
func TestMirrorReader(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	rel := s.commit(base, "r", "r\n", "Release only")
	s.push("gerrit.git", rel, "refs/heads/release")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/gerrit/heads/*")
	r := mirrorReader{c.Gerrit, c.Mirror, c.Store, "p"}
	if in, err := r.inBranch(ctx, "release", rel); !in || err != nil {
		t.Errorf("release: %v %v", in, err)
	}
	if in, err := r.inBranch(ctx, "main", rel); in || err != nil {
		t.Errorf("main: %v %v", in, err)
	}
	if p, err := r.parentsOf(ctx, rel); err != nil || len(p) != 1 || p[0] != base {
		t.Errorf("parents %v %v", p, err)
	}
	if _, err := r.parentsOf(ctx, "refs/remotes/gerrit/heads/release"); err == nil {
		t.Error("took a ref name for a commit")
	}
}

// Where Gerrit owns the branches every registered user's change is pushed
// to a bridge branch, unvetted: recorded so, it is not vouched-for code
// below a trusted user's patch set (fourteenth review). Where GitHub owns
// them, the same push passed the rule, and is.
func TestUnvettedPushIsNotVouchedFor(t *testing.T) {
	for _, owned := range []bool{false, true} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.GitHubOwned = owned
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		c.Publishable = c.Trusted
		low := s.commit(base, "low", "l\n", "C1\n\nChange-Id: "+cid(1)+"\n")
		ci := s.change(1, cid(1), low, "C1\n\nChange-Id: "+cid(1)+"\n", "NEW")
		r := ci.Revisions[low]
		r.Uploader = gerrit.AccountInfo{AccountID: 9} // a stranger's Gerrit-born change
		ci.Revisions[low], ci.Owner = r, r.Uploader
		s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
			HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low, Vetted: owned}}})
		top := s.commit(low, "top", "t\n", "C2\n\nChange-Id: "+cid(2)+"\n")
		ci = s.change(2, cid(2), top, "C2\n\nChange-Id: "+cid(2)+"\n", "NEW")
		r = ci.Revisions[top]
		r.Uploader = gerrit.AccountInfo{AccountID: 8}
		ci.Revisions[top], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
		s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
			HeadBranch: "feature", Lifecycle: "NEW"}})
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[2] == owned {
			t.Errorf("GitHub owns %v: foreign %v", owned, snap.Foreign[2])
		}
	}
}

// A commit below that is neither on the branch nor a patch set the bridge
// can see is not known, so the change waits; but it is named, in both
// directions' notices, not left silent (fourteenth review).
func TestUnseenBelowIsNamed(t *testing.T) {
	for _, owned := range []bool{true, false} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.GitHubOwned = owned
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		c.Publishable = c.Trusted
		unseen := s.commit(base, "u", "u\n", "Nobody's change")
		top := s.commit(unseen, "top", "t\n", "C2\n\nChange-Id: "+cid(2)+"\n")
		ci := s.change(2, cid(2), top, "C2\n\nChange-Id: "+cid(2)+"\n", "NEW")
		r := ci.Revisions[top]
		r.Uploader = gerrit.AccountInfo{AccountID: 8}
		ci.Revisions[top], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
		if !owned {
			// Gerrit-owned: a GitHub-born change's Gerrit patch set.
			s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
				HeadBranch: "feature", Lifecycle: "NEW"}})
		}
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		held := snap.Withheld[2] || snap.Foreign[2]
		if msgs := s.reviewMsgs[2]; !held || len(msgs) != 1 || !strings.Contains(msgs[0], "neither on the branch nor a patch set the bridge can see") {
			t.Errorf("GitHub owns %v: held %v, notices %q", owned, held, msgs)
		}
	}
}
