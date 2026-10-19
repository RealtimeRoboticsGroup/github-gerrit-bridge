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
	t      *testing.T
	gerrit map[int]*GerritChange
	branch map[string]string // all GitHub branches
	prs    map[int]*github.PullRequest
	stacks map[int]*github.Stack
	state  map[int]*state.Change
	onMain map[string]bool
	// onGitHubMain is what replication has pushed to GitHub's main.
	onGitHubMain map[string]bool
	// parents is the commit graph, for HasParent.
	parents map[string][]string
	nextNum int
	log     []string
	// withheld changes touch .github/ (Code.withhold).
	withheld map[int]bool
	// foreign changes' current patch sets are not the author's to push
	// (Code.markForeign).
	foreign map[int]bool
	// hold changes' PRs had their GitHub side mirrored this reconcile.
	hold map[int]bool
	// deferred changes' owners are over their new-PR budget.
	deferred map[int]bool
}

func newWorld(t *testing.T) *world {
	return &world{
		t: t, gerrit: map[int]*GerritChange{}, branch: map[string]string{"main": "m0"},
		prs: map[int]*github.PullRequest{}, stacks: map[int]*github.Stack{}, state: map[int]*state.Change{},
		onMain: map[string]bool{"m0": true}, onGitHubMain: map[string]bool{}, nextNum: 1,
		parents: map[string][]string{},
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
	w.parents[rev] = []string{parent}
	c.Subject = fmt.Sprintf("Change %d", n)
	c.Message = fmt.Sprintf("Change %d\n\nBody of %d, ps %d.\n\nChange-Id: %s", n, n, c.PatchSet, c.ChangeID)
}

func (w *world) snapshot() *Snapshot {
	s := &Snapshot{
		Branch: "main", DefaultBranch: "main",
		Changes: map[int]*GerritChange{}, OnBranch: w.onMain,
		GitHubBranches: map[string]string{}, PRs: map[int]*github.PullRequest{}, State: map[int]*state.Change{},
		Repo: "o/r", NoPush: map[int]bool{}, Withheld: w.withheld, Foreign: w.foreign, Hold: w.hold, Deferred: w.deferred,
	}
	// Like Code.observe: closed changes drop out once the bridge has
	// recorded them closed, unless their PR is still open.
	for n, c := range w.gerrit {
		if st := w.state[n]; c.Status != "NEW" && st != nil && st.State.Lifecycle != "NEW" &&
			(w.prs[st.State.PR] == nil || w.prs[st.State.PR].State != "open") {
			continue
		}
		cc := *c
		s.Changes[n] = &cc
	}
	for b, sha := range w.branch {
		if strings.HasPrefix(b, headPrefix) || strings.HasPrefix(b, frozenPrefix) {
			s.GitHubBranches[b] = sha
		}
	}
	s.SameTree = map[int]string{}
	s.HasParent = map[int]bool{}
	for n, st := range w.state {
		if st.State.Origin == "github" {
			s.GitHubBranches[st.State.HeadBranch] = w.branch[st.State.HeadBranch]
			if pr := w.prs[st.State.PR]; isForkHead(st.State.HeadBranch) && pr != nil && !pr.MaintainerCanModify {
				s.NoPush[n] = true
			}
		}
	}
	heads := map[string]bool{}
	for n, c := range s.Changes {
		heads[s.head(n, c.ChangeID)] = true
	}
	for n, pr := range w.prs {
		// Open PRs on a change's head, plus the recorded PR of each change.
		linked := false
		for c, st := range w.state {
			linked = linked || (st.State.PR == n && s.Changes[c] != nil)
		}
		key := pr.Head.Ref
		if pr.Head.Repo != nil {
			key = pr.HeadKey(s.Repo)
		}
		if !linked && !(pr.State == "open" && heads[key]) {
			continue
		}
		cp := *pr
		if cp.Head.Repo == nil { // the world's PRs are from o/r unless said otherwise
			cp.Head.Repo = &struct {
				FullName string `json:"full_name"`
			}{"o/r"}
		}
		cp.Head.SHA = w.branch[cp.HeadKey(s.Repo)]
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
	for n, c := range s.Changes {
		if gh, ok := s.GitHubBranches[s.head(n, c.ChangeID)]; ok {
			s.HasParent[n] = w.isAncestor(s.ghCommit(c.Parent), gh)
		}
	}
	return s
}

func (w *world) isAncestor(a, b string) bool {
	if a == b {
		return true
	}
	for _, p := range w.parents[b] {
		if w.isAncestor(a, p) {
			return true
		}
	}
	return false
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
					w.parents[sha] = []string{ap.Onto}
					if ap.Merge != "" {
						sha = "merge(" + ap.Onto + "," + ap.Merge + "," + ap.Revision + ")"
						w.parents[sha] = []string{ap.Onto, ap.Merge}
					}
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
	for n, st := range w.state {
		if c := w.gerrit[n]; c != nil && st.State.PR != 0 {
			st.State.Lifecycle = c.Status
		}
	}
	for n, d := range p.Desired {
		st := w.state[n]
		if st == nil || st.State.PR == 0 {
			continue
		}
		if gh := w.branch[d.Head]; st.State.Origin != "github" && gh == w.gerrit[n].Revision &&
			!slices.ContainsFunc(st.Revisions, func(r state.Revision) bool { return r.GitHubSHA == gh }) {
			st.Revisions = append(st.Revisions, state.Revision{PatchSet: w.gerrit[n].PatchSet, GerritSHA: gh, GitHubSHA: gh})
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
	st := &w.state[change].State
	st.PR = pr
	st.HeadBranch = w.prs[pr].Head.Ref
	if w.prs[pr].Head.Repo != nil {
		st.HeadBranch = w.prs[pr].HeadKey("o/r")
	}
	st.Lifecycle = "NEW"
	if st.Origin == "" {
		st.Origin = "gerrit"
	}
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
	for _, want := range []string{"Body of 2, ps 1.", linksStart, "**[Change 2, patch set 1](https://g/c/p/+/2/1) (" + githubLogo + " #2)** ← this PR", "[Change 1, patch set 1](https://g/c/p/+/1/1) (" + githubLogo + " #1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Change-Id") {
		t.Errorf("PR body has the Change-Id trailer:\n%s", body)
	}
	// Top first, as in Gerrit.
	if i3, i1 := strings.Index(body, "Change 3"), strings.Index(body, "Change 1"); i3 < 0 || i1 < i3 {
		t.Errorf("relation chain not top first:\n%s", body)
	}
	// A converged world stays still.
	if p := MakePlan(w.snapshot()); len(p.Actions) != 0 {
		t.Errorf("not idempotent: %v", p.Actions)
	}
}

// A PR whose GitHub side was mirrored once this reconcile keeps its
// newer edits, draft state, close or reopen for the next one: the plan
// does not undo them.
func TestHeldPRIsLeftAsItIs(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	pr := w.pr(1)
	pr.Title, pr.Draft = "Edited again", true
	w.hold = map[int]bool{1: true}
	w.converge()
	if pr.Title != "Edited again" || !pr.Draft {
		t.Errorf("held PR changed: %q draft=%v", pr.Title, pr.Draft)
	}
	pr.State = "closed"
	w.converge()
	if pr.State != "closed" {
		t.Error("held PR reopened")
	}
	w.gerrit[1].Status, pr.State = "ABANDONED", "open"
	w.converge()
	if pr.State != "open" {
		t.Error("held PR closed")
	}
	w.hold = nil
	w.converge()
	if pr.State != "closed" {
		t.Error("PR of an abandoned change left open once no longer held")
	}
}

func TestHoldIsForHandledPRs(t *testing.T) {
	s := &Snapshot{State: map[int]*state.Change{1: {State: state.ChangeState{PR: 10}}, 2: {State: state.ChangeState{PR: 20}}, 3: {}, 4: nil}}
	s.hold(map[int]bool{10: true, 0: true})
	if !reflect.DeepEqual(s.Hold, map[int]bool{1: true}) {
		t.Errorf("hold = %v", s.Hold)
	}
}

// A change deferred for its owner's budget gets nothing on GitHub yet;
// one that has begun keeps going.
func TestDeferredChangeWaits(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "m0")
	w.deferred = map[int]bool{2: true}
	w.converge()
	if _, ok := w.branch["gerrit/"+cid(2)]; ok || w.state[2] != nil {
		t.Error("a deferred change was exported")
	}
	if w.state[1] == nil {
		t.Error("the other change was not exported")
	}
	w.deferred = nil
	w.converge()
	if w.state[2] == nil {
		t.Error("change 2 not exported once allowed")
	}
}

// A change the importer uploaded for a PR, whose link is not saved yet,
// is not taken for a Gerrit-born change: no second PR is opened for it.
func TestUnlinkedImportGetsNoPR(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.gerrit[1].Message += "\nGitHub-PR: https://github.com/o/r/pull/7"
	w.upload(2, "b1", "m0")
	w.gerrit[2].Message += "\nGitHub-PR: https://github.com/other/repo/pull/7"
	w.converge()
	if w.state[1] != nil {
		t.Error("a PR was opened for an unlinked import")
	}
	if w.state[2] == nil {
		t.Error("a change citing another repository's PR was not mirrored")
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
	// A chain of one still links to Gerrit, with the patch set.
	if body := w.pr(1).Body; !strings.Contains(body, "[Change 1, patch set 1](https://g/c/p/+/1/1) ("+githubLogo+" #1)** ← this PR") {
		t.Errorf("PR body has no link to its patch set:\n%s", body)
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

// The planner shows the commit message, wherever the change started:
// GitHub edits reach Gerrit before planning (Code.mirrorPREdits), so an
// edit still on the PR here is one Gerrit did not take, and is undone.
func TestPlanShowsTheCommitMessage(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	pr := w.pr(1)
	_, block, _ := strings.Cut(pr.Body, linksStart)
	pr.Body = "Edited on GitHub.\n\n" + linksStart + block
	pr.Title = "Better title"
	w.converge()
	if !strings.HasPrefix(w.pr(1).Body, "Body of 1, ps 1.") || w.pr(1).Title != "Change 1" {
		t.Errorf("PR not back to the commit message: %q / %q", w.pr(1).Title, w.pr(1).Body)
	}
	// A new commit message shows.
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
	// Imported from the PR: the commit message is the PR's text.
	w.gerrit[1].Subject = "User title"
	w.gerrit[1].Message = "User title\n\nUser body.\n\nGitHub-PR: https://github.com/o/r/pull/1\nChange-Id: " + cid(1)
	w.branch["feature"] = "h1"
	w.parents["h1"] = []string{"m0"} // the author's commit, which Gerrit squashed as s1
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
	w.converge()
	pr := w.prs[1]
	if w.branch["feature"] != "h1" {
		t.Errorf("user's branch rewritten to %s", w.branch["feature"])
	}
	if _, ok := w.branch["gerrit/"+cid(1)]; ok {
		t.Error("a gerrit/ branch was pushed for a GitHub-born change")
	}
	if pr.Title != "User title" || !strings.HasPrefix(pr.Body, "User body.\n\n"+linksStart) || pr.Draft {
		t.Errorf("PR text/draft changed: %q %q draft=%v", pr.Title, pr.Body, pr.Draft)
	}
	if strings.Contains(pr.Body, "GitHub-PR:") || strings.Contains(pr.Body, "Change-Id") {
		t.Errorf("bridge trailers in the PR body: %q", pr.Body)
	}
	// WIP set in Gerrit shows as a draft, as for any change.
	w.gerrit[1].WIP = true
	w.converge()
	if !w.prs[1].Draft {
		t.Error("WIP in Gerrit did not make the PR a draft")
	}
}

func TestGitHubBornGerritEditAppends(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.upload(1, "s2", "m0") // edited in Gerrit, same parent
	w.converge()
	if got := w.branch["feature"]; got != "append(h1,s2)" {
		t.Errorf("feature = %s, want a commit appended on h1", got)
	}
	// Rebased in Gerrit: a merge of the new parent, still a fast-forward
	// of the user's branch (it used to be force-pushed).
	w.upload(1, "s3", "m1")
	w.onMain["m1"] = true
	w.converge()
	if got := w.branch["feature"]; got != "merge(append(h1,s2),m1,s3)" {
		t.Errorf("feature = %s, want a merge of m1 on top of the user's branch", got)
	}
}

// A patch set someone else uploaded in Gerrit never reaches the author's
// branch, nor does its commit message reach the PR; the owner's next one
// does.
func TestGitHubBornForeignPatchSetStaysInGerrit(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.upload(1, "s2", "m1") // by someone else, rebased onto their own m1
	w.gerrit[1].Subject, w.gerrit[1].Message = "Mallory's title", "Mallory's title\n\nMallory's body.\n\nChange-Id: "+cid(1)
	w.foreign = map[int]bool{1: true}
	w.converge()
	pr := w.prs[1]
	if w.branch["feature"] != "h1" || pr.Base.Ref != "main" {
		t.Errorf("feature = %s, base %s: a foreign patch set reached the author's PR", w.branch["feature"], pr.Base.Ref)
	}
	if pr.Title != "User title" || strings.Contains(pr.Body, "Mallory") {
		t.Errorf("PR text changed to %q %q", pr.Title, pr.Body)
	}
	if _, ok := w.branch["gerrit-base/"+cid(1)]; ok {
		t.Error("a frozen base was pushed for it")
	}
	w.upload(1, "s3", "m0") // the owner's own
	w.gerrit[1].Subject = "Owner's title"
	w.foreign = nil
	w.converge()
	if got := w.branch["feature"]; got != "append(h1,s3)" || w.prs[1].Title != "Owner's title" {
		t.Errorf("feature = %s, title %q: the owner's patch set was not pushed", got, w.prs[1].Title)
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

// forkWorld is bornWorld with PR #1 from alice's fork, on a branch named
// like the repository's own main.
func forkWorld(t *testing.T, maintainerCanModify bool) *world {
	w := bornWorld(t)
	delete(w.branch, "feature")
	w.branch["alice/r:main"] = "h1"
	w.prs[1].Head = github.Ref{Ref: "main", Repo: &struct {
		FullName string `json:"full_name"`
	}{"alice/r"}}
	w.prs[1].MaintainerCanModify = maintainerCanModify
	w.state[1].State.HeadBranch = "alice/r:main"
	return w
}

func TestForkPR(t *testing.T) {
	w := forkWorld(t, true)
	w.converge()
	if w.branch["main"] != "m0" || w.branch["alice/r:main"] != "h1" {
		t.Fatalf("branches changed: main=%s fork=%s", w.branch["main"], w.branch["alice/r:main"])
	}
	// Edited in Gerrit: appended to the fork's branch, never to main.
	w.upload(1, "s2", "m0")
	w.converge()
	if w.branch["main"] != "m0" {
		t.Errorf("main = %s: a Gerrit edit of a fork PR went to the repository's main", w.branch["main"])
	}
	if got := w.branch["alice/r:main"]; got != "append(h1,s2)" {
		t.Errorf("fork branch = %s, want a commit appended on h1", got)
	}
	// A Gerrit change on top: no PR can be based on a fork's branch, so it
	// gets a frozen base at the fork's commit and no stack.
	w.upload(2, "b1", "s2")
	w.converge()
	w.checkBase(t, 2, "gerrit-base/"+cid(2))
	if got := w.branch["gerrit-base/"+cid(2)]; got != "append(h1,s2)" {
		t.Errorf("frozen base at %s", got)
	}
	if got := w.stacksAsChanges(); len(got) != 0 {
		t.Errorf("stacks = %v", got)
	}
}

func TestForkPRWithoutMaintainerEdits(t *testing.T) {
	w := forkWorld(t, false)
	w.converge()
	w.upload(1, "s2", "m0")
	w.converge()
	if w.branch["alice/r:main"] != "h1" || w.branch["main"] != "m0" {
		t.Errorf("pushed although the fork does not allow it: fork=%s main=%s", w.branch["alice/r:main"], w.branch["main"])
	}
}

func TestGerritMessageEditUpdatesGitHubBornPR(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	if w.prs[1].Title != "User title" {
		t.Fatalf("title changed without an edit: %q", w.prs[1].Title)
	}
	// Edited in Gerrit (a new patch set with the same tree, say).
	w.gerrit[1].Subject = "Better title"
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

func TestRebasedSubmitShowsMerged(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	// Submitted with rebase: Gerrit merged a different commit, which the
	// bridge swaps in before replication brings it.
	w.gerrit[1].Status = "MERGED"
	w.gerrit[1].Revision = "a1-rebased"
	w.onMain["a1-rebased"] = true
	w.converge()
	if got := w.branch["gerrit/"+cid(1)]; got != "a1-rebased" {
		t.Errorf("head = %s, want the merged commit", got)
	}
	w.replicate()
	w.converge()
	if pr := w.pr(1); !pr.Merged {
		t.Errorf("PR = %s merged=%v; want merged", pr.State, pr.Merged)
	}
}

func TestRebasedSubmitAfterReplicationCloses(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	w.gerrit[1].Status = "MERGED"
	w.gerrit[1].Revision = "a1-rebased"
	w.onMain["a1-rebased"] = true
	w.replicate() // replication beat the bridge
	w.converge()
	if pr := w.pr(1); pr.State != "closed" || pr.Merged {
		t.Errorf("PR = %s merged=%v; want closed with a notice", pr.State, pr.Merged)
	}
	if !slices.ContainsFunc(w.log, func(l string) bool {
		return strings.HasSuffix(l, ": Merged in Gerrit as [change 1](https://g/c/p/+/1).")
	}) {
		t.Errorf("no merged notice:\n%s", strings.Join(w.log, "\n"))
	}
}

// Design §5.2.1: a Gerrit-born PR keeps every patch set in its history.
func TestGerritPatchSetsKeepHistory(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	head := "gerrit/" + cid(1)
	if got := w.branch[head]; got != "a1" {
		t.Fatalf("first patch set: head = %s, want Gerrit's commit a1", got)
	}
	w.upload(1, "a2", "m0") // amended
	w.converge()
	if got := w.branch[head]; got != "append(a1,a2)" {
		t.Errorf("amend: head = %s, want a2's tree on top of a1", got)
	}
	w.upload(1, "a3", "m1") // rebased
	w.onMain["m1"] = true
	w.parents["m1"] = []string{"m0"}
	w.converge()
	if got := w.branch[head]; got != "merge(append(a1,a2),m1,a3)" {
		t.Errorf("rebase: head = %s, want a merge of m1 on top of the history", got)
	}
	w.upload(1, "a4", "m1") // amended again
	w.converge()
	if got := w.branch[head]; got != "append(merge(append(a1,a2),m1,a3),a4)" {
		t.Errorf("amend after rebase: head = %s", got)
	}
	if p := MakePlan(w.snapshot()); len(p.Actions) != 0 {
		t.Errorf("not idempotent: %v", p.Actions)
	}
	// Submitted: the bridge swaps in Gerrit's commit, and GitHub shows the
	// PR merged when replication brings it.
	w.gerrit[1].Status = "MERGED"
	w.onMain["a4"] = true
	w.converge()
	if got := w.branch[head]; got != "a4" {
		t.Errorf("submit: head = %s, want the merged commit a4", got)
	}
	w.replicate()
	w.converge()
	if !w.pr(1).Merged {
		t.Error("PR not merged on GitHub")
	}
}

func TestStackKeepsHistory(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.upload(2, "b1", "a1")
	w.converge()
	w.upload(1, "a2", "m0")
	w.converge()
	w.checkBase(t, 2, "gerrit-base/"+cid(2))
	if got := w.branch["gerrit-base/"+cid(2)]; got != "a1" {
		t.Errorf("frozen base at %s, want a1", got)
	}
	// Rebased onto a2: a merge of layer 1's new head, so layer 2's diff
	// against it is b2's alone.
	w.upload(2, "b2", "a2")
	w.converge()
	if got := w.branch["gerrit/"+cid(2)]; got != "merge(b1,append(a1,a2),b2)" {
		t.Errorf("layer 2 head = %s", got)
	}
	w.checkBase(t, 2, "gerrit/"+cid(1))
	if got := w.stacksAsChanges(); !reflect.DeepEqual(got, [][]int{{1, 2}}) {
		t.Errorf("stacks = %v", got)
	}
	// Both submitted: both heads become Gerrit's commits, retargeted.
	w.gerrit[1].Status, w.gerrit[2].Status = "MERGED", "MERGED"
	w.onMain["a2"], w.onMain["b2"] = true, true
	w.converge()
	for n, want := range map[int]string{1: "a2", 2: "b2"} {
		if got := w.branch["gerrit/"+cid(n)]; got != want {
			t.Errorf("change %d head = %s, want %s", n, got, want)
		}
	}
	w.replicate()
	w.converge()
	for n := 1; n <= 2; n++ {
		if !w.pr(n).Merged {
			t.Errorf("change %d: PR not merged", n)
		}
	}
}

func TestBridgeHead(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	s := w.snapshot()
	head := "gerrit/" + cid(1)
	if got := s.bridgeHead(1, head); got != "a1" {
		t.Errorf("Gerrit's own commit: bridgeHead = %q, want a1", got)
	}
	s.GitHubBranches[head] = "a1-same-tree"
	s.SameTree[1] = "a1-same-tree"
	if got := s.bridgeHead(1, head); got != "a1-same-tree" {
		t.Errorf("same tree and parent: bridgeHead = %q", got)
	}
	s.GitHubBranches[head] = "someones-push"
	if got := s.bridgeHead(1, head); got != "" {
		t.Errorf("a push: bridgeHead = %q, want none", got)
	}
}

func TestGerritMessageOnlyEditPushesNothing(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	w.upload(1, "a1msg", "m0") // same tree, new message
	s := w.snapshot()
	s.SameTree[1] = "a1"
	for _, a := range MakePlan(s).Actions {
		if a.Kind == PushBranches {
			t.Errorf("pushed for a message-only edit: %v", a)
		}
	}
}

// Design §5.2.2: a push to a Gerrit-born PR's branch is the importer's to
// turn into a patch set; the planner must not overwrite it.
func TestPushToGerritBornBranchIsNotOverwritten(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	w.branch["gerrit/"+cid(1)] = "rebased-on-github"
	w.converge()
	if got := w.branch["gerrit/"+cid(1)]; got != "rebased-on-github" {
		t.Errorf("head = %s: the push was overwritten", got)
	}
	// Nor may the converged state record the push as the patch set's
	// GitHub commit: that would hide it from the importer.
	if gh := w.snapshot().bridgeHead(1, "gerrit/"+cid(1)); gh != "" {
		t.Errorf("bridgeHead = %s: a push would be recorded as patch set 1", gh)
	}
	// Imported as patch set 2: in sync, nothing pushed.
	w.upload(1, "a2", "m1")
	w.onMain["m1"] = true
	w.state[1].Revisions = append(w.state[1].Revisions, state.Revision{PatchSet: 2, GerritSHA: "a2", GitHubSHA: "rebased-on-github"})
	w.log = nil
	w.converge()
	for _, l := range w.log {
		if strings.HasPrefix(l, "push") {
			t.Errorf("pushed after the import: %s", l)
		}
	}
}

// It should not matter where a change started: a GitHub-born PR, which
// Gerrit squashed, gets the merged commit swapped onto the author's branch
// and shows merged too.
func TestGitHubBornSubmitShowsMerged(t *testing.T) {
	w := bornWorld(t)
	w.converge()
	w.gerrit[1].Status = "MERGED" // Gerrit merged s1, its squash of h1
	w.onMain["s1"] = true
	w.converge()
	if got := w.branch["feature"]; got != "s1" {
		t.Errorf("feature = %s, want the merged commit s1", got)
	}
	w.replicate()
	w.converge()
	if pr := w.prs[1]; !pr.Merged {
		t.Errorf("PR %s merged=%v; want merged", pr.State, pr.Merged)
	}
}

func TestForkWithoutMaintainerEditsClosesOnSubmit(t *testing.T) {
	w := forkWorld(t, false)
	w.converge()
	w.gerrit[1].Status = "MERGED"
	w.onMain["s1"] = true
	w.converge()
	if got := w.branch["alice/r:main"]; got != "h1" {
		t.Errorf("pushed %s to a fork that does not allow it", got)
	}
	w.replicate()
	w.converge()
	if pr := w.prs[1]; pr.State != "closed" || pr.Merged {
		t.Errorf("PR %s merged=%v; want closed with a notice", pr.State, pr.Merged)
	}
}

// A merged change's record maps its merged commit to its old GitHub
// history head. Once that commit is on main it is the same on both sides:
// a change uploaded on top of it is exported as Gerrit's own commit, not
// rebuilt onto the stale history head.
func TestParentOnBranchIsItselfOnGitHub(t *testing.T) {
	w := newWorld(t)
	w.upload(1, "a1", "m0")
	w.converge()
	w.upload(1, "a2", "m0") // amended: GitHub has append(a1,a2) for a2
	w.converge()
	w.gerrit[1].Status = "MERGED"
	w.onMain["a2"] = true
	w.converge()
	w.replicate()
	w.converge()
	w.upload(2, "b1", "a2") // on the new main
	w.converge()
	if got := w.branch["gerrit/"+cid(2)]; got != "b1" {
		t.Errorf("gerrit/2 = %s, want Gerrit's own commit b1", got)
	}
	w.checkBase(t, 2, "main")
}

// A change touching .github/ is never exported; one that comes to touch
// it keeps its PR as it was: nothing is pushed for it, frozen bases
// included, and its base stays.
func TestWithheldChanges(t *testing.T) {
	w := newWorld(t)
	w.withheld = map[int]bool{1: true}
	w.upload(1, "a1", "m0")
	w.converge()
	if len(w.prs) != 0 || len(w.branch) != 1 {
		t.Errorf("withheld change exported: prs=%v branches=%v", w.prs, w.branch)
	}

	w.upload(2, "b1", "m0")
	w.upload(3, "c1", "b1")
	w.converge()
	w.upload(2, "b2", "m0") // 3 now sits on an outdated patch set
	w.withheld[3] = true
	w.withheld[2] = true
	w.converge()
	if w.branch["gerrit/"+cid(2)] != "b1" || w.branch["gerrit/"+cid(3)] != "c1" {
		t.Errorf("pushed for withheld changes: %v", w.branch)
	}
	if _, ok := w.branch["gerrit-base/"+cid(3)]; ok {
		t.Error("froze a base for a withheld change")
	}
	w.checkBase(t, 3, "gerrit/"+cid(2))
	if w.pr(2).State != "open" || w.pr(3).State != "open" {
		t.Error("closed a withheld change's PR")
	}
}
