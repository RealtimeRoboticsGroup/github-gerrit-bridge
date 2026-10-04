package reconcile

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// GerritChange is a change as the code reconciler sees it.
type GerritChange struct {
	Number   int
	ChangeID string
	Status   string // NEW, MERGED, ABANDONED
	WIP      bool
	Private  bool
	Subject  string
	Message  string
	PatchSet int
	Revision string // current patch set
	Parent   string // its first parent
	URL      string
}

// Snapshot is everything the planner needs about one target branch.
type Snapshot struct {
	// Branch is the Gerrit target branch; the GitHub branch has the same
	// name (Gerrit replicates it). DefaultBranch is GitHub's.
	Branch, DefaultBranch string
	// Changes holds the branch's open changes, plus closed ones the bridge
	// has state for.
	Changes map[int]*GerritChange
	// OnBranch is true for parent SHAs in the target branch's history.
	OnBranch map[string]bool
	// GitHubBranches maps bridge-owned GitHub branches to their SHAs.
	GitHubBranches map[string]string
	// PRs are the pull requests the bridge manages or may adopt.
	PRs map[int]*github.PullRequest
	// Stacks are the stacks that contain any of PRs.
	Stacks []*github.Stack
	// State is the bridge's stored state per change.
	State map[int]*state.Change
}

// ActionKind orders actions: a plan is applied in this order.
type ActionKind int

const (
	PushBranches ActionKind = iota
	Unstack
	CreatePR
	UpdatePR
	SetDraft
	CreateStack
	AddToStack
	ClosePR
	DeleteBranches
)

var kindNames = [...]string{"push", "unstack", "create-pr", "update-pr", "set-draft", "create-stack", "add-to-stack", "close-pr", "delete-branches"}

func (k ActionKind) String() string { return kindNames[k] }

// Action is one step toward the desired state.
type Action struct {
	Kind   ActionKind
	Change int
	PR     int
	NodeID string
	// Refs maps branch -> new SHA ("" deletes), and Leases branch ->
	// SHA it must currently have ("" means it must not exist).
	Refs, Leases map[string]string
	New          *github.NewPull
	Update       *github.PullUpdate
	Draft        bool
	Stack        int
	PRs          []int
	Comment      string
}

func (a Action) String() string {
	var b strings.Builder
	b.WriteString(a.Kind.String())
	if a.Change != 0 {
		fmt.Fprintf(&b, " change=%d", a.Change)
	}
	if a.PR != 0 {
		fmt.Fprintf(&b, " pr=#%d", a.PR)
	}
	switch a.Kind {
	case PushBranches, DeleteBranches:
		names := make([]string, 0, len(a.Refs))
		for n, sha := range a.Refs {
			if sha == "" {
				names = append(names, n+"=delete")
			} else {
				names = append(names, n+"="+short(sha))
			}
		}
		sort.Strings(names)
		b.WriteString(" " + strings.Join(names, " "))
	case CreatePR:
		fmt.Fprintf(&b, " head=%s base=%s draft=%v title=%q", a.New.Head, a.New.Base, a.New.Draft, a.New.Title)
	case UpdatePR:
		if a.Update.Base != nil {
			fmt.Fprintf(&b, " base=%s", *a.Update.Base)
		}
		if a.Update.Title != nil {
			fmt.Fprintf(&b, " title=%q", *a.Update.Title)
		}
		if a.Update.Body != nil {
			b.WriteString(" body")
		}
		if a.Update.State != nil {
			fmt.Fprintf(&b, " state=%s", *a.Update.State)
		}
	case SetDraft:
		fmt.Fprintf(&b, " draft=%v", a.Draft)
	case Unstack:
		fmt.Fprintf(&b, " stack=#%d", a.Stack)
	case CreateStack, AddToStack:
		if a.Stack != 0 {
			fmt.Fprintf(&b, " stack=#%d", a.Stack)
		}
		fmt.Fprintf(&b, " prs=%v", a.PRs)
	}
	return b.String()
}

