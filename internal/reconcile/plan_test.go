package reconcile

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// world is an in-memory GitHub plus Gerrit, enough to run plans to a fixed
// point. It enforces the GitHub rules the planner must respect.
type world struct {
	t        *testing.T
	gerrit   map[int]*GerritChange
	branch   map[string]string // all GitHub branches
	prs      map[int]*github.PullRequest
	stacks   map[int]*github.Stack
	state    map[int]*state.Change
	onMain   map[string]bool
	appendOK map[int]bool
	// onGitHubMain is what replication has pushed to GitHub's main.
	onGitHubMain map[string]bool
	nextNum      int
	log          []string
}

func newWorld(t *testing.T) *world {
	return &world{
		t: t, gerrit: map[int]*GerritChange{}, branch: map[string]string{"main": "m0"},
		prs: map[int]*github.PullRequest{}, stacks: map[int]*github.Stack{}, state: map[int]*state.Change{},
		onMain: map[string]bool{"m0": true}, appendOK: map[int]bool{}, onGitHubMain: map[string]bool{}, nextNum: 1,
	}
}

func cid(n int) string { return fmt.Sprintf("I%040d", n) }

// upload creates or amends change n with a revision on top of parent.
func (w *world) upload(n int, rev, parent string) {
	c := w.gerrit[n]
	if c == nil {
		c = &GerritChange{Number: n, ChangeID: cid(n), Status: "NEW", URL: fmt.Sprintf("https://g/c/p/+/%d", n)}
		w.gerrit[n] = c
	}
	c.PatchSet++
	c.Revision, c.Parent = rev, parent
	c.Subject = fmt.Sprintf("Change %d", n)
	c.Message = fmt.Sprintf("Change %d\n\nBody of %d, ps %d.\n\nChange-Id: %s", n, n, c.PatchSet, c.ChangeID)
}

func (w *world) snapshot() *Snapshot {
	s := &Snapshot{
		Branch: "main", DefaultBranch: "main",
		Changes: map[int]*GerritChange{}, OnBranch: w.onMain,
		GitHubBranches: map[string]string{}, PRs: map[int]*github.PullRequest{}, State: map[int]*state.Change{},
	}
	for n, c := range w.gerrit {
		cc := *c
		s.Changes[n] = &cc
	}
	for b, sha := range w.branch {
		if strings.HasPrefix(b, headPrefix) || strings.HasPrefix(b, frozenPrefix) {
			s.GitHubBranches[b] = sha
		}
	}
	s.AppendOK = map[int]bool{}
	s.SameTree = map[int]string{}
	for n, st := range w.state {
		if st.State.Origin == "github" {
			s.GitHubBranches[st.State.HeadBranch] = w.branch[st.State.HeadBranch]
			s.AppendOK[n] = w.appendOK[n]
		}
	}
	for n, pr := range w.prs {
		cp := *pr
		cp.Head.SHA = w.branch[pr.Head.Ref]
		s.PRs[n] = &cp
	}
	s.OnGitHubBranch = w.onGitHubMain
	for _, st := range w.stacks {
		cp := *st
		s.Stacks = append(s.Stacks, &cp)
	}
	slices.SortFunc(s.Stacks, func(a, b *github.Stack) int { return a.Number - b.Number })
	for n, st := range w.state {
		cp := *st
		s.State[n] = &cp
	}
	return s
}

func (w *world) stackOf(pr int) *github.Stack {
	for _, s := range w.stacks {
		if slices.Contains(s.Numbers(), pr) {
			return s
		}
	}
	return nil
}

func (w *world) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("GitHub would reject: "+format, args...)
}

