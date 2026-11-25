package reconcile

import (
	"strings"
	"testing"
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