func short(sha string) string { return sha[:min(len(sha), 8)] }

// Desired is what one mirrored change should look like on GitHub.
type Desired struct {
	Change   int
	Head     string
	Base     string
	Frozen   string // frozen base branch, if any
	FrozenAt string
	PR       int // existing PR, 0 if none yet
	Title    string
	BodyHash string
	Draft    bool
}

// Plan is the planner's output.
type Plan struct {
	Actions []Action
	Desired map[int]*Desired
	// Paths are the linear chains, bottom first; those of length >= 2 are
	// GitHub stacks.
	Paths [][]int
}

// MakePlan computes the actions that bring GitHub in line with Gerrit for
// one target branch. It is pure: same snapshot, same plan.
func MakePlan(s *Snapshot) *Plan {
	p := &Plan{Desired: map[int]*Desired{}}

	// Mirrored changes and their current revisions.
	mirrored := map[int]*GerritChange{}
	byRev := map[string]int{}
	for n, c := range s.Changes {
		if c.Status == "NEW" && !c.Private {
			mirrored[n] = c
			byRev[c.Revision] = n
		}
	}

	// Parent edges: a change stacks on another only if its parent is that
	// change's *current* patch set (rule 2).
	parentOf := map[int]int{}
	for n, c := range mirrored {
		if pn, ok := byRev[c.Parent]; ok && pn != n {
			parentOf[n] = pn
		}
	}
	p.Paths = linearPaths(mirrored, parentOf)
	pathOf := map[int][]int{}
	for _, path := range p.Paths {
		for _, n := range path {
			pathOf[n] = path
		}
	}

	// Existing PRs: the linked one, else adopt an open PR on the head
	// branch (state lost or not yet saved).
	prFor := map[int]*github.PullRequest{}
	byHead := map[string]*github.PullRequest{}
	for _, pr := range s.PRs {
		if pr.State == "open" {
			byHead[pr.Head.Ref] = pr
		}
	}
	for n, c := range s.Changes {
		if st := s.State[n]; st != nil && st.State.PR != 0 && s.PRs[st.State.PR] != nil {
			prFor[n] = s.PRs[st.State.PR]
		} else if pr := byHead[HeadBranch(s.Branch, s.DefaultBranch, c.ChangeID)]; pr != nil {
			prFor[n] = pr
		}
	}

	// Desired branches and PR shape.
	refs := map[string]string{}
	finalBase := map[int]string{} // PR -> base after this plan
	for _, n := range sortedKeys(mirrored) {
		c := mirrored[n]
		d := &Desired{Change: n, Head: HeadBranch(s.Branch, s.DefaultBranch, c.ChangeID), Title: c.Subject, Draft: c.WIP}
		refs[d.Head] = c.Revision
		var note string
		switch {
		case parentOf[n] != 0:
			d.Base = HeadBranch(s.Branch, s.DefaultBranch, mirrored[parentOf[n]].ChangeID)
		case s.OnBranch[c.Parent]:
			d.Base = s.Branch
		default:
			// Outdated patch set of another change, an abandoned or
			// private parent: pin the exact parent so the diff matches
			// Gerrit's.
			d.Frozen = FrozenBranch(s.Branch, s.DefaultBranch, c.ChangeID)
			d.FrozenAt = c.Parent
			d.Base = d.Frozen
			refs[d.Frozen] = c.Parent
			note = "⚠️ This change is based on an outdated or unmerged parent, so this PR is diffed against that exact commit and is not in the stack. Rebase it in Gerrit to rejoin the stack."
		}
		pr := prFor[n]
		if pr != nil {
			d.PR = pr.Number
		}
		var chain []chainEntry
		if path := pathOf[n]; len(path) > 1 {
			for _, m := range path {
				e := chainEntry{Change: m, URL: mirrored[m].URL, Subject: mirrored[m].Subject, This: m == n}
				if prFor[m] != nil {
					e.PR = prFor[m].Number
				}
				chain = append(chain, e)
			}
		}
		block := linksBlock(c.URL, n, c.PatchSet, chain, note)
		var lastHash, lastTitle, curBody string
		if st := s.State[n]; st != nil {
			lastHash, lastTitle = st.State.BodyHash, st.State.Title
		}
		if pr != nil {
			curBody = pr.Body
		}
		body, hash := composeBody(curBody, c.Message, lastHash, block)
		d.BodyHash = hash
		p.Desired[n] = d

		if pr == nil {
			p.Actions = append(p.Actions, Action{Kind: CreatePR, Change: n, New: &github.NewPull{
				Title: c.Subject, Head: d.Head, Base: d.Base, Body: body, Draft: c.WIP,
			}})
			continue
		}
		finalBase[pr.Number] = d.Base
		u := &github.PullUpdate{}
		changed := false
		if pr.State != "open" {
			u.State = ptr("open")
			changed = true
		}
		if pr.Base.Ref != d.Base {
			u.Base = ptr(d.Base)
			changed = true
		}
		// Rewrite the title only when the subject changed in Gerrit, so a
		// title edited on GitHub survives.
		if pr.Title != c.Subject && lastTitle != c.Subject {
			u.Title = ptr(c.Subject)
			changed = true
		}
		if pr.Body != body {
			u.Body = ptr(body)
			changed = true
		}
		if changed {
			p.Actions = append(p.Actions, Action{Kind: UpdatePR, Change: n, PR: pr.Number, Update: u})
		}
		if pr.Draft != c.WIP && pr.State == "open" {
			p.Actions = append(p.Actions, Action{Kind: SetDraft, Change: n, PR: pr.Number, NodeID: pr.NodeID, Draft: c.WIP})
		}
	}

	// Branch pushes, leased on what we observed.
	push := Action{Kind: PushBranches, Refs: map[string]string{}, Leases: map[string]string{}}
	for name, sha := range refs {
		if s.GitHubBranches[name] != sha {
			push.Refs[name] = sha
			push.Leases[name] = s.GitHubBranches[name]
		}
	}
	if len(push.Refs) > 0 {
		p.Actions = append(p.Actions, push)
	}

	// Stacks. Each path of 2+ changes is a desired stack; a PR that does
	// not exist yet is 0 and holds back stack edits that need it.
	var paths [][]int
	for _, path := range p.Paths {
		if len(path) < 2 {
			continue
		}
		prs := make([]int, len(path))
		for i, n := range path {
			if prFor[n] != nil {
				prs[i] = prFor[n].Number
			}
		}
		paths = append(paths, prs)
	}
	claimed := map[int]bool{} // index into paths
	inKept := map[int]bool{}
	for _, st := range s.Stacks {
		nums := st.Numbers()
		i := slices.IndexFunc(paths, func(w []int) bool {
			return len(w) >= len(nums) && slices.Equal(w[:len(nums)], nums)
		})
		if i < 0 || claimed[i] {
			p.Actions = append(p.Actions, Action{Kind: Unstack, Stack: st.Number})
			continue
		}
		claimed[i] = true
		for _, n := range nums {
			inKept[n] = true
		}
		// Growing on top keeps the stack (and its number). Wait until all
		// the new PRs exist.
		if rest := paths[i][len(nums):]; len(rest) > 0 && !slices.Contains(rest, 0) {
			p.Actions = append(p.Actions, Action{Kind: AddToStack, Stack: st.Number, PRs: rest})
		}
	}
	for i, prs := range paths {
		if claimed[i] || slices.Contains(prs, 0) || slices.ContainsFunc(prs, func(n int) bool { return inKept[n] }) {
			continue
		}
		p.Actions = append(p.Actions, Action{Kind: CreateStack, PRs: prs})
	}

	// Close PRs of changes that are no longer mirrored.
	for _, n := range sortedKeys(s.Changes) {
		c := s.Changes[n]
		if mirrored[n] != nil {
			continue
		}
		pr := prFor[n]
		if pr == nil {
			continue
		}
		finalBase[pr.Number] = pr.Base.Ref
		if pr.State != "open" || pr.Merged {
			continue
		}
		var msg string
		switch {
		case c.Status == "MERGED":
			msg = fmt.Sprintf("Merged in Gerrit: [change %d](%s). Closing this mirror PR.", n, c.URL)
		case c.Status == "ABANDONED":
			msg = fmt.Sprintf("Abandoned in Gerrit: [change %d](%s). Restoring it there reopens this PR.", n, c.URL)
		default:
			msg = fmt.Sprintf("[Change %d](%s) is no longer mirrored (made private). Closing this PR.", n, c.URL)
		}
		if st := s.State[n]; st != nil && st.State.ClosedNoticePosted {
			msg = ""
		}
		p.Actions = append(p.Actions, Action{Kind: ClosePR, Change: n, PR: pr.Number, Comment: msg})
	}

	// Frozen bases nothing needs any more.
	del := Action{Kind: DeleteBranches, Refs: map[string]string{}, Leases: map[string]string{}}
	for name, sha := range s.GitHubBranches {
		if _, ok := frozenFor(name, s.Branch, s.DefaultBranch); !ok || refs[name] != "" {
			continue
		}
		used := false
		for _, pr := range s.PRs {
			base, ok := finalBase[pr.Number]
			if !ok {
				base = pr.Base.Ref
			}
			used = used || (pr.State == "open" && base == name)
		}
		if !used {
			del.Refs[name] = ""
			del.Leases[name] = sha
		}
	}
	if len(del.Refs) > 0 {
		p.Actions = append(p.Actions, del)
	}

	// A new PR changes the relation chain in its neighbours' bodies; hold
	// back body-only edits until the next pass, when its number is known,
	// rather than editing every body twice.
	if slices.ContainsFunc(p.Actions, func(a Action) bool { return a.Kind == CreatePR }) {
		p.Actions = slices.DeleteFunc(p.Actions, func(a Action) bool {
			u := a.Update
			return a.Kind == UpdatePR && u.Body != nil && u.Base == nil && u.Title == nil && u.State == nil
		})
	}

	slices.SortStableFunc(p.Actions, func(a, b Action) int { return int(a.Kind) - int(b.Kind) })
	return p
}