func (w *world) apply(p *Plan) {
	w.t.Helper()
	for _, a := range p.Actions {
		w.log = append(w.log, a.String())
		switch a.Kind {
		case PushBranches, DeleteBranches:
			for name, ap := range a.Appends {
				if w.branch[name] != ap.Lease {
					w.fail("append lease on %s: have %q, lease %q", name, w.branch[name], ap.Lease)
				}
				sha := ap.Revision
				if !ap.Force {
					sha = "append(" + ap.Onto + "," + ap.Revision + ")"
				}
				w.branch[name] = sha
				if w.state[ap.Change] == nil {
					w.state[ap.Change] = &state.Change{}
				}
				st := w.state[ap.Change]
				st.Revisions = append(st.Revisions, state.Revision{PatchSet: ap.PatchSet, GerritSHA: ap.Revision, GitHubSHA: sha})
			}
			for name, sha := range a.Refs {
				if w.branch[name] != a.Leases[name] {
					w.fail("lease on %s: have %q, lease %q", name, w.branch[name], a.Leases[name])
				}
				if sha == "" {
					delete(w.branch, name)
				} else {
					w.branch[name] = sha
				}
			}
		case CreatePR:
			if w.branch[a.New.Head] == "" || w.branch[a.New.Base] == "" {
				w.fail("create PR %s -> %s: missing branch", a.New.Head, a.New.Base)
			}
			n := w.nextNum
			w.nextNum++
			w.prs[n] = &github.PullRequest{Number: n, NodeID: fmt.Sprintf("PR_%d", n), State: "open",
				Draft: a.New.Draft, Title: a.New.Title, Body: a.New.Body,
				Head: github.Ref{Ref: a.New.Head}, Base: github.Ref{Ref: a.New.Base}}
			w.link(a.Change, n)
		case UpdatePR:
			pr := w.prs[a.PR]
			u := a.Update
			if u.Base != nil {
				if w.stackOf(a.PR) != nil {
					w.fail("retarget #%d while stacked", a.PR)
				}
				if w.branch[*u.Base] == "" {
					w.fail("retarget #%d to missing %s", a.PR, *u.Base)
				}
				pr.Base.Ref = *u.Base
			}
			if u.Title != nil {
				pr.Title = *u.Title
			}
			if u.Body != nil {
				pr.Body = *u.Body
			}
			if u.State != nil {
				pr.State = *u.State
			}
			w.link(a.Change, a.PR)
		case SetDraft:
			w.prs[a.PR].Draft = a.Draft
		case Unstack:
			if w.stacks[a.Stack] == nil {
				w.fail("unstack missing #%d", a.Stack)
			}
			delete(w.stacks, a.Stack)
		case CreateStack, AddToStack:
			prs := a.PRs
			var st *github.Stack
			if a.Kind == AddToStack {
				st = w.stacks[a.Stack]
				prs = append(st.Numbers(), a.PRs...)
			}
			if len(prs) < 2 {
				w.fail("stack of %d", len(prs))
			}
			for i, n := range prs {
				pr := w.prs[n]
				if pr == nil || pr.State != "open" {
					w.fail("stack with closed/missing #%d", n)
				}
				if s := w.stackOf(n); s != nil && s != st {
					w.fail("#%d already in stack #%d", n, s.Number)
				}
				if i > 0 && pr.Base.Ref != w.prs[prs[i-1]].Head.Ref {
					w.fail("#%d base %s is not #%d's head %s", n, pr.Base.Ref, prs[i-1], w.prs[prs[i-1]].Head.Ref)
				}
			}
			if st == nil {
				st = &github.Stack{Number: w.nextNum, Open: true}
				w.nextNum++
				w.stacks[st.Number] = st
			}
			st.PullRequests = nil
			for _, n := range prs {
				st.PullRequests = append(st.PullRequests, github.StackPR{Number: n})
			}
		case ClosePR:
			if s := w.stackOf(a.PR); s != nil {
				// GitHub drops closed PRs from stacks; we unstack first anyway.
				w.fail("close #%d while stacked", a.PR)
			}
			w.prs[a.PR].State = "closed"
			if a.Comment != "" {
				w.log = append(w.log, fmt.Sprintf("comment #%d: %s", a.PR, a.Comment))
			}
			w.state[a.Change].State.ClosedNoticePosted = true
			w.state[a.Change].State.Lifecycle = w.gerrit[a.Change].Status
		}
	}
	// Record state the way the applier does.
	for n, d := range p.Desired {
		st := w.state[n]
		if st == nil || st.State.PR == 0 {
			continue
		}
		st.State.Title = w.gerrit[n].Subject
		st.State.BodyHash = d.BodyHash
		if st.State.Origin == "github" {
			title, body := prTextFromMessage(w.gerrit[n].Message)
			st.State.GerritMessageHash = hashString(title + "\x00" + body)
		}
		st.State.ClosedNoticePosted = false
	}
}

