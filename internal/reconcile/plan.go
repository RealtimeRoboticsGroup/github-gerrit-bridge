package reconcile

import (
	"fmt"
	"regexp"
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
	// OnGitHubBranch is true for merged revisions already on GitHub's copy
	// of the target branch (replication has pushed them).
	OnGitHubBranch map[string]bool
	// AppendOK marks GitHub-born changes whose current patch set has the
	// same parent tree as the one last synced to GitHub, so a Gerrit edit
	// can be appended to the user's branch as a fast-forward (design §5.2).
	AppendOK map[int]bool
	// SameTree maps a GitHub-born change to its GitHub head when that head
	// has the current patch set's tree and parent tree: rule 1 says they
	// are the same revision (e.g. a commit-message edit in Gerrit), so
	// nothing is pushed.
	SameTree map[int]string
	// Repo is the GitHub repository, owner/name.
	Repo string
	// NoPush marks GitHub-born changes from forks whose authors do not
	// allow maintainers to push: Gerrit edits stay in Gerrit.
	NoPush map[int]bool
}

// githubBorn reports whether change n came from a GitHub PR.
func (s *Snapshot) githubBorn(n int) bool {
	st := s.State[n]
	return st != nil && st.State.Origin == "github"
}

// head is the GitHub branch holding change n: the user's own branch for a
// GitHub-born change, else the bridge's gerrit/<Change-Id>.
func (s *Snapshot) head(n int, changeID string) string {
	if st := s.State[n]; st != nil && st.State.Origin == "github" && st.State.HeadBranch != "" {
		return st.State.HeadBranch
	}
	return HeadBranch(s.Branch, s.DefaultBranch, changeID)
}

// isForkHead reports whether head is a branch in a fork. No PR can be
// based on one, so a change above it gets a frozen base.
func isForkHead(head string) bool {
	_, _, fork := github.SplitHeadKey(head)
	return fork
}

// ghCommit is the GitHub commit standing for Gerrit commit sha: the one
// recorded for it, else sha itself.
func (s *Snapshot) ghCommit(sha string) string {
	for _, st := range s.State {
		for i := len(st.Revisions) - 1; i >= 0; i-- {
			if r := st.Revisions[i]; r.GerritSHA == sha && r.GitHubSHA != "" {
				return r.GitHubSHA
			}
		}
	}
	return sha
}