// linearPaths splits the forest of changes (edges child -> parent) into
// linear paths, bottom first: from each root, the longest path down is one
// path (ties go to the lower change number), and every subtree hanging off
// it is split the same way.
func linearPaths(nodes map[int]*GerritChange, parentOf map[int]int) [][]int {
	children := map[int][]int{}
	for c, p := range parentOf {
		children[p] = append(children[p], c)
	}
	for _, cs := range children {
		sort.Ints(cs)
	}
	depth := map[int]int{}
	var height func(n int) int
	height = func(n int) int {
		if d, ok := depth[n]; ok {
			return d
		}
		depth[n] = 1 // guards against cycles, which Gerrit cannot have
		h := 1
		for _, c := range children[n] {
			h = max(h, 1+height(c))
		}
		depth[n] = h
		return h
	}
	var paths [][]int
	var walk func(root int)
	walk = func(root int) {
		path := []int{root}
		for n := root; ; {
			best := 0
			for _, c := range children[n] {
				if best == 0 || height(c) > height(best) {
					best = c
				}
			}
			for _, c := range children[n] {
				if c != best {
					walk(c)
				}
			}
			if best == 0 {
				break
			}
			path = append(path, best)
			n = best
		}
		paths = append(paths, path)
	}
	for _, n := range sortedKeys(nodes) {
		if _, ok := parentOf[n]; !ok {
			walk(n)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i][0] < paths[j][0] })
	return paths
}

func ptr[T any](v T) *T { return &v }

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}