func (w *world) link(change, pr int) {
	if w.state[change] == nil {
		w.state[change] = &state.Change{}
	}
	w.state[change].State.PR = pr
}

// converge plans and applies until the plan is empty.
func (w *world) converge() {
	w.t.Helper()
	for i := 0; i < 6; i++ {
		p := MakePlan(w.snapshot())
		if len(p.Actions) == 0 {
			return
		}
		w.apply(p)
	}
	w.t.Fatalf("did not converge; last plan: %v", MakePlan(w.snapshot()).Actions)
}

func (w *world) pr(change int) *github.PullRequest {
	return w.prs[w.state[change].State.PR]
}

func (w *world) stacksAsChanges() [][]int {
	byPR := map[int]int{}
	for n, st := range w.state {
		byPR[st.State.PR] = n
	}
	var out [][]int
	for _, st := range w.stacks {
		var cs []int
		for _, n := range st.Numbers() {
			cs = append(cs, byPR[n])
		}
		out = append(out, cs)
	}
	slices.SortFunc(out, func(a, b []int) int { return a[0] - b[0] })
	return out
}

func (w *world) checkBase(t *testing.T, change int, base string) {
	t.Helper()
	if got := w.pr(change).Base.Ref; got != base {
		t.Errorf("change %d PR base = %s, want %s", change, got, base)
	}
}

func TestChainBecomesStack(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()

	w.checkBase(t, 1, "main")
	w.checkBase(t, 2, "gerrit/"+cid(1))
	w.checkBase(t, 3, "gerrit/"+cid(2))
	if w.branch["gerrit/"+cid(3)] != "c1" {
		t.Errorf("head branch = %q", w.branch["gerrit/"+cid(3)])
	}
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Errorf("stacks = %v", got)
	}
	body := w.pr(2).Body
	for _, want := range []string{"Body of 2, ps 1.", linksStart, "[change 2](https://g/c/p/+/2)", "← this PR", "(#1) Change 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	// A converged world stays still.
	if p := MakePlan(w.snapshot()); len(p.Actions) != 0 {
		t.Errorf("not idempotent: %v", p.Actions)
	}
}

func TestSingleChangeHasNoStack(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.gerrit[1].WIP = true
	w.converge()
	if len(w.stacks) != 0 || !w.pr(1).Draft {
		t.Errorf("stacks=%v draft=%v", w.stacks, w.pr(1).Draft)
	}
	w.gerrit[1].WIP = false
	w.converge()
	if w.pr(1).Draft {
		t.Error("still draft after WIP cleared")
	}
}

// Design §5.4's example: amend B and push only B.
func TestPartialPushFreezesBase(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()

	w.upload(2, "b2", "a1")
	w.converge()
	w.checkBase(t, 3, "gerrit-base/"+cid(3))
	if w.branch["gerrit-base/"+cid(3)] != "b1" {
		t.Errorf("frozen base at %q, want b1", w.branch["gerrit-base/"+cid(3)])
	}
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2}}) {
		t.Errorf("stacks = %v, want [[1 2]]", got)
	}
	if !strings.Contains(w.pr(3).Body, "outdated") {
		t.Error("PR 3 body does not explain the frozen base")
	}

	// Rebase C onto B2: it rejoins, and the frozen branch goes away.
	w.upload(3, "c2", "b2")
	w.converge()
	w.checkBase(t, 3, "gerrit/"+cid(2))
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Errorf("stacks = %v", got)
	}
	if _, ok := w.branch["gerrit-base/"+cid(3)]; ok {
		t.Error("frozen base not deleted")
	}
	if strings.Contains(w.pr(3).Body, "outdated") {
		t.Error("stale note left in body")
	}
}

