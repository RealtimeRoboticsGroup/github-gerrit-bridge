package reconcile

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Where GitHub owns main, a stack's bottom squash-merged on GitHub: GitHub
// retargets the PR above to main and rebases its commits onto the squash
// itself. The change above still has the merged change's old patch set as
// its parent until that restack is imported; the bridge must leave the PR
// on main meanwhile, not pin it to a frozen base (whose base changes
// dismiss its approvals, and whose old commit conflicts with the squash).
func TestGitHubOwnedSquashedBottomLeavesTheNextOnMain(t *testing.T) {
	w := newWorld(t)
	w.githubOwned = true
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	// GitHub squashes PR 1 into s1 on main; Gerrit marks change 1 merged
	// at it; GitHub moves PR 2 onto main.
	pr1, pr2 := w.pr(1), w.pr(2)
	pr1.State, pr1.Merged = "closed", true
	w.gerrit[1].Status, w.gerrit[1].Revision, w.gerrit[1].Parent = "MERGED", "s1", "m0"
	w.gerrit[1].PatchSet++
	w.onMain["s1"] = true
	w.branch["main"] = "s1"
	pr2.Base.Ref = "main"
	plan := MakePlan(w.snapshot())
	for _, a := range plan.Actions {
		if s := a.String(); strings.Contains(s, frozenPrefix) {
			t.Errorf("planned %s: change 2 frozen on its old parent", s)
		}
	}
	w.converge()
	w.checkBase(t, 2, "main")
	w.checkBase(t, 3, "gerrit/"+cid(2))
}

// A change on an outdated patch set of the bottom is frozen, out of its
// stack, and GitHub does not move it when the bottom merges: it stays
// diffed against that exact parent.
func TestGitHubOwnedSquashedBottomKeepsAnOutdatedChildFrozen(t *testing.T) {
	w := newWorld(t)
	w.githubOwned = true
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.converge()
	w.upload(1, "a2", "m0")
	w.converge()
	w.checkBase(t, 2, "gerrit-base/"+cid(2))
	pr1 := w.pr(1)
	pr1.State, pr1.Merged = "closed", true
	w.gerrit[1].Status, w.gerrit[1].Revision = "MERGED", "s1"
	w.gerrit[1].PatchSet++
	w.onMain["s1"] = true
	w.branch["main"] = "s1"
	w.converge()
	w.checkBase(t, 2, "gerrit-base/"+cid(2))
}

// The change below merged (squashed on GitHub, marked merged in Gerrit at
// that commit, its current revision now) and the change above imported
// onto it: the squash is on the branch, though it is a change's revision,
// so the change above's PR is based on the branch (seen on the test
// instance: frozen at the squash, then moved back, its approvals
// dismissed).
func TestObserveMergedChangesRevisionIsOnTheBranch(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.GitHubOwned = true
	squash := s.commit(base, "one.txt", "1\n", "One (#10)\n\nChange-Id: "+cid(1)+"\n")
	s.push("github.git", squash, "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	s.change(1, cid(1), squash, "One (#10)\n\nChange-Id: "+cid(1)+"\n", "MERGED")
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
		HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}})
	two := s.commit(squash, "two.txt", "2\n", "Two\n\nChange-Id: "+cid(2)+"\n")
	s.change(2, cid(2), two, "Two\n\nChange-Id: "+cid(2)+"\n", "NEW")
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Changes[1] == nil {
		t.Fatalf("setup: change 1 not in the snapshot")
	}
	if !snap.OnBranch[squash] {
		t.Errorf("the squash, change 1's merged revision, is not seen on main")
	}
}

// GitHub restacks every layer above a merged bottom at once; the bridge
// imports them one at a time. A layer whose restack is not imported yet
// stands on the old patch set of the change below, which has a new one:
// its PR is left as GitHub has it (seen on the test instance: frozen,
// then moved back once imported), and nothing is pushed over it.
func TestGitHubOwnedUnimportedRestackKeepsItsBase(t *testing.T) {
	w := newWorld(t)
	w.githubOwned = true
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	pr1, pr2 := w.pr(1), w.pr(2)
	pr1.State, pr1.Merged = "closed", true
	w.gerrit[1].Status, w.gerrit[1].Revision, w.gerrit[1].Parent = "MERGED", "s1", "m0"
	w.gerrit[1].PatchSet++
	w.onMain["s1"] = true
	w.branch["main"] = "s1"
	pr2.Base.Ref = "main"
	// 2's restack imported (b2 on s1); 3's pushed by GitHub, not yet.
	w.upload(2, "b2", "s1")
	w.branch["gerrit/"+cid(3)] = "c1-restacked"
	before := w.pr(3).Base.Ref
	for _, a := range MakePlan(w.snapshot()).Actions {
		if s := a.String(); strings.Contains(s, frozenPrefix) || (a.PR == w.pr(3).Number && a.Update != nil && a.Update.Base != nil) {
			t.Errorf("planned %s while 3's restack waits to be imported", s)
		}
	}
	if w.pr(3).Base.Ref != before {
		t.Errorf("base moved")
	}
}

// ...and the stack GitHub kept, the layers above the merged one, is left
// as it is meanwhile: not dissolved and made again (which GitHub refuses
// for a PR with auto-merge on, leaving it out of the stack for good).
func TestGitHubOwnedUnimportedRestackKeepsItsStack(t *testing.T) {
	w := newWorld(t)
	w.githubOwned = true
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	pr1, pr2 := w.pr(1), w.pr(2)
	pr1.State, pr1.Merged = "closed", true
	w.gerrit[1].Status, w.gerrit[1].Revision, w.gerrit[1].Parent = "MERGED", "s1", "m0"
	w.gerrit[1].PatchSet++
	w.onMain["s1"] = true
	w.branch["main"] = "s1"
	pr2.Base.Ref = "main"
	for _, st := range w.stacks { // GitHub drops the merged PR from its stack
		st.PullRequests = slices.DeleteFunc(st.PullRequests, func(p github.StackPR) bool { return p.Number == pr1.Number })
	}
	w.upload(2, "b2", "s1")
	w.branch["gerrit/"+cid(3)] = "c1-restacked"
	for _, a := range MakePlan(w.snapshot()).Actions {
		if isStackAction(a.Kind) {
			t.Errorf("planned %s while 3's restack waits to be imported", a.String())
		}
	}
	if got := w.stacksAsChanges(); !slices.Equal(got[0], []int{2, 3}) || len(got) != 1 {
		t.Errorf("stacks = %v", got)
	}
}
