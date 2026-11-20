package reconcile

import (
	"context"
	"strings"
	"testing"

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
	if err := c.follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != merged {
		t.Errorf("Gerrit's main = %s, want GitHub's %s", got, merged)
	}
	// In sync: nothing more to do.
	if err := c.follow(ctx, "main"); err != nil {
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
	if err := c.follow(ctx, "main"); err != nil {
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
	if err := c.follow(ctx, "main"); err != nil {
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
		if err := c.follow(ctx, b); err != nil {
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
	if err := c.follow(ctx, "main"); err != nil {
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
		if err := c.follow(ctx, "main"); err != nil {
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

// Design §3.1: a PR merged on GitHub whose commit lost its Change-Id
// (edited out in the merge dialog) leaves its change open in Gerrit: it
// is abandoned, saying where it merged.
func TestMergedOnGitHubWithoutChangeIDIsAbandoned(t *testing.T) {
	s, c, merged := mergedSites(t, "One (#10)\n\nThe trailer was edited out.\n")
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "ABANDONED" {
		t.Fatalf("change 1 is %s, want ABANDONED", got)
	}
	want := "Merged on GitHub by @‍alice in PR #10, as " + merged + " on main, but its commit does not say this change's Change-Id (" + cid(1) + ")"
	if !strings.HasPrefix(s.actionMsgs[1], want) {
		t.Errorf("message %q, want it to start %q", s.actionMsgs[1], want)
	}
}

// Said so when the commit has its Change-Id and Gerrit still did not
// close it (another branch's change of the same Change-Id, say).
func TestMergedOnGitHubWithChangeIDStillOpenIsAbandoned(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n\nChange-Id: "+cid(1)+"\n")
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "ABANDONED" || !strings.Contains(s.actionMsgs[1], "but Gerrit did not close it as merged") {
		t.Errorf("change 1 is %s, message %q", got, s.actionMsgs[1])
	}
}

// Not before the merge is on Gerrit's branch (Gerrit may yet close it as
// merged), nor where Gerrit owns the branch, nor for a merge into another
// PR's branch.
func TestMergedOnGitHubWaitsForItsMerge(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("not followed yet: change 1 is %s", got)
	}
	c.GitHubOwned = false
	if err := c.follow(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	c.GitHubOwned = true
	if err := c.follow(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	c.GitHubOwned = false
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("Gerrit owns main: change 1 is %s", got)
	}
	c.GitHubOwned = true
	s.pulls[10].Base.Ref = "gerrit/I2"
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("merged into another PR's branch: change 1 is %s", got)
	}
	s.pulls[10].Base.Ref = "main"
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