func TestNewPRDefersNeighbourBodies(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.converge()
	w.upload(3, "c1", "b1")
	w.log = nil
	w.converge()
	edits := 0
	for _, l := range w.log {
		if strings.HasPrefix(l, "update-pr change=1 ") {
			edits++
		}
	}
	if edits != 1 {
		t.Errorf("change 1's PR edited %d times, want 1:\n%s", edits, strings.Join(w.log, "\n"))
	}
}

func TestGrowingChainExtendsStack(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.converge()
	before := w.stackOf(w.pr(1).Number).Number
	w.upload(3, "c1", "b1")
	w.converge()
	if st := w.stackOf(w.pr(3).Number); st == nil || st.Number != before {
		t.Errorf("stack not extended in place: %+v (was #%d)", st, before)
	}
}

func TestAbandonMiddle(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.upload(4, "d1", "c1")
	w.converge()

	w.gerrit[2].Status = "ABANDONED"
	w.converge()
	if w.pr(2).State != "closed" {
		t.Error("abandoned change's PR still open")
	}
	w.checkBase(t, 3, "gerrit-base/"+cid(3))
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{3, 4}}) {
		t.Errorf("stacks = %v, want [[3 4]] (bottom on a frozen base)", got)
	}
	n := 0
	for _, l := range w.log {
		if strings.HasPrefix(l, "comment #") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("posted %d close comments, want 1", n)
	}

	// Restore: the PR reopens and the chain re-forms.
	w.gerrit[2].Status = "NEW"
	w.converge()
	if w.pr(2).State != "open" {
		t.Error("restored change's PR not reopened")
	}
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2, 3, 4}}) {
		t.Errorf("stacks = %v", got)
	}
}

func TestMergedChainDeletesBridgeBranches(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	// Submit 1 and 2 (fast-forward); 3 stays open on top.
	for _, n := range []int{1, 2} {
		w.gerrit[n].Status = "MERGED"
	}
	w.onMain["a1"], w.onMain["b1"] = true, true
	w.converge()
	w.replicate()
	w.converge()
	w.checkBase(t, 3, "main")
	for _, n := range []int{1, 2} {
		if _, ok := w.branch["gerrit/"+cid(n)]; ok {
			t.Errorf("branch of merged change %d not deleted", n)
		}
		if w.pr(n).State != "closed" {
			t.Errorf("PR of merged change %d still open", n)
		}
	}
	if _, ok := w.branch["gerrit/"+cid(3)]; !ok {
		t.Error("open change's branch deleted")
	}
}

func TestMergedUnderFrozenStackRetargets(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	w.upload(1, "a2", "m0") // 2 now sits on an outdated parent: frozen
	w.converge()
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{2, 3}}) {
		t.Fatalf("stacks = %v", got)
	}
	// a1 gets onto main some other way (say an old submit): 2's base must
	// become main, which GitHub only allows outside a stack.
	w.onMain["a1"] = true
	w.converge()
	w.checkBase(t, 2, "main")
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{2, 3}}) {
		t.Errorf("stacks = %v", got)
	}
}

func TestMergedBottom(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()

	// Submitted with fast-forward: a1 is now on main.
	w.gerrit[1].Status = "MERGED"
	w.onMain["a1"] = true
	w.converge()
	w.replicate()
	w.converge()
	w.checkBase(t, 2, "main")
	if w.pr(1).State != "closed" {
		t.Error("merged change's PR still open")
	}
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{2, 3}}) {
		t.Errorf("stacks = %v", got)
	}
}

func TestTreeTakesLongestPath(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1") // short side branch
	w.upload(3, "c1", "a1")
	w.upload(4, "d1", "c1")
	w.upload(5, "e1", "b1")
	w.upload(6, "f1", "e1")
	w.upload(7, "g1", "f1") // 1-2-5-6-7 is longest
	w.converge()
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2, 5, 6, 7}, {3, 4}}) {
		t.Errorf("stacks = %v", got)
	}
	w.checkBase(t, 3, "gerrit/"+cid(1))
}

