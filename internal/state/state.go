// Package state stores the bridge's per-change mapping in the Gerrit
// project itself, under refs/bridge/changes/<NN>/<number> (design §4.4).
//
// Each ref points at a commit whose tree holds a few JSON files. Every save
// is a new commit on top of the previous one, pushed with a lease on the
// old value, so the history is an audit log and concurrent writers cannot
// silently overwrite each other.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// Change is everything the bridge remembers about one Gerrit change.
type Change struct {
	State     ChangeState
	Revisions []Revision
	Heads     []Head
	Threads   []Thread
	Comments  []Comment
	Votes     []Vote

	// Version is the state commit this was loaded from; "" for a change
	// with no state yet. Save uses it as the lease.
	Version string
}

// ChangeState links a change to its pull request (state.json).
type ChangeState struct {
	Change   int    `json:"change"`
	ChangeID string `json:"change_id"`
	Branch   string `json:"branch"`
	// Origin is where the change was born: "gerrit" or "github".
	Origin string `json:"origin"`

	PR         int    `json:"pr,omitempty"`
	PRNodeID   string `json:"pr_node_id,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	// BaseBranch is the PR's base: the target branch, the head branch of
	// the change below in the stack, or a frozen base (design §5.4).
	BaseBranch string `json:"base_branch,omitempty"`
	Stack      int    `json:"stack,omitempty"`
	// Lifecycle mirrors the Gerrit status: NEW, ABANDONED, MERGED.
	Lifecycle string `json:"lifecycle,omitempty"`
	// Title is the PR title the bridge last wrote, and BodyHash the hash
	// of the commit-message part of the body. They tell a commit message
	// edit (rewrite) from a GitHub edit (keep).
	Title    string `json:"title,omitempty"`
	BodyHash string `json:"body_hash,omitempty"`
	// ImportHash identifies the PR title/body/draft state last imported
	// for a GitHub-born change; a change re-imports the message.
	ImportHash string `json:"import_hash,omitempty"`
	// GerritMessageHash is the hash of the Gerrit commit message last
	// synced to a GitHub-born PR's title and body.
	GerritMessageHash string `json:"gerrit_message_hash,omitempty"`
	// SyncedText is the hash of the PR title and description (minus the
	// bridge's block) when they last matched the commit message, and
	// SyncedWIP the draft/WIP state ("wip" or "ready") when it last
	// matched: whichever side has changed since is newer (design §6.0).
	SyncedText string `json:"synced_text,omitempty"`
	SyncedWIP  string `json:"synced_wip,omitempty"`
	// ClosedNoticePosted records that the "merged/abandoned in Gerrit"
	// comment was posted on the PR.
	ClosedNoticePosted bool `json:"closed_notice_posted,omitempty"`
	// WithheldNotice is the patch set whose "not mirrored: changes
	// .github/" notice was posted in Gerrit.
	WithheldNotice int `json:"withheld_notice,omitempty"`
	// ForeignNotice is the patch set whose "not pushed to the author's
	// branch" notice was posted in Gerrit.
	ForeignNotice int `json:"foreign_notice,omitempty"`
	// HandledCommands are the GitHub comments whose commands (/submit)
	// the bridge has run or answered.
	HandledCommands []int64 `json:"handled_commands,omitempty"`
}

// Revision links a patch set to the GitHub commit with the same diff
// (revisions.json).
type Revision struct {
	PatchSet  int    `json:"patch_set"`
	GerritSHA string `json:"gerrit_sha"`
	GitHubSHA string `json:"github_sha"`
	// Tree and ParentTree identify the diff (design §5.4, rule 1).
	Tree       string `json:"tree"`
	ParentTree string `json:"parent_tree"`
	// At is when the bridge recorded it on both sides: GitHub had it by
	// then (zero in state written before this was kept).
	At time.Time `json:"at,omitzero"`
}

// Head is a commit the PR's branch was seen at (heads.json): from At,
// when the bridge first saw it there or put it there, until the next
// one. Unlike Revisions, which record a commit once, a branch moved back
// to an earlier commit is a new Head.
type Head struct {
	SHA string    `json:"sha"`
	At  time.Time `json:"at"`
}

// Thread links a Gerrit comment thread to a GitHub review thread
// (threads.json).
type Thread struct {
	GerritRoot   string `json:"gerrit_root"`
	GitHubThread string `json:"github_thread,omitempty"` // GraphQL node id
	GitHubRootID int64  `json:"github_root_id"`
	// Resolved is the state both sides last agreed on; a side that
	// differs from it has changed.
	Resolved bool `json:"resolved"`
}

// Comment links one comment on each side (comments.json).
type Comment struct {
	GerritID string `json:"gerrit_id"`
	GitHubID int64  `json:"github_id"`
	// Kind is "review_comment", "issue_comment" or "review_body".
	Kind string `json:"kind"`
	// Origin is the side the comment was written on.
	Origin   string `json:"origin"`
	BodyHash string `json:"body_hash"`
}

// Vote links a Gerrit vote to the GitHub review that mirrors it
// (votes.json).
type Vote struct {
	Account        int    `json:"account"`
	Label          string `json:"label"`
	Value          int    `json:"value"`
	GitHubReviewID int64  `json:"github_review_id"`
}

// ErrConflict means the ref moved since the change was loaded. Reload and
// redo the reconcile.
var ErrConflict = errors.New("state changed concurrently")

// Ref is the state ref for change n, sharded like NoteDb.
func Ref(n int) string {
	return fmt.Sprintf("refs/bridge/changes/%02d/%d", n%100, n)
}

const refPrefix = "refs/bridge/changes/"

// Store reads and writes change state through a local mirror.
//
// Reads come from the mirror and never touch the network. The bridge is
// the only writer, so the mirror is current except after a failed push or a
// second bridge instance; both surface as ErrConflict on the next Save,
// which refetches the ref.
type Store struct {
	Repo *gitmirror.Repo

	mu       sync.Mutex
	byPR     map[int]int             // PR number -> change number
	byBranch map[string]map[int]bool // target branch -> changes with state
	// refs is each change's state commit, once the index is built; the
	// store moves it with every write, so Load needs no git command to
	// find it. Nil before Fetch: Load asks git.
	refs map[int]string
	// files caches the state files of each change's current state commit
	// (commits never change); a commit's entry goes when its change moves
	// on, so the cache holds one version per change.
	files map[string]map[string][]byte
	// filesBytes is the size of what files holds, bounded by
	// maxCachedState.
	filesBytes int
}

// maxCachedState bounds the state file cache, in bytes (a var for
// tests). Past it the cache starts over: forgetting costs a git command.
var maxCachedState = 64 << 20

// head is change n's state commit ("" for none).
func (s *Store) head(ctx context.Context, n int) (string, error) {
	s.mu.Lock()
	refs := s.refs
	ver, ok := refs[n]
	s.mu.Unlock()
	if refs != nil {
		if ok {
			return ver, nil
		}
		return "", nil
	}
	return s.Repo.RevParse(ctx, Ref(n)+"^{commit}")
}

// setHead records change n's state commit ("" = none).
func (s *Store) setHead(n int, commit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs == nil || n < 0 {
		return
	}
	if old, ok := s.refs[n]; ok && old != commit {
		for _, b := range s.files[old] {
			s.filesBytes -= len(b)
		}
		delete(s.files, old)
	}
	if commit == "" {
		delete(s.refs, n)
	} else {
		s.refs[n] = commit
	}
}

// file returns name in state commit ver (nil, false if absent), from the
// cache if read before.
func (s *Store) file(ctx context.Context, ver, name string) ([]byte, bool, error) {
	s.mu.Lock()
	b, cached := s.files[ver][name]
	s.mu.Unlock()
	if cached {
		return b, b != nil, nil
	}
	b, err := s.Repo.Git(ctx, nil, "cat-file", "blob", ver+":"+name)
	if err != nil {
		var ge *gitmirror.Error
		if !errors.As(err, &ge) || !strings.Contains(ge.Stderr, "does not exist") {
			return nil, false, err
		}
		b = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isHead(ver) {
		return b, b != nil, nil // only current state is cached
	}
	if s.files == nil || s.filesBytes+len(b) > maxCachedState {
		s.files, s.filesBytes = map[string]map[string][]byte{}, 0
	}
	s.filesBytes += len(b)
	if s.files[ver] == nil {
		s.files[ver] = map[string][]byte{}
	}
	s.files[ver][name] = b
	return b, b != nil, nil
}

// isHead reports whether ver is some change's current state commit, as
// far as the index knows (never, before it is built). s.mu is held.
func (s *Store) isHead(ver string) bool {
	if s.refs == nil {
		return false
	}
	for _, h := range s.refs {
		if h == ver {
			return true
		}
	}
	return false
}

// Fetch updates the mirror's refs/bridge/* from Gerrit and rebuilds the
// PR index. Call it at startup.
func (s *Store) Fetch(ctx context.Context) error {
	if _, err := s.Repo.Git(ctx, nil, "fetch", "--quiet", "--prune", "origin",
		"+"+refPrefix+"*:"+refPrefix+"*", "+"+IdentitiesRef+"*:"+IdentitiesRef+"*"); err != nil {
		return err
	}
	return s.rebuildIndex(ctx)
}

func (s *Store) rebuildIndex(ctx context.Context) error {
	out, err := s.Repo.Git(ctx, nil, "for-each-ref", "--format=%(objectname) %(refname)", refPrefix)
	if err != nil {
		return err
	}
	idx := map[int]int{}
	byBranch := map[string]map[int]bool{}
	refs := map[int]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		commit, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(ref[strings.LastIndexByte(ref, '/')+1:]); err == nil {
			refs[n] = commit
		}
		var st ChangeState
		b, ok, err := s.file(ctx, commit, "state.json")
		if err != nil {
			return err
		}
		if !ok {
			continue // A state commit without state.json links nothing.
		}
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("%s: state.json: %w", ref, err)
		}
		if st.PR != 0 {
			idx[st.PR] = st.Change
		}
		if byBranch[st.Branch] == nil {
			byBranch[st.Branch] = map[int]bool{}
		}
		byBranch[st.Branch][st.Change] = true
	}
	s.mu.Lock()
	s.byPR = idx
	s.byBranch = byBranch
	s.refs = refs
	s.mu.Unlock()
	return nil
}

// ChangesOnBranch returns the changes with state whose target is branch.
func (s *Store) ChangesOnBranch(branch string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var nums []int
	for n := range s.byBranch[branch] {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

// Branches returns every target branch that has state.
func (s *Store) Branches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bs []string
	for b, m := range s.byBranch {
		if len(m) > 0 {
			bs = append(bs, b)
		}
	}
	sort.Strings(bs)
	return bs
}

// ChangeForPR returns the change linked to pr, or 0.
func (s *Store) ChangeForPR(pr int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPR[pr]
}

// files maps each file in the state tree to its field.
func (c *Change) files() map[string]any {
	return map[string]any{
		"state.json":     &c.State,
		"revisions.json": &c.Revisions,
		"heads.json":     &c.Heads,
		"threads.json":   &c.Threads,
		"comments.json":  &c.Comments,
		"votes.json":     &c.Votes,
	}
}

// Load returns the stored state of change n. A change the bridge has never
// written comes back empty, with Version "".
func (s *Store) Load(ctx context.Context, n int) (*Change, error) {
	c := &Change{}
	ver, err := s.head(ctx, n)
	if err != nil || ver == "" {
		return c, err
	}
	c.Version = ver
	for name, v := range c.files() {
		b, ok, err := s.file(ctx, ver, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := json.Unmarshal(b, v); err != nil {
			return nil, fmt.Errorf("%s:%s: %w", Ref(n), name, err)
		}
	}
	// Empty lists are stored as []; hand them back as nil, as Save got them.
	c.Revisions = nilIfEmpty(c.Revisions)
	c.Heads = nilIfEmpty(c.Heads)
	c.Threads = nilIfEmpty(c.Threads)
	c.Comments = nilIfEmpty(c.Comments)
	c.Votes = nilIfEmpty(c.Votes)
	return c, nil
}

// Save writes c as a new state commit for change n with message msg, if
// the ref is still at c.Version. On success c.Version is updated. If the
// ref moved, the mirror is refreshed and ErrConflict returned. Saving a
// state identical to the stored one does nothing.
func (s *Store) Save(ctx context.Context, n int, c *Change, msg string) error {
	commit, err := s.commit(ctx, n, c, msg)
	if err != nil || commit == "" {
		return err
	}
	if err := s.pushRef(ctx, Ref(n), c.Version, commit); err != nil {
		return err
	}
	s.saved(n, c, commit)
	return nil
}

// commit makes the commit that saves c as change n's state on top of its
// version, "" if nothing changed.
func (s *Store) commit(ctx context.Context, n int, c *Change, msg string) (string, error) {
	if c.State.Change == 0 {
		c.State.Change = n
	}
	if c.State.Change != n {
		return "", fmt.Errorf("saving change %d state under change %d", c.State.Change, n)
	}
	tree, err := s.writeTree(ctx, c)
	if err != nil {
		return "", err
	}
	if c.Version != "" {
		old, err := s.Repo.RevParse(ctx, c.Version+"^{tree}")
		if err != nil {
			return "", err
		}
		if old == tree {
			return "", nil
		}
	}
	args := []string{"commit-tree", tree, "-m", msg}
	if c.Version != "" {
		args = append(args, "-p", c.Version)
	}
	out, err := s.Repo.Git(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// saved records that commit, pushed, is change n's state c.
func (s *Store) saved(n int, c *Change, commit string) {
	c.Version = commit
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byPR == nil {
		s.byPR = map[int]int{}
	}
	for pr, ch := range s.byPR {
		if ch == n && pr != c.State.PR {
			delete(s.byPR, pr)
		}
	}
	if c.State.PR != 0 {
		s.byPR[c.State.PR] = n
	}
	if s.byBranch == nil {
		s.byBranch = map[string]map[int]bool{}
	}
	for _, m := range s.byBranch {
		delete(m, n)
	}
	if s.byBranch[c.State.Branch] == nil {
		s.byBranch[c.State.Branch] = map[int]bool{}
	}
	s.byBranch[c.State.Branch][n] = true
}

// SaveAll is Save for several changes, in one push: each push to Gerrit
// has it send every ref the project has first, which on a big project
// takes longer than the rest. Atomic: either every change's state moves
// or none does, each only from the version it was loaded at; if any was
// written meanwhile, it is ErrConflict and none moved (the written ones
// are refreshed, as for Save).
func (s *Store) SaveAll(ctx context.Context, cs map[int]*Change, msg func(n int) string) error {
	var args []string
	commits := map[int]string{}
	nums := make([]int, 0, len(cs))
	for n := range cs {
		nums = append(nums, n)
	}
	slices.Sort(nums)
	for _, n := range nums {
		commit, err := s.commit(ctx, n, cs[n], msg(n))
		if err != nil {
			return err
		}
		if commit != "" {
			commits[n] = commit
			args = append(args, "--force-with-lease="+Ref(n)+":"+cs[n].Version, commit+":"+Ref(n))
		}
	}
	switch len(commits) {
	case 0:
		return nil
	case 1:
		for n, commit := range commits {
			if err := s.pushRef(ctx, Ref(n), cs[n].Version, commit); err != nil {
				return err
			}
			s.saved(n, cs[n], commit)
		}
		return nil
	}
	res, err := s.Repo.Push(ctx, append([]string{"--atomic"}, args...)...)
	if err != nil {
		return err
	}
	var refs []string
	for _, n := range nums {
		if commits[n] != "" {
			refs = append(refs, Ref(n))
		}
	}
	conflicts, err := leaseConflicts(refs, res)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		for _, ref := range conflicts {
			n, _ := strconv.Atoi(ref[strings.LastIndexByte(ref, '/')+1:])
			if err := s.refresh(ctx, ref, n); err != nil {
				return err
			}
		}
		return fmt.Errorf("%w: %s", ErrConflict, strings.Join(conflicts, ", "))
	}
	for _, r := range res {
		if r.Rejected {
			return fmt.Errorf("push %s rejected: %s", r.Ref, r.Reason)
		}
	}
	for _, n := range nums {
		if commit := commits[n]; commit != "" {
			if _, err := s.Repo.Git(ctx, nil, "update-ref", Ref(n), commit); err != nil {
				return err
			}
			s.setHead(n, commit)
			s.saved(n, cs[n], commit)
		}
	}
	return nil
}

// pushRef moves ref from old ("" = absent) to commit in Gerrit and the
// mirror. If the ref moved meanwhile, the mirror is refreshed and
// ErrConflict returned.
func (s *Store) pushRef(ctx context.Context, ref, old, commit string) error {
	res, err := s.Repo.Push(ctx, "--force-with-lease="+ref+":"+old, commit+":"+ref)
	if err != nil {
		return err
	}
	if len(res) != 1 || res[0].Ref != ref {
		return fmt.Errorf("push %s: unexpected result %+v", ref, res)
	}
	if res[0].Rejected && !isLeaseFailure(res[0].Reason) {
		return fmt.Errorf("push %s rejected: %s", ref, res[0].Reason)
	}
	// Change refs are tracked in refs (not identities).
	n := -1
	if strings.HasPrefix(ref, refPrefix) {
		n, _ = strconv.Atoi(ref[strings.LastIndexByte(ref, '/')+1:])
	}
	if res[0].Rejected {
		if err := s.refresh(ctx, ref, n); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s: %s", ErrConflict, ref, res[0].Reason)
	}
	if _, err = s.Repo.Git(ctx, nil, "update-ref", ref, commit); err != nil {
		return err
	}
	s.setHead(n, commit)
	return nil
}

// refresh fetches ref (change n's state, or -1), which someone else
// wrote, so a retry sees theirs.
func (s *Store) refresh(ctx context.Context, ref string, n int) error {
	if _, err := s.Repo.Git(ctx, nil, "fetch", "--quiet", "origin", "+"+ref+":"+ref); err != nil {
		// Fetching a ref that was deleted fails; drop ours to match.
		// Any other failure leaves ours alone: an empty state would
		// look like nothing was ever mirrored.
		out, lerr := s.Repo.Git(ctx, nil, "ls-remote", "origin", ref)
		if lerr != nil || len(bytes.TrimSpace(out)) != 0 {
			return fmt.Errorf("refreshing %s after a conflict: %w", ref, err)
		}
		s.Repo.Git(ctx, nil, "update-ref", "-d", ref)
	}
	theirs, _ := s.Repo.RevParse(ctx, ref+"^{commit}")
	s.setHead(n, theirs)
	return nil
}

// writeTree stores c's files as blobs and returns the tree id. Output is
// deterministic: indented JSON with a trailing newline, so the state
// commits diff well.
func (s *Store) writeTree(ctx context.Context, c *Change) (string, error) {
	files := c.files()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var mktree bytes.Buffer
	for _, name := range names {
		b, err := json.MarshalIndent(files[name], "", "  ")
		if err != nil {
			return "", err
		}
		if string(b) == "null" {
			b = []byte("[]")
		}
		b = append(b, '\n')
		out, err := s.Repo.Git(ctx, b, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&mktree, "100644 blob %s\t%s\n", strings.TrimSpace(string(out)), name)
	}
	out, err := s.Repo.Git(ctx, mktree.Bytes(), "mktree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Changes lists the change numbers that have state.
func (s *Store) Changes(ctx context.Context) ([]int, error) {
	out, err := s.Repo.Git(ctx, nil, "for-each-ref", "--format=%(refname)", refPrefix)
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, ref := range strings.Fields(string(out)) {
		n, err := strconv.Atoi(ref[strings.LastIndexByte(ref, '/')+1:])
		if err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums, nil
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// isLeaseFailure reports whether a push rejection means the ref moved
// (the lease failed), as opposed to a permission or validation error.
// leaseConflicts are the refs of an atomic push whose leases failed. The
// others are then rejected only as the transaction's ("atomic push
// failed", or Gerrit's "transaction aborted"), whichever comes first in
// the results: anything else is an error (ninth audit: a sibling's
// "transaction aborted" hid the conflict, so nothing fell back).
func leaseConflicts(refs []string, res []gitmirror.PushResult) ([]string, error) {
	byRef := map[string]gitmirror.PushResult{}
	for _, r := range res {
		byRef[r.Ref] = r
	}
	var conflicts []string
	var other error
	for _, ref := range refs {
		r, ok := byRef[ref]
		switch {
		case !ok:
			return nil, fmt.Errorf("push %s: no result in %+v", ref, res)
		case !r.Rejected:
		case isLeaseFailure(r.Reason):
			conflicts = append(conflicts, ref)
		case strings.Contains(r.Reason, "atomic") || strings.Contains(r.Reason, "aborted"):
		case other == nil:
			other = fmt.Errorf("push %s rejected: %s", ref, r.Reason)
		}
	}
	if other != nil {
		return nil, other
	}
	return conflicts, nil
}

func isLeaseFailure(reason string) bool {
	for _, r := range []string{"stale info", "fetch first", "non-fast-forward", "failed to lock", "lock"} {
		if strings.Contains(reason, r) {
			return true
		}
	}
	return false
}