// githubFor maps a GitHub commit of change n to the Gerrit commit it was
// synced with.
func (s *Snapshot) githubFor(n int) map[string]string {
	m := map[string]string{}
	if st := s.State[n]; st != nil {
		for _, r := range st.Revisions {
			m[r.GitHubSHA] = r.GerritSHA
		}
	}
	return m
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
	// Appends are pushes that add a commit with the patch set's tree on
	// top of the user's branch instead of replacing it.
	Appends map[string]Append
	New     *github.NewPull
	Update  *github.PullUpdate
	Draft   bool
	Stack   int
	PRs     []int
	Comment string
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
		for n, a := range a.Appends {
			names = append(names, n+"+=ps"+fmt.Sprint(a.PatchSet))
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

// Append exports a patch set as a commit the bridge builds: the patch
// set's tree on a GitHub-side parent. Used when GitHub's history differs
// from Gerrit's (design §2, "same trees"):
//   - a Gerrit edit to a GitHub-born change goes on top of the user's
//     branch (a fast-forward, so `git pull` works);
//   - a change whose parent is GitHub-born sits on the parent's GitHub
//     commit rather than on Gerrit's squash of it.
type Append struct {
	Change   int
	PatchSet int
	Revision string // Gerrit commit whose tree to use
	Onto     string // parent of the new commit
	Lease    string // what the branch must hold now ("" = absent)
	// KeepMessage reuses the patch set's message (a re-parented change)
	// instead of "Patch set N from Gerrit" (an edit appended to a branch).
	KeepMessage bool
	// Force pushes Revision itself instead (the patch set was rebased in
	// Gerrit, so appending would mix two bases).
	Force bool
}

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
		// No PR can be based on a fork's branch, so a change above a
		// fork PR's change is not stacked on it (it gets a frozen base).
		if pn, ok := byRev[c.Parent]; ok && pn != n && !isForkHead(s.head(pn, mirrored[pn].ChangeID)) {
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
			byHead[pr.HeadKey(s.Repo)] = pr
		}
	}
	for n, c := range s.Changes {
		if st := s.State[n]; st != nil && st.State.PR != 0 && s.PRs[st.State.PR] != nil {
			prFor[n] = s.PRs[st.State.PR]
		} else if pr := byHead[s.head(n, c.ChangeID)]; pr != nil {
			prFor[n] = pr
		}
	}

	// Desired branches and PR shape.
	refs := map[string]string{}
	appends := map[string]Append{}
	finalBase := map[int]string{} // PR -> base after this plan
	for _, n := range sortedKeys(mirrored) {
		c := mirrored[n]
		born := s.githubBorn(n)
		d := &Desired{Change: n, Head: s.head(n, c.ChangeID), Title: c.Subject, Draft: c.WIP}
		ghParent := s.ghCommit(c.Parent)
		gh, onGitHub := s.GitHubBranches[d.Head]
		synced := s.githubFor(n)
		switch ge, known := synced[gh]; {
		case onGitHub && known && ge == c.Revision:
			// Rule 1: GitHub already has this revision.
		case born && onGitHub && known && s.SameTree[n] == gh:
			// Rule 1: same tree and parent tree; record, don't push.
		case !born && ghParent == c.Parent:
			refs[d.Head] = c.Revision
		case !born:
			appends[d.Head] = Append{Change: n, PatchSet: c.PatchSet, Revision: c.Revision, Onto: ghParent,
				Lease: gh, KeepMessage: true}
		case !onGitHub || !known:
			// The user pushed something not imported yet (or not
			// importable): never overwrite their work.
		case s.NoPush[n]:
			// A fork that does not allow maintainer edits.
		default:
			appends[d.Head] = Append{Change: n, PatchSet: c.PatchSet, Revision: c.Revision, Onto: gh, Lease: gh,
				Force: !s.AppendOK[n]}
		}
		var note string
		switch {
		case parentOf[n] != 0:
			d.Base = s.head(parentOf[n], mirrored[parentOf[n]].ChangeID)
		case s.OnBranch[c.Parent]:
			d.Base = s.Branch
		default:
			// Outdated patch set of another change, an abandoned or
			// private parent: pin the exact parent so the diff matches
			// Gerrit's.
			d.Frozen = FrozenBranch(s.Branch, s.DefaultBranch, c.ChangeID)
			d.FrozenAt = ghParent
			d.Base = d.Frozen
			refs[d.Frozen] = ghParent
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
		bornTitle := ""
		if born {
			// The PR text is the user's: only the links block is ours,
			// unless the commit message was edited in Gerrit, which then
			// becomes the PR's title and body.
			user, _ := splitBody(curBody)
			title, msgBody := prTextFromMessage(c.Message)
			mh := hashString(title + "\x00" + msgBody)
			if st := s.State[n]; st != nil && st.State.GerritMessageHash != "" && st.State.GerritMessageHash != mh &&
				(pr == nil || title != pr.Title || msgBody != prBodyForMessage(curBody)) {
				user, bornTitle = msgBody, title
			}
			body = strings.TrimSpace(user + "\n\n" + block)
		}
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
		if !born && pr.Title != c.Subject && lastTitle != c.Subject {
			u.Title = ptr(c.Subject)
			changed = true
		}
		if bornTitle != "" && pr.Title != bornTitle {
			u.Title = ptr(bornTitle)
			changed = true
		}
		if pr.Body != body {
			u.Body = ptr(body)
			changed = true
		}
		if changed {
			p.Actions = append(p.Actions, Action{Kind: UpdatePR, Change: n, PR: pr.Number, Update: u})
		}
		// A GitHub-born PR's draft state belongs to its author.
		if !born && pr.Draft != c.WIP && pr.State == "open" {
			p.Actions = append(p.Actions, Action{Kind: SetDraft, Change: n, PR: pr.Number, NodeID: pr.NodeID, Draft: c.WIP})
		}
	}

	// Branch pushes, leased on what we observed.
	push := Action{Kind: PushBranches, Refs: map[string]string{}, Leases: map[string]string{}, Appends: appends}
	for name, sha := range refs {
		if s.GitHubBranches[name] != sha {
			push.Refs[name] = sha
			push.Leases[name] = s.GitHubBranches[name]
		}
	}
	for name, a := range appends {
		push.Leases[name] = a.Lease
	}
	if len(push.Refs) > 0 || len(appends) > 0 {
		p.Actions = append(p.Actions, push)
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
		if c.Status == "MERGED" && pr.Head.SHA == c.Revision && !s.OnGitHubBranch[c.Revision] {
			// The PR's head is exactly the merged commit, and replication
			// has not brought it to GitHub yet. GitHub marks a PR merged
			// when its commits land on its *base* branch, so point the PR
			// at the target branch now (leaving its stack); when they
			// land, GitHub shows it merged. (Measured 2026-10-11: a PR
			// still based on the layer below stays open, and once the
			// commits are in, it can no longer be retargeted.)
			if pr.Base.Ref != s.Branch {
				p.Actions = append(p.Actions, Action{Kind: UpdatePR, Change: n, PR: pr.Number,
					Update: &github.PullUpdate{Base: ptr(s.Branch)}})
				finalBase[pr.Number] = s.Branch
			}
			continue
		}
		var msg string
		switch {
		case c.Status == "MERGED":
			msg = fmt.Sprintf("Merged in Gerrit: [change %d](%s). Closing this PR.", n, c.URL)
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
	// GitHub refuses to change the base of a stacked PR, so a stack whose
	// members get a new base must be dissolved and rebuilt.
	retarget := map[int]bool{}
	for _, a := range p.Actions {
		if a.Kind == UpdatePR && a.Update.Base != nil {
			retarget[a.PR] = true
		}
	}
	claimed := map[int]bool{} // index into paths
	inKept := map[int]bool{}
	for _, st := range s.Stacks {
		nums := st.Numbers()
		i := slices.IndexFunc(paths, func(w []int) bool {
			return len(w) >= len(nums) && slices.Equal(w[:len(nums)], nums)
		})
		if i < 0 || claimed[i] || slices.ContainsFunc(nums, func(n int) bool { return retarget[n] }) {
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

	// Frozen bases nothing needs any more, and the bridge's branches of
	// merged changes (design §6.7). Abandoned changes keep theirs, so a
	// restore can reopen the PR.
	merged := map[string]bool{}
	for n, c := range s.Changes {
		if c.Status == "MERGED" && !s.githubBorn(n) {
			merged[HeadBranch(s.Branch, s.DefaultBranch, c.ChangeID)] = true
		}
	}
	for _, st := range s.State { // merged earlier, branch maybe left over
		if st.State.Lifecycle == "MERGED" && st.State.Origin == "gerrit" && st.State.HeadBranch != "" {
			merged[st.State.HeadBranch] = true
		}
	}
	closing := map[int]bool{}
	for _, a := range p.Actions {
		if a.Kind == ClosePR {
			closing[a.PR] = true
		}
	}
	del := Action{Kind: DeleteBranches, Refs: map[string]string{}, Leases: map[string]string{}}
	openHead := map[string]bool{} // deleting an open PR's head closes it unmerged
	for _, pr := range s.PRs {
		if pr.State == "open" && !closing[pr.Number] {
			openHead[pr.HeadKey(s.Repo)] = true
		}
	}
	for name, sha := range s.GitHubBranches {
		if openHead[name] {
			continue
		}
		if _, ok := frozenFor(name, s.Branch, s.DefaultBranch); (!ok && !merged[name]) || refs[name] != "" {
			continue
		}
		used := false
		for _, pr := range s.PRs {
			base, ok := finalBase[pr.Number]
			if !ok {
				base = pr.Base.Ref
			}
			used = used || (pr.State == "open" && !closing[pr.Number] && base == name)
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

// bridgeTrailers are the trailers the bridge adds to imported commits.
// Signed-off-by is a trailer too: the importer carries sign-offs from the
// commits, so they are not part of the PR text.
var bridgeTrailers = regexp.MustCompile(`(?m)^(Change-Id|GitHub-PR|Signed-off-by): .*$`)

// prTextFromMessage is the PR title and body a commit message stands for:
// the subject, and the rest without the bridge's trailers.
func prTextFromMessage(msg string) (title, body string) {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	title, body, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	body = strings.TrimSpace(bridgeTrailers.ReplaceAllString(body, ""))
	return strings.TrimSpace(title), body
}