func TestReorder(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	// Swap 2 and 3: 1 <- 3 <- 2.
	w.upload(3, "c2", "a1")
	w.upload(2, "b2", "c2")
	w.converge()
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 3, 2}}) {
		t.Errorf("stacks = %v", got)
	}
	w.checkBase(t, 2, "gerrit/"+cid(3))
}

func TestPrivateNeverMirrored(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.gerrit[1].Private = true
	w.converge()
	if len(w.prs) != 0 || len(w.branch) != 1 {
		t.Errorf("private change leaked: prs=%v branches=%v", w.prs, w.branch)
	}
}

func TestGitHubBodyEditsSurviveUntilMessageChanges(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	pr := w.pr(1)
	_, block, _ := strings.Cut(pr.Body, linksStart)
	pr.Body = "Edited on GitHub.\n\n" + linksStart + block
	pr.Title = "Better title"
	w.converge()
	if !strings.HasPrefix(w.pr(1).Body, "Edited on GitHub.") || w.pr(1).Title != "Better title" {
		t.Errorf("GitHub edit lost: %q / %q", w.pr(1).Title, w.pr(1).Body)
	}
	// A new patch set with a new commit message wins.
	w.upload(1, "a2", "m0")
	w.gerrit[1].Subject = "Renamed"
	w.converge()
	if !strings.HasPrefix(w.pr(1).Body, "Body of 1, ps 2.") || w.pr(1).Title != "Renamed" {
		t.Errorf("message edit not mirrored: %q / %q", w.pr(1).Title, w.pr(1).Body)
	}
}

func TestAdoptsExistingPRWithoutState(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	n := w.pr(1).Number
	w.state = map[int]*state.Change{} // refs/bridge lost
	p := MakePlan(w.snapshot())
	for _, a := range p.Actions {
		if a.Kind == CreatePR {
			t.Fatalf("created a duplicate PR: %v", p.Actions)
		}
	}
	if p.Desired[1].PR != n {
		t.Errorf("did not adopt #%d: %+v", n, p.Desired[1])
	}
}

func TestNaming(t *testing.T) {
	id := cid(9)
	if HeadBranch("main", "main", id) != "gerrit/"+id || HeadBranch("rel-1", "main", id) != "gerrit/rel-1/"+id {
		t.Error("HeadBranch")
	}
	if b := FrozenBranch("rel-1", "main", id); b != "gerrit-base/rel-1/"+id {
		t.Errorf("FrozenBranch = %s", b)
	}
	if got, ok := frozenFor("gerrit-base/"+id, "main", "main"); !ok || got != id {
		t.Error("frozenFor default")
	}
	if _, ok := frozenFor("gerrit-base/rel-1/"+id, "main", "main"); ok {
		t.Error("frozenFor matched another branch's frozen base")
	}
	if _, ok := frozenFor("gerrit-base/rel-1/"+id, "rel-1", "main"); !ok {
		t.Error("frozenFor rel-1")
	}
}

// bornWorld has change 1 imported from PR #1 on the user's branch
// "feature", GitHub head h1 squashed into Gerrit commit s1.
func bornWorld(t *testing.T) *world {
	w := newWorld(t)
	w.upload(1, "s1", "m0")
	w.branch["feature"] = "h1"
	w.prs[1] = &github.PullRequest{Number: 1, State: "open", Title: "User title", Body: "User body.",
		Head: github.Ref{Ref: "feature"}, Base: github.Ref{Ref: "main"}}
	w.nextNum = 2
	w.state[1] = &state.Change{
		State:     state.ChangeState{PR: 1, Origin: "github", HeadBranch: "feature"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "s1", GitHubSHA: "h1"}},
	}
	return w
}

