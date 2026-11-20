package reconcile

import (
	"context"
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
