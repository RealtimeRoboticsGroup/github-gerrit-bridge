package reconcile

// Gerrit-owned regressions: what the default direction (branch_owner:
// gerrit, as AOS runs it) does on main, kept so the GitHub-owned work can
// go on without changing it unseen (the owner's ask, 2026-11-21). Each
// test here passed on main (a01d401) and was broken by a change on the
// github-owner branch, found in review; add one for every such find.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Fourteenth review: a stranger's Gerrit-born change the bridge mirrored to its
// own gerrit/* branch (every change is, where Gerrit owns the branches)
// is still a stranger's below a GitHub-born change.
func TestGerritOwnedMirroredStrangerBelowIsStillAStrangers(t *testing.T) {
	for _, owned := range []bool{false, true} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.GitHubOwned = owned
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		low := s.commit(base, "low", "l\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
		ci := s.change(1, cid(1), low, fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)), "NEW")
		r := ci.Revisions[low]
		r.Uploader = gerrit.AccountInfo{AccountID: 9}
		ci.Revisions[low], ci.Owner = r, r.Uploader
		s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
			Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low}}})
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
		if !snap.Foreign[2] {
			t.Errorf("GitHub owns %v: a stranger's mirrored change below is not foreign", owned)
		}
	}
}

// Fourteenth review: where Gerrit owns the branches, a PR shown merged into the
// target branch is replication's doing (only it may write main): the PR
// gets no "merged on GitHub, abandon it in Gerrit" notice.
func TestGerritOwnedReplicationMergeGetsNoNotice(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	c.GitHubOwned = false
	s.pulls[10].MergedBy = &github.User{Login: "replicator"}
	mirrorStates(t, c, false)
	for _, body := range s.comments[10] {
		if strings.Contains(body, "Gerrit owns this repository's branches") || strings.Contains(body, "Abandon") {
			t.Errorf("notice on a replication merge: %q", body)
		}
	}
}

// Fourteenth review: where Gerrit owns the branches, an author's fork PR above
// their own other PR (imported by the bridge) may have Gerrit's squash
// swapped in: the code below is the author's own.
func TestGerritOwnedSwapOfAnAuthorsOwnForkStack(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 6, true }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	low := s.commit(m0, "low.txt", "l\n", "Low\n\nChange-Id: "+cid(2)+"\n")
	ci := s.change(2, cid(2), low, "Low\n\nChange-Id: "+cid(2)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	r := ci.Revisions[low]
	r.Uploader = gerrit.AccountInfo{AccountID: 1} // the bridge's import of alice's PR
	ci.Revisions[low] = r
	s.save(ctx, store, 2, &state.Change{State: state.ChangeState{Change: 2, PR: 9, Branch: "main", Origin: "github", HeadBranch: "alice/r:low", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low}}})
	sq := s.commit(low, "f.txt", "2\n", "Feature\n\nChange-Id: "+cid(1)+"\n")
	ci = s.change(1, cid(1), sq, "Feature\n\nChange-Id: "+cid(1)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	r = ci.Revisions[sq]
	r.Uploader = gerrit.AccountInfo{AccountID: 1}
	ci.Revisions[sq] = r
	st := &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "alice/r:feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: "1111111111111111111111111111111111111111"}}}
	full, err := c.Gerrit.GetChange(ctx, 1, "ALL_REVISIONS", "ALL_COMMITS", "DETAILED_ACCOUNTS")
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := full.Current()
	if err := c.maySwap(ctx, full, rev, st); err != nil {
		t.Errorf("swap refused: %v", err)
	}
	// Into another fork, the lower PR's code is not its own: refused.
	st.State.HeadBranch = "mallory/r:feature"
	if err := c.maySwap(ctx, full, rev, st); err == nil {
		t.Error("swapped another fork's import into a different fork")
	}
}