func TestGitHubBornInSync(t *testing.T) {
	w := bornWorld(t)
	w.gerrit[1].WIP = true // the user's draft state wins on GitHub
	w.converge()
	pr := w.prs[1]
	if w.branch["feature"] != "h1" {
		t.Errorf("user's branch rewritten to %s", w.branch["feature"])
	}
	if _, ok := w.branch["gerrit/"+cid(1)]; ok {
		t.Error("a gerrit/ branch was pushed for a GitHub-born change")
	}
	if pr.Title != "User title" || !strings.HasPrefix(pr.Body, "User body.\n\n"+linksStart) || pr.Draft {
		t.Errorf("PR text/draft not the user's: %q %q draft=%v", pr.Title, pr.Body, pr.Draft)
	}
}

func TestGitHubBornGerritEditAppends(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.upload(1, "s2", "m0") // edited in Gerrit, same parent
	w.appendOK[1] = true
	w.converge()
	if got := w.branch["feature"]; got != "append(h1,s2)" {
		t.Errorf("feature = %s, want a commit appended on h1", got)
	}
	// Rebased in Gerrit: force-push the patch set itself.
	w.upload(1, "s3", "m1")
	w.onMain["m1"] = true
	w.appendOK[1] = false
	w.converge()
	if got := w.branch["feature"]; got != "s3" {
		t.Errorf("feature = %s, want s3", got)
	}
}

func TestGitHubBornUnknownHeadIsNeverOverwritten(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.branch["feature"] = "h2" // the user pushed; not imported yet
	w.upload(1, "s2", "m0")    // and Gerrit moved too
	w.converge()
	if w.branch["feature"] != "h2" {
		t.Errorf("user's unimported push was overwritten with %s", w.branch["feature"])
	}
}

func TestStackOnGitHubBornParent(t *testing.T) {
	w := bornWorld(t)
	w.upload(2, "b1", "s1") // a Gerrit change on top of the imported one
	w.converge()
	w.checkBase(t, 2, "feature")
	// On GitHub it must sit on the user's commit h1, not Gerrit's s1, or
	// GitHub would diff it against main.
	if got := w.branch["gerrit/"+cid(2)]; got != "append(h1,b1)" {
		t.Errorf("gerrit/2 = %s, want b1's tree re-parented onto h1", got)
	}
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2}}) {
		t.Errorf("stacks = %v", got)
	}
}

func TestGitHubBornFrozenBaseUsesGitHubCommit(t *testing.T) {
	w := bornWorld(t)
	// Layer 2 imported on top of layer 1 (GitHub h2 -> Gerrit t1).
	w.upload(2, "t1", "s1")
	w.branch["feature2"] = "h2"
	w.prs[2] = &github.PullRequest{Number: 2, State: "open", Head: github.Ref{Ref: "feature2"}, Base: github.Ref{Ref: "feature"}}
	w.nextNum = 3
	w.state[2] = &state.Change{State: state.ChangeState{PR: 2, Origin: "github", HeadBranch: "feature2"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "t1", GitHubSHA: "h2"}}}
	w.converge()
	// The user force-pushes layer 1 only; it is imported as s2.
	w.branch["feature"] = "h1b"
	w.upload(1, "s2", "m0")
	w.state[1].Revisions = append(w.state[1].Revisions, state.Revision{PatchSet: 2, GerritSHA: "s2", GitHubSHA: "h1b"})
	w.converge()
	w.checkBase(t, 2, "gerrit-base/"+cid(2))
	if got := w.branch["gerrit-base/"+cid(2)]; got != "h1" {
		t.Errorf("frozen base at %s, want the GitHub commit h1 (not Gerrit's s1)", got)
	}
}

