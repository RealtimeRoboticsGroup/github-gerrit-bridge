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
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// Change is everything the bridge remembers about one Gerrit change.
type Change struct {
	State     ChangeState
	Revisions []Revision
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
	// ClosedNoticePosted records that the "merged/abandoned in Gerrit"
	// comment was posted on the PR.
	ClosedNoticePosted bool `json:"closed_notice_posted,omitempty"`
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
}

// Fetch updates the mirror's refs/bridge/* from Gerrit and rebuilds the
// PR index. Call it at startup.
func (s *Store) Fetch(ctx context.Context) error {
	if _, err := s.Repo.Git(ctx, nil, "fetch", "--quiet", "--prune", "origin",
		"+"+refPrefix+"*:"+refPrefix+"*"); err != nil {
		return err
	}
	return s.rebuildIndex(ctx)
}

func (s *Store) rebuildIndex(ctx context.Context) error {
	out, err := s.Repo.Git(ctx, nil, "for-each-ref", "--format=%(refname)", refPrefix)
	if err != nil {
		return err
	}
	idx := map[int]int{}
	byBranch := map[string]map[int]bool{}
	for _, ref := range strings.Fields(string(out)) {
		var st ChangeState
		b, err := s.Repo.Git(ctx, nil, "cat-file", "blob", ref+":state.json")
		if err != nil {
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
		"threads.json":   &c.Threads,
		"comments.json":  &c.Comments,
		"votes.json":     &c.Votes,
	}
}

// Load returns the stored state of change n. A change the bridge has never
// written comes back empty, with Version "".
func (s *Store) Load(ctx context.Context, n int) (*Change, error) {
	c := &Change{}
	ver, err := s.Repo.RevParse(ctx, Ref(n)+"^{commit}")
	if err != nil || ver == "" {
		return c, err
	}
	c.Version = ver
	for name, v := range c.files() {
		b, err := s.Repo.Git(ctx, nil, "cat-file", "blob", ver+":"+name)
		if err != nil {
			var ge *gitmirror.Error
			if errors.As(err, &ge) && strings.Contains(ge.Stderr, "does not exist") {
				continue
			}
			return nil, err
		}
		if err := json.Unmarshal(b, v); err != nil {
			return nil, fmt.Errorf("%s:%s: %w", Ref(n), name, err)
		}
	}
	// Empty lists are stored as []; hand them back as nil, as Save got them.
	c.Revisions = nilIfEmpty(c.Revisions)
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
	if c.State.Change == 0 {
		c.State.Change = n
	}
	if c.State.Change != n {
		return fmt.Errorf("saving change %d state under change %d", c.State.Change, n)
	}
	tree, err := s.writeTree(ctx, c)
	if err != nil {
		return err
	}
	if c.Version != "" {
		old, err := s.Repo.RevParse(ctx, c.Version+"^{tree}")
		if err != nil {
			return err
		}
		if old == tree {
			return nil
		}
	}
	args := []string{"commit-tree", tree, "-m", msg}
	if c.Version != "" {
		args = append(args, "-p", c.Version)
	}
	out, err := s.Repo.Git(ctx, nil, args...)
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(out))

	ref := Ref(n)
	res, err := s.Repo.Push(ctx, "--force-with-lease="+ref+":"+c.Version, commit+":"+ref)
	if err != nil {
		return err
	}
	if len(res) != 1 || res[0].Ref != ref {
		return fmt.Errorf("push %s: unexpected result %+v", ref, res)
	}
	if res[0].Rejected && !isLeaseFailure(res[0].Reason) {
		return fmt.Errorf("push %s rejected: %s", ref, res[0].Reason)
	}
	if res[0].Rejected {
		// Someone else wrote it. Refresh our copy so the retry sees theirs.
		if _, err := s.Repo.Git(ctx, nil, "fetch", "--quiet", "origin", "+"+ref+":"+ref); err != nil {
			// Fetching a ref that was deleted fails; drop ours to match.
			s.Repo.Git(ctx, nil, "update-ref", "-d", ref)
		}
		return fmt.Errorf("%w: %s: %s", ErrConflict, ref, res[0].Reason)
	}
	if _, err := s.Repo.Git(ctx, nil, "update-ref", ref, commit); err != nil {
		return err
	}
	c.Version = commit
	s.mu.Lock()
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
	s.mu.Unlock()
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
func isLeaseFailure(reason string) bool {
	for _, r := range []string{"stale info", "fetch first", "non-fast-forward", "failed to lock", "lock"} {
		if strings.Contains(reason, r) {
			return true
		}
	}
	return false
}