func TestGerritMessageEditUpdatesGitHubBornPR(t *testing.T) {
	w := bornWorld(t)
	w.gerrit[1].Message = "User title\n\nUser body.\n\nGitHub-PR: https://github.com/o/r/pull/1\nChange-Id: " + cid(1)
	w.state[1].State.GerritMessageHash = hashString("User title\x00User body.")
	w.converge()
	if w.prs[1].Title != "User title" {
		t.Fatalf("title changed without an edit: %q", w.prs[1].Title)
	}
	// Edited in Gerrit (a new patch set with the same tree, say).
	w.gerrit[1].Message = "Better title\n\nClearer body.\n\nGitHub-PR: https://github.com/o/r/pull/1\nChange-Id: " + cid(1)
	w.converge()
	pr := w.prs[1]
	if pr.Title != "Better title" || !strings.HasPrefix(pr.Body, "Clearer body.\n\n"+linksStart) {
		t.Errorf("PR not updated from Gerrit: %q / %q", pr.Title, pr.Body)
	}
	if strings.Contains(pr.Body, "Change-Id") || strings.Contains(pr.Body, "GitHub-PR:") {
		t.Errorf("bridge trailers leaked into the PR body: %q", pr.Body)
	}
}

func TestPRTextFromMessage(t *testing.T) {
	title, body := prTextFromMessage("Subject\n\nPara one.\n\nGitHub-PR: u\nChange-Id: I1\n")
	if title != "Subject" || body != "Para one." {
		t.Errorf("%q %q", title, body)
	}
}

func TestGitHubBornMessageOnlyEditPushesNothing(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.upload(1, "s1msg", "m0") // same tree, new message
	s := w.snapshot()
	s.SameTree[1] = "h1"
	p := MakePlan(s)
	for _, a := range p.Actions {
		if a.Kind == PushBranches {
			t.Errorf("pushed for a message-only edit: %v", a)
		}
	}
}

// replicate pushes Gerrit's main to GitHub, applying GitHub's merge
// detection: an open PR based on main whose head commit arrives is merged.
func (w *world) replicate() {
	for sha := range w.onMain {
		w.onGitHubMain[sha] = true
	}
	for _, pr := range w.prs {
		if pr.State == "open" && pr.Base.Ref == "main" && w.onGitHubMain[w.branch[pr.Head.Ref]] {
			pr.State, pr.Merged = "closed", true
		}
	}
}

func TestSubmittedStackShowsMergedOnGitHub(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.upload(3, "c1", "b1")
	w.converge()
	// Gerrit submits the whole chain (fast-forward); the bridge hears
	// before replication pushes.
	for n := 1; n <= 3; n++ {
		w.gerrit[n].Status = "MERGED"
	}
	w.onMain["a1"], w.onMain["b1"], w.onMain["c1"] = true, true, true
	w.converge()
	for n := 1; n <= 3; n++ {
		if pr := w.pr(n); pr.State != "open" || pr.Base.Ref != "main" {
			t.Errorf("change %d: PR %s based on %s; want open, retargeted to main", n, pr.State, pr.Base.Ref)
		}
		if _, ok := w.branch["gerrit/"+cid(n)]; !ok {
			t.Errorf("change %d: head branch deleted while its PR is open", n)
		}
	}
	w.replicate()
	w.converge()
	for n := 1; n <= 3; n++ {
		if pr := w.pr(n); !pr.Merged {
			t.Errorf("change %d: PR not merged on GitHub", n)
		}
		if _, ok := w.branch["gerrit/"+cid(n)]; ok {
			t.Errorf("change %d: head branch left behind", n)
		}
	}
}

func TestReplicationFirstFallsBackToClose(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.converge()
	w.gerrit[1].Status, w.gerrit[2].Status = "MERGED", "MERGED"
	w.onMain["a1"], w.onMain["b1"] = true, true
	w.replicate() // replication beat the bridge: PR 2 can't be retargeted now
	w.converge()
	if pr := w.pr(1); !pr.Merged {
		t.Error("bottom PR not merged")
	}
	if pr := w.pr(2); pr.State != "closed" {
		t.Errorf("PR 2 = %s; want closed with a notice", pr.State)
	}
}

func TestRebasedSubmitCloses(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	// Submitted with rebase: Gerrit merged a different commit.
	w.gerrit[1].Status = "MERGED"
	w.gerrit[1].Revision = "a1-rebased"
	w.onMain["a1-rebased"] = true
	w.converge()
	if pr := w.pr(1); pr.State != "closed" || pr.Merged {
		t.Errorf("PR = %s merged=%v; want closed with a notice", pr.State, pr.Merged)
	}
}
