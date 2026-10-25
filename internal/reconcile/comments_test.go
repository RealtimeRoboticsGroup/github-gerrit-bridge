package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

const bridgeID = 99

type fakeGerrit struct {
	// onList, if set, runs whenever comments are listed.
	onList func()
	// noFixes makes SetReview fail like a site without the fix
	// suggestions feature flag.
	noFixes bool
	// voted is when each "label/user" vote was given.
	voted map[string]string
	// votes records labels set by SetReview, as "user:Label=value";
	// labels holds the resulting votes, label -> user -> value.
	votes       []string
	labels      map[string]map[string]int
	refuseRunAs bool
	// refuseReview makes SetReview as anyone but the bridge refuse (400)
	// everything, as over a change's message limits.
	refuseReview bool
	// refuse makes SetReview refuse (400) comments containing it.
	refuse string
	// closed makes SetReview refuse (409) votes, as on a closed change.
	closed   bool
	attempts int // SetReview calls, refused or not
	maxVote  int
	messages []gerrit.ChangeMessage
	comments []gerrit.CommentInfo
	revs     map[string]int // sha -> ps
	// created is when each patch set was uploaded ("" if not said).
	created map[int]string
	current string
	files   map[string]string
	clock   int
	nextID  int
	reviews int
}

// vote sets user's vote on the current patch set, as Gerrit would.
func (f *fakeGerrit) vote(user, label string, v int) {
	if f.labels == nil {
		f.labels, f.voted = map[string]map[string]int{}, map[string]string{}
	}
	if f.labels[label] == nil {
		f.labels[label] = map[string]int{}
	}
	f.labels[label][user] = v
	f.voted[label+"/"+user] = f.tick()
}

// now is the fake's clock, as a time: after the last tick.
func (f *fakeGerrit) now() time.Time {
	return time.Date(2026, 10, 4, 0, 0, f.clock, 500000000, time.UTC)
}

func (f *fakeGerrit) tick() string {
	f.clock++
	return fmt.Sprintf("2026-10-04 00:00:%02d.000000000", f.clock)
}

func (f *fakeGerrit) ListComments(context.Context, int) ([]gerrit.CommentInfo, error) {
	if f.onList != nil {
		f.onList()
	}
	return append([]gerrit.CommentInfo(nil), f.comments...), nil
}

func (f *fakeGerrit) SetReview(ctx context.Context, n int, rev string, in gerrit.ReviewInput) error {
	return f.setReview(ctx, gerrit.AccountInfo{AccountID: bridgeID, Name: "GitHub Bridge"}, n, rev, in)
}

func (f *fakeGerrit) DeleteVote(ctx context.Context, n int, account, label string) error {
	return f.deleteVote(gerrit.AccountInfo{AccountID: bridgeID}, account, label)
}

func (f *fakeGerrit) deleteVote(author gerrit.AccountInfo, account, label string) error {
	if account == "self" {
		account = author.Username
	}
	if f.labels[label][account] == 0 {
		return &gerrit.Error{Status: 404, Body: "Not found"}
	}
	f.votes = append(f.votes, fmt.Sprintf("%s:%s deleted", account, label))
	f.vote(account, label, 0)
	return nil
}

// fakeAs is the fake Gerrit acting as another user (Run-As).
type fakeAs struct {
	*fakeGerrit
	user gerrit.AccountInfo
}

func (a fakeAs) DeleteVote(ctx context.Context, n int, account, label string) error {
	if a.refuseRunAs {
		return &gerrit.Error{Status: 403, Body: "not permitted to run as"}
	}
	return a.deleteVote(a.user, account, label)
}

func (a fakeAs) SetReview(ctx context.Context, n int, rev string, in gerrit.ReviewInput) error {
	if a.refuseReview {
		return &gerrit.Error{Status: 400, Body: "too many messages"}
	}
	if a.refuseRunAs {
		return &gerrit.Error{Status: 403, Body: "not permitted to run as"}
	}
	if in.Labels["Code-Review"] > a.maxVote {
		return &gerrit.Error{Status: 403, Body: "Applying label \"Code-Review\": 2 is restricted"}
	}
	return a.setReview(ctx, a.user, n, rev, in)
}

func (f *fakeGerrit) setReview(_ context.Context, author gerrit.AccountInfo, _ int, rev string, in gerrit.ReviewInput) error {
	f.attempts++
	if f.noFixes && hasFixes(in.Comments) {
		return &gerrit.Error{Status: 500, Body: "Internal server error"}
	}
	for _, cs := range in.Comments {
		for _, c := range cs {
			if len(c.Message) > 16384 {
				return &gerrit.Error{Status: 400, Body: fmt.Sprintf("Comment size exceeds limit (%d > 16384)", len(c.Message))}
			}
			if f.refuse != "" && strings.Contains(c.Message, f.refuse) {
				return &gerrit.Error{Status: 400, Body: "Exceeding maximum number of comments"}
			}
		}
	}
	if f.closed && len(in.Labels) > 0 {
		return &gerrit.Error{Status: 409, Body: "change is closed"}
	}
	f.reviews++
	for l, v := range in.Labels {
		f.votes = append(f.votes, fmt.Sprintf("%s:%s=%d", author.Username, l, v))
		f.vote(author.Username, l, v)
	}
	ts := f.tick()
	var ps int
	fmt.Sscan(rev, &ps)
	if in.Message != "" || len(in.Labels) > 0 {
		// Gerrit's change message for the review: its votes, then its text.
		head := fmt.Sprintf("Patch Set %d:", ps)
		for _, l := range slices.Sorted(maps.Keys(in.Labels)) {
			if v := in.Labels[l]; v == 0 {
				head += " -" + l
			} else {
				head += fmt.Sprintf(" %s%+d", l, v)
			}
		}
		f.nextID++
		f.messages = append(f.messages, gerrit.ChangeMessage{ID: fmt.Sprintf("bm%d", f.nextID), Author: author, Tag: in.Tag, Date: ts,
			RevisionNumber: ps, Message: strings.TrimSpace(head + "\n\n" + in.Message)})
	}
	for path, cs := range in.Comments {
		for _, ci := range cs {
			f.nextID++
			line := ci.Line
			if ci.Range != nil {
				line = ci.Range.EndLine
			}
			f.comments = append(f.comments, gerrit.CommentInfo{
				ID: fmt.Sprintf("b%d", f.nextID), Path: path, PatchSet: ps, Side: ci.Side, Line: line, Range: ci.Range,
				InReplyTo: ci.InReplyTo, Message: ci.Message, Updated: ts,
				Author: author, Tag: in.Tag, Unresolved: ci.Unresolved != nil && *ci.Unresolved,
				FixSuggestions: ci.FixSuggestions,
			})
		}
	}
	return nil
}

func (f *fakeGerrit) GetChange(context.Context, int, ...string) (*gerrit.ChangeInfo, error) {
	ci := &gerrit.ChangeInfo{CurrentRevision: f.current, Revisions: map[string]gerrit.RevisionInfo{}, Messages: f.messages,
		Labels: map[string]gerrit.LabelInfo{}}
	for label, byUser := range f.labels {
		var l gerrit.LabelInfo
		for _, u := range slices.Sorted(maps.Keys(byUser)) {
			l.All = append(l.All, gerrit.ApprovalInfo{AccountInfo: gerrit.AccountInfo{Username: u}, Value: byUser[u], Date: f.voted[label+"/"+u]})
		}
		ci.Labels[label] = l
	}
	for sha, ps := range f.revs {
		ci.Revisions[sha] = gerrit.RevisionInfo{Number: ps, Created: f.created[ps]}
	}
	return ci, nil
}

func (f *fakeGerrit) FileContent(_ context.Context, _ int, rev string, path string) ([]byte, error) {
	if c, ok := f.files[rev+"/"+path]; ok {
		return []byte(c), nil
	}
	return []byte(f.files[path]), nil
}

func (f *fakeGerrit) ChangeURL(project string, change int) string {
	return fmt.Sprintf("https://g/c/%s/+/%d", project, change)
}

func (f *fakeGerrit) CommentURL(project string, change int, id string) string {
	return fmt.Sprintf("https://g/c/%s/+/%d/comment/%s/", project, change, id)
}

// user adds a human Gerrit comment.
func (f *fakeGerrit) user(id, path string, ps, line int, msg, inReplyTo string, unresolved bool) {
	f.comments = append(f.comments, gerrit.CommentInfo{
		ID: id, Path: path, PatchSet: ps, Line: line, Message: msg, InReplyTo: inReplyTo,
		Updated: f.tick(), Unresolved: unresolved, Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"},
	})
}

type fakeGitHub struct {
	// issueListings counts ListIssueComments calls.
	issueListings int
	// refuse makes CreateIssueComment refuse (422) bodies containing it.
	refuse string
	// pushes is the activity log of the PR's branch, newest first.
	pushes []github.Push
	edits  map[int64]string // edited comment id -> editor
	// reviewEdits is edits for review bodies.
	reviewEdits map[int64]string
	// dismissals are the PR's dismissal events, by review; onDismissals
	// runs when they are read.
	dismissals   map[int64]github.Dismissal
	onDismissals func()
	// dismissed records DismissReview calls, as "id: message".
	dismissed []string
	// afterReview runs once, when the bridge reads back a review it posted.
	afterReview func()
	rcs         []github.ReviewComment
	ics         []github.IssueComment
	reviews     []github.Review
	// head is the PR's head ("sha2" if unset).
	head       string
	resolved   map[int64]bool        // thread root -> resolved
	resolvedBy map[int64]github.User // thread root -> who resolved it
	// diff lines per path that accept line comments; diffAt overrides it
	// per "commit/path".
	diff     map[string]map[int]bool
	diffAt   map[string]map[int]bool
	files    map[string]string // "commit/path" -> content
	ancestor map[string]bool   // "a..b"
	nextID   int64
	posts    int
	// as is who posts now (fakeUserClient), if not the bridge.
	as github.User
	// refuseVote, if set, is GitHub's answer to an approval or change
	// request (on one's own PR, say).
	refuseVote error
	// prCommits, if set, are the PR's commits: a review of another is
	// refused, as GitHub does. voteTries counts approvals and change
	// requests asked for.
	prCommits map[string]bool
	voteTries int
	// refuseUserResolve, if set, is GitHub's answer to a linked user
	// resolving a thread.
	refuseUserResolve error
}

func (f *fakeGitHub) id() int64 { f.nextID++; return 1000 + f.nextID }

// who posts: the user a fakeUserClient posts as, else the bridge.
func (f *fakeGitHub) who() github.User {
	if f.as.Login != "" {
		return f.as
	}
	return github.User{Login: "bridge"}
}

// fakeUserClient posts on f as a linked user; refuse, if set, is what
// GitHub answers instead.
type fakeUserClient struct {
	f      *fakeGitHub
	as     github.User
	refuse error
	calls  *int
}

func (u fakeUserClient) do(f func()) error {
	*u.calls++
	if u.refuse != nil {
		return u.refuse
	}
	u.f.as = u.as
	defer func() { u.f.as = github.User{} }()
	f()
	return nil
}

func (u fakeUserClient) CreateReview(ctx context.Context, n int, r github.NewReview) (out *github.Review, err error) {
	if e := u.do(func() { out, err = u.f.CreateReview(ctx, n, r) }); e != nil {
		return nil, e
	}
	return out, err
}
func (u fakeUserClient) CreateReviewComment(ctx context.Context, n int, rc github.NewReviewComment) (out *github.ReviewComment, err error) {
	if e := u.do(func() { out, err = u.f.CreateReviewComment(ctx, n, rc) }); e != nil {
		return nil, e
	}
	return out, err
}
func (u fakeUserClient) ReplyToReviewComment(ctx context.Context, n int, id int64, body string) (out *github.ReviewComment, err error) {
	if e := u.do(func() { out, err = u.f.ReplyToReviewComment(ctx, n, id, body) }); e != nil {
		return nil, e
	}
	return out, err
}
func (u fakeUserClient) CreateIssueComment(ctx context.Context, n int, body string) (out *github.IssueComment, err error) {
	if e := u.do(func() { out, err = u.f.CreateIssueComment(ctx, n, body) }); e != nil {
		return nil, e
	}
	return out, err
}
func (u fakeUserClient) SetThreadResolved(ctx context.Context, id string, resolved bool) (err error) {
	if u.f.refuseUserResolve != nil {
		*u.calls++
		return u.f.refuseUserResolve
	}
	if e := u.do(func() { err = u.f.SetThreadResolved(ctx, id, resolved) }); e != nil {
		return e
	}
	return err
}

// fakeAuthors links Gerrit accounts to GitHub accounts.
type fakeAuthors struct {
	f      *fakeGitHub
	linked map[int]github.User
	refuse error // what GitHub answers their tokens
	err    error // renewing a token fails
	calls  int
}

func (a *fakeAuthors) Linked(account int) (github.User, bool) {
	u, ok := a.linked[account]
	return u, ok
}

func (a *fakeAuthors) Client(_ context.Context, account int) (UserGitHub, github.User, bool, error) {
	if a.err != nil {
		return nil, github.User{}, false, a.err
	}
	u, ok := a.linked[account]
	if !ok {
		return nil, github.User{}, false, nil
	}
	return fakeUserClient{f: a.f, as: u, refuse: a.refuse, calls: &a.calls}, u, true, nil
}

func (f *fakeGitHub) lineOK(commit, path string, line int) bool {
	if d, ok := f.diffAt[commit+"/"+path]; ok {
		return d[line]
	}
	return f.diff[path][line]
}

func (f *fakeGitHub) UpdateReviewComment(_ context.Context, id int64, body string) error {
	for i := range f.rcs {
		if f.rcs[i].ID == id {
			f.rcs[i].Body = body
			f.edited(id, "bridge")
		}
	}
	return nil
}

// edited records that by last edited comment id.
func (f *fakeGitHub) edited(id int64, by string) {
	if f.edits == nil {
		f.edits = map[int64]string{}
	}
	f.edits[id] = by
}

func (f *fakeGitHub) UpdateIssueComment(_ context.Context, id int64, body string) error {
	for i := range f.ics {
		if f.ics[i].ID == id {
			f.ics[i].Body = body
			f.edited(id, "bridge")
		}
	}
	return nil
}

func (f *fakeGitHub) FileAt(_ context.Context, ref, path string) ([]byte, error) {
	c, ok := f.files[ref+"/"+path]
	if !ok {
		return nil, &github.APIError{Status: 404}
	}
	return []byte(c), nil
}

func (f *fakeGitHub) IsAncestor(_ context.Context, a, b string) (bool, error) {
	return a == b || f.ancestor[a+".."+b], nil
}

func (f *fakeGitHub) ListReviewComments(context.Context, int) ([]github.ReviewComment, error) {
	return append([]github.ReviewComment(nil), f.rcs...), nil
}
func (f *fakeGitHub) ListReviews(context.Context, int) ([]github.Review, error) {
	out := append([]github.Review(nil), f.reviews...)
	for i := range out {
		if out[i].SubmittedAt.IsZero() {
			// As for issue comments.
			out[i].SubmittedAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		}
	}
	return out, nil
}
func (f *fakeGitHub) ListIssueComments(context.Context, int) ([]github.IssueComment, error) {
	f.issueListings++
	out := append([]github.IssueComment(nil), f.ics...)
	for i := range out {
		if out[i].CreatedAt.IsZero() {
			// GitHub always says; tests that do not care get a time
			// after the fake Gerrit's patch sets.
			out[i].CreatedAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		}
	}
	return out, nil
}
func (f *fakeGitHub) Edits(context.Context, int) (map[int64]string, error) {
	out := map[int64]string{}
	for id, by := range f.edits {
		out[id] = by
	}
	return out, nil
}
func (f *fakeGitHub) ReviewEdits(context.Context, int) (map[int64]string, error) {
	out := map[int64]string{}
	for id, by := range f.reviewEdits {
		out[id] = by
	}
	return out, nil
}
func (f *fakeGitHub) GetPull(_ context.Context, n int) (*github.PullRequest, error) {
	pr := &github.PullRequest{Number: n, Head: github.Ref{Ref: "topic", SHA: cmp.Or(f.head, "sha2")}}
	pr.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{"o/r"}
	return pr, nil
}
func (f *fakeGitHub) Pushes(_ context.Context, repo, ref string) ([]github.Push, error) {
	if repo != "o/r" || ref != "refs/heads/topic" {
		return nil, fmt.Errorf("pushes to %s %s", repo, ref)
	}
	return f.pushes, nil
}

// pushed makes sha the PR's head, as seen at at.
func (e *commentsEnv) pushed(t *testing.T, sha string, at time.Time) {
	t.Helper()
	e.h.head = sha
	if err := updateStateFn(context.Background(), e.st, 5, "head", func(st *state.Change) { st.Heads = withHead(st.Heads, sha, at) }); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeGitHub) ReviewThreads(context.Context, int) ([]github.ReviewThread, error) {
	var out []github.ReviewThread
	for _, rc := range f.rcs {
		if rc.InReplyTo == 0 {
			out = append(out, github.ReviewThread{ID: fmt.Sprintf("T%d", rc.ID), RootID: rc.ID, IsResolved: f.resolved[rc.ID], ResolvedBy: f.resolvedBy[rc.ID]})
		}
	}
	return out, nil
}

var errUnprocessable = &github.APIError{Status: 422, Message: "Line could not be resolved"}

func (f *fakeGitHub) CreateReview(_ context.Context, _ int, r github.NewReview) (*github.Review, error) {
	for _, d := range r.Comments {
		if !f.lineOK(r.CommitID, d.Path, d.Line) {
			return nil, errUnprocessable
		}
	}
	if r.Event != "COMMENT" {
		f.voteTries++
	}
	if f.prCommits != nil && !f.prCommits[r.CommitID] {
		return nil, &github.APIError{Status: 422, Message: "The commitOID is not part of the pull request"}
	}
	if f.refuseVote != nil && r.Event != "COMMENT" {
		return nil, f.refuseVote
	}
	f.posts++
	rid := f.id()
	if r.Event != "COMMENT" || r.Body != "" {
		state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[r.Event]
		f.reviews = append(f.reviews, github.Review{ID: rid, User: f.who(), State: state, Body: r.Body, CommitID: r.CommitID,
			SubmittedAt: time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)})
	}
	for _, d := range r.Comments {
		id := f.id()
		f.rcs = append(f.rcs, github.ReviewComment{ID: id, ReviewID: rid, Path: d.Path, Line: d.Line, OriginalLine: d.Line,
			Side: d.Side, CommitID: r.CommitID, OriginalCommitID: r.CommitID, Body: d.Body, SubjectType: "line",
			HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#discussion_r%d", id), User: f.who()})
	}
	return &github.Review{ID: rid}, nil
}

func (f *fakeGitHub) ListReviewCommentsOf(_ context.Context, _ int, review int64) ([]github.ReviewComment, error) {
	var out []github.ReviewComment
	for _, rc := range f.rcs {
		if rc.ReviewID == review {
			out = append(out, rc)
		}
	}
	if f.afterReview != nil {
		f.afterReview() // someone acts right after the read
		f.afterReview = nil
	}
	return out, nil
}

func (f *fakeGitHub) CreateReviewComment(_ context.Context, _ int, rc github.NewReviewComment) (*github.ReviewComment, error) {
	if f.prCommits != nil && !f.prCommits[rc.CommitID] {
		return nil, &github.APIError{Status: 422, Message: "The commitOID is not part of the pull request"}
	}
	if rc.SubjectType == "file" {
		if f.diff[rc.Path] == nil {
			return nil, errUnprocessable
		}
	} else if !f.lineOK(rc.CommitID, rc.Path, rc.Line) {
		return nil, errUnprocessable
	}
	f.posts++
	id := f.id()
	st := rc.SubjectType
	if st == "" {
		st = "line"
	}
	c := github.ReviewComment{ID: id, Path: rc.Path, Line: rc.Line, OriginalLine: rc.Line, StartLine: rc.StartLine, Side: rc.Side, CommitID: rc.CommitID,
		OriginalCommitID: rc.CommitID, Body: rc.Body, SubjectType: st, User: f.who()}
	f.rcs = append(f.rcs, c)
	return &c, nil
}

func (f *fakeGitHub) ReplyToReviewComment(_ context.Context, _ int, id int64, body string) (*github.ReviewComment, error) {
	f.posts++
	var root *github.ReviewComment
	for i := range f.rcs {
		if f.rcs[i].ID == id {
			root = &f.rcs[i]
		}
	}
	if root == nil || root.InReplyTo != 0 {
		return nil, fmt.Errorf("reply to %d: not a thread root", id)
	}
	c := github.ReviewComment{ID: f.id(), InReplyTo: id, Path: root.Path, Line: root.Line, OriginalLine: root.OriginalLine,
		CommitID: root.CommitID, OriginalCommitID: root.OriginalCommitID, Body: body, User: f.who()}
	f.rcs = append(f.rcs, c)
	return &c, nil
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, _ int, body string) (*github.IssueComment, error) {
	if len(body) > 65536 || (f.refuse != "" && strings.Contains(body, f.refuse)) {
		return nil, errUnprocessable
	}
	f.posts++
	c := github.IssueComment{ID: f.id(), Body: body, User: f.who()}
	f.ics = append(f.ics, c)
	return &c, nil
}

func (f *fakeGitHub) SetThreadResolved(_ context.Context, id string, resolved bool) error {
	var root int64
	fmt.Sscanf(id, "T%d", &root)
	f.resolved[root] = resolved
	if f.resolvedBy == nil {
		f.resolvedBy = map[int64]github.User{}
	}
	f.resolvedBy[root] = f.who()
	return nil
}

// human adds a GitHub user's review comment (a root if replyTo is 0).
func (f *fakeGitHub) human(path string, line int, commit, body string, replyTo int64) int64 {
	id := f.id()
	f.rcs = append(f.rcs, github.ReviewComment{ID: id, ReviewID: id + 5000, InReplyTo: replyTo, Path: path, Line: line, OriginalLine: line,
		Side: "RIGHT", CommitID: commit, OriginalCommitID: commit, Body: body, SubjectType: "line",
		HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#discussion_r%d", id), User: github.User{Login: "bob"}})
	return id
}

type commentsEnv struct {
	g  *fakeGerrit
	h  *fakeGitHub
	c  *Comments
	st *state.Store
}

func newCommentsEnv(t *testing.T) *commentsEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	origin := &gitmirror.Repo{Dir: filepath.Join(dir, "origin.git")}
	if err := origin.Init(ctx); err != nil {
		t.Fatal(err)
	}
	mirror := &gitmirror.Repo{Dir: filepath.Join(dir, "m.git"), Remote: origin.Dir, Name: "B", Email: "b@example.com"}
	if err := mirror.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Repo: mirror}
	// Patch sets 1 and 2 reached GitHub long before anything a test posts
	// there (by default).
	early := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, 5, &state.Change{State: state.ChangeState{PR: 1, Branch: "main", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "sha1", At: early}, {PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "sha2", At: early}},
		Heads:     []state.Head{{SHA: "sha1", At: early}, {SHA: "sha2", At: early}}}, "link"); err != nil {
		t.Fatal(err)
	}
	g := &fakeGerrit{revs: map[string]int{"sha1": 1, "sha2": 2}, current: "sha2",
		files: map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"}}
	h := &fakeGitHub{resolved: map[int64]bool{}, diff: map[string]map[int]bool{"a.txt": {2: true, 3: true}},
		diffAt: map[string]map[int]bool{}, files: map[string]string{}, ancestor: map[string]bool{}}
	return &commentsEnv{g: g, h: h, st: store, c: &Comments{
		Project: "p", Gerrit: g, GitHub: h, Store: store, BridgeAccount: bridgeID, Logger: slog.Default(),
	}}
}

func (e *commentsEnv) sync(t *testing.T) {
	t.Helper()
	if err := e.c.Sync(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
}

func (e *commentsEnv) settled(t *testing.T) {
	t.Helper()
	gp, hp := e.g.reviews, e.h.posts
	e.sync(t)
	if e.g.reviews != gp || e.h.posts != hp {
		t.Errorf("a second sync posted again (gerrit %d->%d, github %d->%d)", gp, e.g.reviews, hp, e.h.posts)
	}
}

// Someone posting during each pass of a sync, on either side, cannot keep
// it from finishing (fourth audit): what is posted, or edited, after its
// first pass waits for the next sync.
func TestCommentSyncSettlesDespiteNewComments(t *testing.T) {
	for _, side := range []string{"github", "gerrit", "message", "edit"} {
		e := newCommentsEnv(t)
		e.c.BridgeLogin = "bridge"
		e.h.ics = append(e.h.ics, github.IssueComment{ID: 7, Body: "First.", User: github.User{Login: "stranger"}})
		i := 0
		e.g.onList = func() {
			i++
			switch side {
			case "github":
				e.h.ics = append(e.h.ics, github.IssueComment{ID: int64(100 + i), Body: fmt.Sprintf("Again %d.", i), User: github.User{Login: "stranger"}})
			case "gerrit":
				e.g.user(fmt.Sprintf("x%d", i), "a.txt", 2, 2, fmt.Sprintf("Again %d.", i), "", false)
			case "message":
				e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: fmt.Sprintf("m%d", i), Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice"},
					RevisionNumber: 2, Message: fmt.Sprintf("Patch Set 2:\n\nAgain %d.", i)})
			case "edit":
				e.h.ics[0].Body = fmt.Sprintf("First, edit %d.", i)
				e.h.edited(7, "stranger")
			}
		}
		if err := e.c.Sync(context.Background(), 5); err != nil {
			t.Errorf("%s: %v", side, err)
		}
	}
	// What the bridge posts in the first pass is still read back in the
	// next: a GitHub reply follows its root to Gerrit in one sync.
	e := newCommentsEnv(t)
	e.c.BridgeLogin = "bridge"
	root := e.h.human("a.txt", 3, "sha2", "Typo here.", 0)
	e.h.human("a.txt", 3, "sha2", "And here.", root)
	e.sync(t)
	if len(e.g.comments) != 2 || e.g.comments[1].InReplyTo != e.g.comments[0].ID {
		t.Errorf("Gerrit has %+v, want the root and a reply to it", e.g.comments)
	}
}

func TestGerritLineCommentsBecomeOneReview(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "Why two?", "", true)
	e.g.comments = append(e.g.comments, e.g.comments[0])
	e.g.comments[1].ID, e.g.comments[1].Line, e.g.comments[1].Message = "c2", 3, "And three."
	e.sync(t)
	if len(e.h.rcs) != 2 || e.h.rcs[0].ReviewID != e.h.rcs[1].ReviewID {
		t.Fatalf("want one review with 2 comments, got %+v", e.h.rcs)
	}
	b := e.h.rcs[0].Body
	for _, want := range []string{"**Alice Trusted (alice)**", "Why two?", "<!-- bridge:gerrit/5/c1 -->", "https://g/c/p/+/5/comment/c1/"} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %q:\n%s", want, b)
		}
	}
	if e.h.rcs[0].CommitID != "sha2" || e.h.rcs[0].Line != 2 {
		t.Errorf("anchor = %s:%d", e.h.rcs[0].CommitID, e.h.rcs[0].Line)
	}
	e.settled(t)
}

// A reply posted in Gerrit before the bridge mirrored its parent, by
// someone else (another review), goes in its thread once the parent is
// on GitHub, not on its own beside it.
func TestReplyInTheSamePassAsItsParentIsThreaded(t *testing.T) {
	for _, line := range []int{2, 5} { // in the diff; outside it, file level
		t.Run(fmt.Sprint(line), func(t *testing.T) {
			e := newCommentsEnv(t)
			e.g.user("c1", "a.txt", 2, line, "Why?", "", true)
			e.g.user("c2", "a.txt", 2, line, "Because.", "c1", false)
			e.g.comments[1].Author = gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
			e.sync(t)
			if len(e.h.rcs) != 2 || e.h.rcs[1].InReplyTo != e.h.rcs[0].ID || !strings.Contains(e.h.rcs[1].Body, "Because.") {
				t.Fatalf("got %+v", e.h.rcs)
			}
			e.settled(t)
		})
	}
}

// Gerrit ahead of a fork's branch: a comment on a patch set GitHub lacks
// goes on the latest one it has, on the line or the file.
func TestCommentOnPatchSetNotOnGitHub(t *testing.T) {
	for _, line := range []int{2, 5} { // in the diff; outside it, file level
		t.Run(fmt.Sprint(line), func(t *testing.T) {
			e := newCommentsEnv(t)
			e.h.prCommits = map[string]bool{"sha1": true, "sha2": true}
			e.g.revs["sha3"] = 3
			e.g.current = "sha3"
			e.g.user("c1", "a.txt", 3, line, "Why?", "", true)
			e.sync(t)
			if len(e.h.rcs) != 1 || e.h.rcs[0].CommitID != "sha2" || len(e.h.ics) != 0 {
				t.Fatalf("review comments %+v, PR comments %+v", e.h.rcs, e.h.ics)
			}
			if line == 2 && (e.h.rcs[0].Line != 2 || !strings.Contains(e.h.rcs[0].Body, "Ported from patch set 3")) {
				t.Errorf("not ported: %+v", e.h.rcs[0])
			}
			e.settled(t)
		})
	}
}

func TestOutOfHunkFallsBackToFileLevel(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 5, "Line five is outside the diff.", "", true)
	e.sync(t)
	if len(e.h.rcs) != 1 || e.h.rcs[0].SubjectType != "file" {
		t.Fatalf("got %+v", e.h.rcs)
	}
	if !strings.Contains(e.h.rcs[0].Body, "`a.txt` L5:\n```\nfive\n```") {
		t.Errorf("no quote of the true anchor:\n%s", e.h.rcs[0].Body)
	}
	e.settled(t)
}

func TestGitHubCommentsAndRepliesBothWays(t *testing.T) {
	e := newCommentsEnv(t)
	// Bob comments on an old patch set's commit on GitHub.
	root := e.h.human("a.txt", 3, "sha1", "Typo here.", 0)
	e.sync(t)
	var mirrored *gerrit.CommentInfo
	for i := range e.g.comments {
		if e.g.comments[i].Author.AccountID == bridgeID {
			mirrored = &e.g.comments[i]
		}
	}
	if mirrored == nil || mirrored.PatchSet != 1 || mirrored.Line != 3 || !mirrored.Unresolved {
		t.Fatalf("mirrored = %+v", mirrored)
	}
	if !strings.Contains(mirrored.Message, "**@bob**") || !strings.Contains(mirrored.Message, fmt.Sprintf("discussion_r%d", root)) {
		t.Errorf("message = %q", mirrored.Message)
	}
	e.settled(t)

	// Alice replies in Gerrit: it lands in the GitHub thread.
	e.g.user("c9", "a.txt", 1, 3, "Fixed.", mirrored.ID, true)
	e.sync(t)
	last := e.h.rcs[len(e.h.rcs)-1]
	if last.InReplyTo != root || !strings.Contains(last.Body, "Fixed.") {
		t.Errorf("reply = %+v", last)
	}
	// Bob replies on GitHub: it lands in the Gerrit thread.
	e.h.human("a.txt", 3, "sha1", "Thanks!", root)
	e.sync(t)
	gl := e.g.comments[len(e.g.comments)-1]
	if gl.InReplyTo != mirrored.ID || !strings.Contains(gl.Message, "Thanks!") {
		t.Errorf("gerrit reply = %+v", gl)
	}
	e.settled(t)
}

func TestResolutionBothWays(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "Please fix.", "", true)
	e.sync(t)
	root := e.h.rcs[0].ID
	if e.h.resolved[root] {
		t.Fatal("thread resolved too early")
	}
	// Alice marks it Done in Gerrit.
	e.g.user("c2", "a.txt", 2, 2, "Done", "c1", false)
	e.sync(t)
	if !e.h.resolved[root] {
		t.Error("Gerrit resolution not mirrored to GitHub")
	}
	e.settled(t)

	// Someone unresolves it on GitHub.
	e.h.resolved[root] = false
	e.sync(t)
	gl := e.g.comments[len(e.g.comments)-1]
	if gl.Author.AccountID != bridgeID || !gl.Unresolved || gl.Message != "Unresolved on GitHub." {
		t.Errorf("last gerrit comment = %+v", gl)
	}
	e.settled(t)
}

func TestPatchSetLevelAndIssueComments(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "Overall LGTM.", "", false)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 7, Body: "Can we land this?", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-7"})
	e.sync(t)
	if len(e.h.ics) != 2 || !strings.Contains(e.h.ics[1].Body, "Overall LGTM.") {
		t.Errorf("issue comments = %+v", e.h.ics)
	}
	found := false
	for _, gc := range e.g.comments {
		found = found || (gc.Path == gerrit.PatchSetLevel && strings.Contains(gc.Message, "Can we land this?"))
	}
	if !found {
		t.Error("issue comment not mirrored to Gerrit")
	}
	e.settled(t)
}

func TestLostStateCausesNoDuplicates(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "One.", "", true)
	e.h.human("a.txt", 3, "sha2", "Two.", 0)
	e.sync(t)
	// Wipe the comment mapping.
	ctx := context.Background()
	updateStateFn(ctx, e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.settled(t)
	st, _ := e.st.Load(ctx, 5)
	if len(st.Comments) != 2 || len(st.Threads) != 2 {
		t.Errorf("mapping not rebuilt: %+v / %+v", st.Comments, st.Threads)
	}
}

func TestOwnPostsNeverMirroredBack(t *testing.T) {
	e := newCommentsEnv(t)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 3, Body: "Abandoned in Gerrit.\n\n" + noticeMarker})
	e.g.user("c1", "a.txt", 2, 2, "One.", "", true)
	e.sync(t)
	e.sync(t)
	e.sync(t)
	n := 0
	for _, gc := range e.g.comments {
		if gc.Author.AccountID == bridgeID {
			n++
		}
	}
	if n != 0 || len(e.h.rcs) != 1 {
		t.Errorf("echo: %d bridge comments in Gerrit, %d on GitHub", n, len(e.h.rcs))
	}
}

func TestChangeMessages(t *testing.T) {
	e := newCommentsEnv(t)
	e.c.CIMessage = DefaultCIMessage
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Tag: "autogenerated:gerrit:newPatchSet", Message: "Uploaded patch set 2.", RevisionNumber: 2},
		{ID: "m2", Author: alice, Message: "Patch Set 2: Code-Review+1\n\nPosted from the CLI: please split this.", RevisionNumber: 2},
		{ID: "m3", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
		{ID: "m4", Author: alice, Message: "Patch Set 2:\n\n(1 comment)", RevisionNumber: 2},
		{ID: "m5", Author: gerrit.AccountInfo{AccountID: 3}, Message: "Patch Set 2:\n\nBuild Started: https://bk/1", RevisionNumber: 2},
		{ID: "m6", Author: gerrit.AccountInfo{AccountID: bridgeID}, Message: "Patch Set 2:\n\nGitHub: https://github.com/o/r/pull/1", RevisionNumber: 2},
	}
	e.sync(t)
	if len(e.h.ics) != 1 {
		t.Fatalf("mirrored %d messages, want only m2: %+v", len(e.h.ics), e.h.ics)
	}
	b := e.h.ics[0].Body
	for _, want := range []string{"**Alice Trusted**", "Posted from the CLI: please split this.", "<!-- bridge:gerrit/5/m2 -->", "#message-m2"} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %q:\n%s", want, b)
		}
	}
	if !strings.Contains(b, "on patch set 2, **Code-Review +1**:") || strings.Contains(b, "Patch Set 2:") {
		t.Errorf("want the vote in the header line, not Gerrit's raw header:\n%s", b)
	}
	e.settled(t)
}

func TestGerritCommentPortedToCurrentPatchSet(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.files["1/a.txt"] = "a\nb\nc\n"
	e.g.files["2/a.txt"] = "new\na\nb\nc\n"
	e.h.diffAt["sha1/a.txt"] = map[int]bool{} // PS1's lines are no longer in the PR's diff
	e.h.diffAt["sha2/a.txt"] = map[int]bool{3: true}
	e.g.user("c1", "a.txt", 1, 2, "About b.", "", true)
	e.sync(t)
	if len(e.h.rcs) != 1 {
		t.Fatalf("got %+v", e.h.rcs)
	}
	rc := e.h.rcs[0]
	if rc.CommitID != "sha2" || rc.Line != 3 || rc.SubjectType != "line" || !strings.Contains(rc.Body, "Ported from patch set 1, line 2.") {
		t.Errorf("ported comment = %s:%d %s\n%s", rc.CommitID, rc.Line, rc.SubjectType, rc.Body)
	}
	e.settled(t)
}

func TestGerritFixBecomesSuggestion(t *testing.T) {
	e := newCommentsEnv(t) // a.txt = one..five, lines 2-3 in the diff
	e.g.user("c1", "a.txt", 2, 2, "Shout it.", "", true)
	e.g.comments[0].FixSuggestions = []gerrit.FixSuggestion{{Replacements: []gerrit.FixReplacement{{
		Path: "a.txt", Range: gerrit.Range{StartLine: 2, EndLine: 3}, Replacement: "TWO\n"}}}}
	e.g.user("c2", "a.txt", 2, 3, "Partial.", "", true)
	e.g.comments[1].FixSuggestions = []gerrit.FixSuggestion{{Replacements: []gerrit.FixReplacement{{
		Path: "a.txt", Range: gerrit.Range{StartLine: 3, StartCharacter: 1, EndLine: 3, EndCharacter: 3}, Replacement: "H"}}}}
	e.sync(t)
	if len(e.h.rcs) != 2 {
		t.Fatalf("got %+v", e.h.rcs)
	}
	if b := e.h.rcs[0].Body; !strings.Contains(b, "```suggestion\nTWO\n```") || e.h.rcs[0].Line != 2 {
		t.Errorf("whole-line fix:\n%s", b)
	}
	if b := e.h.rcs[1].Body; !strings.Contains(b, "```suggestion\ntHee\n```") || e.h.rcs[1].Line != 3 {
		t.Errorf("partial-line fix:\n%s", b)
	}
}

func TestGitHubSuggestionBecomesFix(t *testing.T) {
	e := newCommentsEnv(t)
	e.h.human("a.txt", 3, "sha2", "Caps please:\n```suggestion\nTHREE\n```\nThanks.", 0)
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if len(gc.FixSuggestions) != 1 {
		t.Fatalf("no fix: %+v", gc)
	}
	r := gc.FixSuggestions[0].Replacements[0]
	if r.Path != "a.txt" || r.Range != (gerrit.Range{StartLine: 3, EndLine: 3, EndCharacter: 5}) || r.Replacement != "THREE" {
		t.Errorf("replacement = %+v", r)
	}
	if strings.Contains(gc.Message, "```suggestion") || !strings.Contains(gc.Message, "Apply fix") || !strings.Contains(gc.Message, "Thanks.") {
		t.Errorf("message = %q", gc.Message)
	}
}

// Text the bridge posts as itself is defused, wherever it comes from: an
// edit, a comment posted by the bridge because Run-As was refused (which
// also cannot pass for one of the bridge's attributed copies), and a
// suggestion's fix stays the author's exact code.
func TestBridgePostsAreDefused(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	id := e.h.human("a.txt", 3, "sha2", "first", 0)
	e.sync(t)
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == id {
			e.h.rcs[i].Body = "now cc @alice"
		}
	}
	e.h.edits = map[int64]string{id: "bob"}
	e.sync(t)
	if gc := e.g.comments[len(e.g.comments)-1]; !strings.Contains(gc.Message, "edited on GitHub") || strings.Contains(gc.Message, "@alice") {
		t.Errorf("edit posted as %q", gc.Message)
	}

	e = newCommentsEnv(t)
	e.runAs()
	e.g.refuseRunAs = true
	forged := "**@x** ([GitHub](https://github.com/o/r/pull/1#discussion_r999)): cc @alice"
	e.h.human("a.txt", 3, "sha2", forged, 0)
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.Author.AccountID != bridgeID || strings.Contains(gc.Message, "@alice") || githubRef.MatchString(gc.Message) {
		t.Errorf("fallback posted by %+v as %q", gc.Author, gc.Message)
	}
	e.settled(t)

	e = newCommentsEnv(t)
	e.h.human("a.txt", 3, "sha2", "Annotate:\n```suggestion\n@Override\n```", 0)
	e.h.rcs[len(e.h.rcs)-1].User.Login = "stranger"
	e.sync(t)
	gc = e.g.comments[len(e.g.comments)-1]
	if len(gc.FixSuggestions) != 1 || gc.FixSuggestions[0].Replacements[0].Replacement != "@Override" {
		t.Errorf("fix = %+v", gc.FixSuggestions)
	}
}

func TestGitHubEditBecomesReply(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	root := e.h.human("a.txt", 3, "sha2", "Typo.", 0)
	e.sync(t)
	n := len(e.g.comments)
	mirrored := e.g.comments[n-1].ID
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == root {
			e.h.rcs[i].Body = "Typo on this line, and the next."
		}
	}
	e.sync(t)
	if len(e.g.comments) != n+1 {
		t.Fatalf("got %d new Gerrit comments, want 1", len(e.g.comments)-n)
	}
	gc := e.g.comments[n]
	if gc.InReplyTo != mirrored || !strings.Contains(gc.Message, "comment was edited") || !strings.Contains(gc.Message, "and the next.") {
		t.Errorf("edit reply = %+v", gc)
	}
	// Anyone with write access may have made the edit: never as the author.
	if gc.Author.Username == "bobg" || !isBridgeTag(gc.Tag) {
		t.Errorf("edit posted as %q with tag %q, want the bridge", gc.Author.Username, gc.Tag)
	}
	if strings.Contains(gc.Message, "#discussion_r") {
		t.Error("edit reply would be mistaken for a mirror of a new comment")
	}
	e.settled(t)
}

func TestGerritContentRemovalUpdatesGitHub(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "Something rude.", "", true)
	e.sync(t)
	e.g.comments[0].Message = "Comment removed by: Administrator"
	e.sync(t)
	if b := e.h.rcs[0].Body; !strings.Contains(b, "Comment removed by") || strings.Contains(b, "rude") {
		t.Errorf("GitHub copy = %q", b)
	}
	e.settled(t)
}

func TestGitHubCommentOnIntermediateCommitIsPorted(t *testing.T) {
	e := newCommentsEnv(t)
	ctx := context.Background()
	updateStateFn(ctx, e.st, 5, "revs", func(s *state.Change) {
		s.Revisions = []state.Revision{{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "h1"}, {PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "h2"}}
	})
	e.h.ancestor["c0..h2"] = true // c0 is an earlier commit in the layer that became PS2
	e.h.files["c0/a.txt"] = "x\ny\n"
	e.h.files["h2/a.txt"] = "w\nx\ny\n"
	e.h.human("a.txt", 2, "c0", "About y.", 0)
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.PatchSet != 2 || gc.Line != 3 || !strings.Contains(gc.Message, "Ported from GitHub commit c0") {
		t.Errorf("ported = ps%d L%d %q", gc.PatchSet, gc.Line, gc.Message)
	}
}

func TestApplyReplacement(t *testing.T) {
	lines := strings.Split("one\ntwo\nthree\n", "\n")
	for _, tc := range []struct {
		r           gerrit.Range
		rep, want   string
		first, last int
	}{
		{gerrit.Range{StartLine: 2, EndLine: 3}, "TWO\n", "TWO\n", 2, 2},
		{gerrit.Range{StartLine: 1, EndLine: 3}, "", "", 1, 2},
		{gerrit.Range{StartLine: 2, StartCharacter: 0, EndLine: 3, EndCharacter: 2}, "X", "Xree\n", 2, 3},
	} {
		got, f, l, ok := applyReplacement(lines, gerrit.FixReplacement{Range: tc.r, Replacement: tc.rep})
		if !ok || got != tc.want || f != tc.first || l != tc.last {
			t.Errorf("%+v %q -> %q %d-%d %v, want %q %d-%d", tc.r, tc.rep, got, f, l, ok, tc.want, tc.first, tc.last)
		}
	}
}

func TestGitHubSuggestionWithoutGerritFeatureFlag(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.noFixes = true
	e.h.human("a.txt", 3, "sha2", "Caps please:\n```suggestion\nTHREE\n```", 0)
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if len(gc.FixSuggestions) != 0 || !strings.Contains(gc.Message, "```suggestion\nTHREE\n```") {
		t.Errorf("fallback comment = %+v", gc)
	}
	e.settled(t)
}

func (e *commentsEnv) runAs() {
	e.g.maxVote = 2
	e.c.Users = &Users{Overrides: map[string]string{"bob": "bobg"}}
	e.c.GerritAs = func(user string) GerritAPI {
		return fakeAs{e.g, gerrit.AccountInfo{AccountID: 7, Username: user, Name: "Bob G"}}
	}
}

func TestRunAsPostsAsTheUserAndNeverEchoes(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.human("a.txt", 3, "sha2", "retest", 0)
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.Author.Username != "bobg" || gc.Tag != mirrorTag {
		t.Fatalf("posted as %q with tag %q, want bobg / %s", gc.Author.Username, gc.Tag, mirrorTag)
	}
	// Posted as bob, so no attribution: just bob's words.
	if gc.Message != "retest" {
		t.Errorf("message = %q, want just retest", gc.Message)
	}
	// It is bob's comment in Gerrit, but the bridge's post: never mirrored back.
	posts := e.h.posts
	e.settled(t)
	if e.h.posts != posts {
		t.Error("Run-As post mirrored back to GitHub")
	}
	// Mapping rebuilds from the tag and text when state is lost.
	updateStateFn(context.Background(), e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.settled(t)
}

func TestRunAsIdenticalCommentsEachMirroredOnce(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	root := e.h.human("a.txt", 3, "sha2", "Typo.", 0)
	e.sync(t)
	n := len(e.g.comments)
	// Two separate "Done" replies in the same thread, and one whose text
	// links another GitHub comment.
	e.h.human("a.txt", 3, "sha2", "Done", root)
	e.h.human("a.txt", 3, "sha2", "Done", root)
	e.h.human("a.txt", 3, "sha2", fmt.Sprintf("**@bob** ([GitHub](https://github.com/o/r/pull/1#discussion_r%d)): see that", root), root)
	e.sync(t)
	if got := len(e.g.comments) - n; got != 3 {
		t.Fatalf("got %d new Gerrit comments, want 3", got)
	}
	e.settled(t)
	// With the map lost, each is recognised again, and nothing reposted.
	updateStateFn(context.Background(), e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.settled(t)
	st, _ := e.st.Load(context.Background(), 5)
	seen := map[string]bool{}
	for _, c := range st.Comments {
		if seen[c.GerritID] {
			t.Errorf("Gerrit comment %s mapped twice", c.GerritID)
		}
		seen[c.GerritID] = true
	}
	if len(st.Comments) != 4 {
		t.Errorf("%d comments mapped, want 4", len(st.Comments))
	}
}

func TestGerritCommentsCopyEditedOnGitHubIsRestored(t *testing.T) {
	e := newCommentsEnv(t)
	e.c.BridgeLogin = "bridge"
	e.g.user("c1", "a.txt", 2, 2, "Why two?", "", true)
	e.sync(t)
	var copyID int64
	for _, rc := range e.h.rcs {
		if strings.Contains(rc.Body, "Why two?") {
			copyID = rc.ID
		}
	}
	if copyID == 0 {
		t.Fatal("not mirrored")
	}
	// Someone with write access rewrites the bot's copy, marker and all.
	e.g.user("c2", "a.txt", 2, 3, "Unrelated.", "", true)
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == copyID {
			e.h.rcs[i].Body = "**Alice Trusted (alice)**: LGTM, ship it\n\n<!-- bridge:gerrit/5/c2 -->"
		}
	}
	e.h.edited(copyID, "mallory")
	e.sync(t)
	e.settled(t)
	for _, rc := range e.h.rcs {
		if rc.ID == copyID && (!strings.Contains(rc.Body, "Why two?") || !strings.Contains(rc.Body, "bridge:gerrit/5/c1 ") ||
			!strings.Contains(rc.Body, "Restored")) {
			t.Errorf("copy not restored: %q", rc.Body)
		}
	}
	// c2 was mirrored as a comment of its own, not taken for the forged copy.
	st, _ := e.st.Load(context.Background(), 5)
	for _, c := range st.Comments {
		if c.GerritID == "c2" && c.GitHubID == copyID {
			t.Error("forged marker remapped c2")
		}
	}
}

func TestBotCopyEditedRightAfterPosting(t *testing.T) {
	e := newCommentsEnv(t)
	e.c.BridgeLogin = "bridge"
	e.g.user("c1", "a.txt", 2, 2, "Why two?", "", true)
	// Edited before the bridge's next pass could read its marker.
	e.h.afterReview = func() {
		for i := range e.h.rcs {
			if strings.Contains(e.h.rcs[i].Body, "Why two?") {
				e.h.rcs[i].Body = "rewritten"
				e.h.edited(e.h.rcs[i].ID, "mallory")
			}
		}
	}
	e.sync(t)
	e.settled(t)
	copies := 0
	for _, rc := range e.h.rcs {
		if strings.Contains(rc.Body, "Why two?") {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("%d GitHub copies of the Gerrit comment, want 1 (restored)", copies)
	}
	if len(e.g.comments) != 1 {
		t.Errorf("the bot's edited comment was mirrored into Gerrit: %+v", e.g.comments[1:])
	}
}

func TestUnmappedUserPostsAsBridge(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 9, Body: "hi", User: github.User{Login: "stranger"},
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-9"})
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.Author.AccountID != bridgeID || gc.Tag != commentTag {
		t.Errorf("unmapped user's comment posted as %+v tag %q", gc.Author, gc.Tag)
	}
}

func TestRunAsRefusedFallsBackToBridge(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.g.refuseRunAs = true
	e.h.human("a.txt", 3, "sha2", "hello", 0)
	e.h.reviews = append(e.h.reviews, github.Review{ID: 60, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-60"})
	e.sync(t)
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.Author.AccountID != bridgeID {
		t.Errorf("posted as %+v, want the bridge after Run-As was refused", gc.Author)
	}
	if len(e.g.votes) != 0 {
		t.Errorf("votes cast without Run-As: %v", e.g.votes)
	}
}

// Anyone with write access can edit anyone's review: one last edited by
// someone else is posted by the bridge, and never votes.
func TestReviewEditedByOtherIsNotTheAuthors(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 50, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "retest", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-50"})
	e.h.reviewEdits = map[int64]string{50: "mallory"}
	e.sync(t)
	if len(e.g.votes) != 0 {
		t.Errorf("votes = %v", e.g.votes)
	}
	var found bool
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "retest") {
			found = true
			if gc.Author.AccountID != bridgeID || !strings.Contains(gc.Message, "Not counted as a vote: someone other than its author edited it.") {
				t.Errorf("posted by %+v: %q", gc.Author, gc.Message)
			}
		}
	}
	if !found {
		t.Errorf("review not mirrored: %+v", e.g.comments)
	}
}

// A review's inline comments and its body share a batch only when they
// are posted as the same person: a body someone else edited is never
// posted as the review's author along with its unedited comments.
func TestReviewBodyEditedByOtherIsNotPostedWithItsComments(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	id := e.h.human("a.txt", 3, "sha2", "nit", 0)
	rid := id + 5000
	e.h.reviews = append(e.h.reviews, github.Review{ID: rid, State: "COMMENTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "retest", HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#pullrequestreview-%d", rid)})
	e.h.reviewEdits = map[int64]string{rid: "mallory"}
	e.sync(t)
	var body, nit bool
	for _, gc := range e.g.comments {
		switch {
		case strings.Contains(gc.Message, "retest"):
			body = true
			if gc.Author.AccountID != bridgeID {
				t.Errorf("a review body mallory edited was posted as %+v", gc.Author)
			}
		case strings.Contains(gc.Message, "nit"):
			nit = true
			if gc.Author.AccountID == bridgeID {
				t.Errorf("bob's own inline comment was posted by the bridge")
			}
		}
	}
	if !body || !nit {
		t.Errorf("not all mirrored (body %v, inline %v): %+v", body, nit, e.g.comments)
	}
	e.settled(t)
}

// Someone else's comments filling a change's limits do not cost a
// reviewer their vote: Gerrit takes the vote alone, the review's text is
// recorded as refused, and the GitHub approval is not dismissed. On a
// closed change, where Gerrit takes no votes, the text still goes in.
// And an edit Gerrit will not take does not stop the sync.
func TestVoteSurvivesRefusedComments(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.g.refuse = "LIMIT"
	e.h.reviews = append(e.h.reviews, github.Review{ID: 50, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "LGTM LIMIT", SubmittedAt: time.Now(), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-50"})
	e.sync(t)
	if len(e.g.votes) != 1 || e.g.votes[0] != "bobg:Code-Review=2" {
		t.Errorf("votes = %v", e.g.votes)
	}
	if len(e.h.dismissed) != 0 {
		t.Errorf("dismissed %v", e.h.dismissed)
	}
	e.settled(t)
	// Only +1 allowed: +1 alone. None: the approval is left alone.
	for _, max := range []int{1, 0} {
		e = newCommentsEnv(t)
		e.runAs()
		e.g.refuse, e.g.maxVote = "LIMIT", max
		e.h.reviews = append(e.h.reviews, github.Review{ID: 50, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			Body: "LGTM LIMIT", SubmittedAt: time.Now(), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-50"})
		e.sync(t)
		if want := map[int]string{1: "bobg:Code-Review=1", 0: ""}[max]; strings.Join(e.g.votes, ",") != want || len(e.h.dismissed) != 0 {
			t.Errorf("max %d: votes = %v, dismissed %v", max, e.g.votes, e.h.dismissed)
		}
	}

	e = newCommentsEnv(t)
	e.runAs()
	e.g.closed = true
	e.h.reviews = append(e.h.reviews, github.Review{ID: 51, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "Needs work.", SubmittedAt: time.Now(), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-51"})
	e.sync(t)
	if !slices.ContainsFunc(e.g.comments, func(gc gerrit.CommentInfo) bool { return strings.Contains(gc.Message, "Needs work.") }) {
		t.Errorf("review text not posted on a closed change: %+v", e.g.comments)
	}

	e = newCommentsEnv(t)
	e.runAs()
	id := e.h.human("a.txt", 3, "sha2", "first", 0)
	e.sync(t)
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == id {
			e.h.rcs[i].Body = "edited LIMIT"
		}
	}
	e.h.edits = map[int64]string{id: "bob"}
	e.g.refuse = "LIMIT"
	e.sync(t)
	e.settled(t)
}

func TestGitHubApprovalBecomesVote(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 50, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-50"})
	e.sync(t)
	if len(e.g.votes) != 1 || e.g.votes[0] != "bobg:Code-Review=2" {
		t.Errorf("votes = %v", e.g.votes)
	}
	e.settled(t)
	if len(e.g.votes) != 1 {
		t.Errorf("approval voted again: %v", e.g.votes)
	}

	// A user who may only give +1 gets +1; request changes gives -1.
	e.g.maxVote = 1
	e.h.reviews = append(e.h.reviews,
		github.Review{ID: 51, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"}, HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-51"},
		github.Review{ID: 52, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"}, Body: "Needs work.", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-52"})
	e.sync(t)
	if got := strings.Join(e.g.votes[1:], " "); got != "bobg:Code-Review=1 bobg:Code-Review=-1" {
		t.Errorf("votes = %v", e.g.votes)
	}
	for _, v := range e.g.votes {
		if strings.HasPrefix(v, ":") {
			t.Errorf("the bridge voted as itself: %v", e.g.votes)
		}
	}
	// Unmapped reviewers never vote; their review is text only.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 53, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "stranger"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-53"})
	e.sync(t)
	if len(e.g.votes) != 3 {
		t.Errorf("unmapped reviewer voted: %v", e.g.votes)
	}
	// An approval of a commit Gerrit does not have (pushed to GitHub, not
	// imported) must not vote on Gerrit's current patch set.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 54, State: "APPROVED", CommitID: "unimported", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-54"})
	e.sync(t)
	if len(e.g.votes) != 3 {
		t.Errorf("approval of an unknown commit voted: %v", e.g.votes)
	}
}

// The bridge sees a head only when it reads the PR: a branch moved away
// and back in between (with the bridge down or busy) would look as if it
// had stayed, but the activity log lists both pushes (eighth audit). A
// comment written while GitHub showed patch set 1's commit is about no
// patch set it can name.
func TestCommentWhileTheBranchWasElsewhereIsNotAboutTheHead(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	now := t0.Add(2 * time.Hour)
	e.c.Now = func() time.Time { return now }
	updateStateFn(context.Background(), e.st, 5, "revisions", func(s *state.Change) {
		s.Revisions = []state.Revision{{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "sha1", At: t0}, {PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "sha2", At: t0.Add(time.Hour)}}
		s.Heads = []state.Head{{SHA: "sha1", At: t0}, {SHA: "sha2", At: t0.Add(time.Hour)}}
	})
	e.h.pushes = []github.Push{{Before: "sha1", After: "sha2", At: now.Add(-5 * time.Minute)}, {Before: "sha2", After: "sha1", At: now.Add(-10 * time.Minute)}}
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 1, Body: "Ship it", User: github.User{Login: "bob"}, CreatedAt: now.Add(-7 * time.Minute),
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-1"})
	e.sync(t)
	posted := false
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "Ship it") {
			posted = true
			if gc.Author.Username == "bobg" {
				t.Errorf("posted as bob on patch set %d: GitHub showed patch set 1's commit then", gc.PatchSet)
			}
		}
	}
	if !posted {
		t.Error("not mirrored")
	}
}

// A comment on no commit (an issue comment, a review of a commit Gerrit
// never had) goes on the patch set whose commit GitHub had from a minute
// before it was written until now, as its author; any other is the
// bridge's. So a trusted reviewer's "retest" never starts CI on a patch
// set uploaded after it, or less than a minute before, or on one GitHub
// stopped showing since (seventh audit: a head seen later may have been
// there when the comment was written).
func TestCommentsGoOnThePatchSetTheirAuthorCouldSee(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	updateStateFn(ctx, e.st, 5, "revisions", func(s *state.Change) {
		s.Revisions = []state.Revision{{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "sha1", At: t0}, {PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "sha2", At: t0.Add(time.Hour)},
			{PatchSet: 3, GerritSHA: "sha3", At: t0.Add(90 * time.Minute)}} // not on GitHub
		s.Heads = []state.Head{{SHA: "sha1", At: t0}, {SHA: "sha2", At: t0.Add(time.Hour)}}
	})
	e.g.revs["sha3"], e.g.current = 3, "sha3"
	ic := func(id int64, body string, at time.Time) github.IssueComment {
		return github.IssueComment{ID: id, Body: body, User: github.User{Login: "bob"}, CreatedAt: at,
			HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#issuecomment-%d", id)}
	}
	e.h.ics = append(e.h.ics, ic(1, "retest one", t0.Add(time.Minute)), ic(2, "retest two", t0.Add(2*time.Hour)), ic(3, "too early", t0.Add(-time.Minute)),
		// Seconds after patch set 2 reached GitHub: its author cannot
		// have read it (fourth audit).
		ic(4, "retest moments after", t0.Add(time.Hour+30*time.Second)))
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "COMMENTED", CommitID: "unimported", User: github.User{Login: "bob"},
		Body: "review of a push", SubmittedAt: t0.Add(time.Minute), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.h.reviews = append(e.h.reviews, github.Review{ID: 72, State: "COMMENTED", CommitID: "unimported", User: github.User{Login: "bob"},
		Body: "later review of a push", SubmittedAt: t0.Add(2 * time.Hour), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-72"})
	e.h.rcs = append(e.h.rcs, github.ReviewComment{ID: 80, ReviewID: 81, Path: "a.txt", CommitID: "unimported", OriginalCommitID: "unimported",
		Body: "on a file of a push", SubjectType: "file", CreatedAt: t0.Add(2 * time.Hour), User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#discussion_r80"})
	e.h.rcs = append(e.h.rcs, github.ReviewComment{ID: 82, ReviewID: 83, Path: "a.txt", CommitID: "unimported", OriginalCommitID: "unimported",
		Body: "early file comment", SubjectType: "file", CreatedAt: t0.Add(-time.Minute), User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#discussion_r82"})
	e.h.reviews = append(e.h.reviews, github.Review{ID: 71, State: "COMMENTED", CommitID: "unimported", User: github.User{Login: "bob"},
		Body: "early review", SubmittedAt: t0.Add(-time.Minute), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71"})
	e.sync(t)
	want := map[string]string{"retest one": "3 ", "retest two": "2 bobg", "too early": "3 ", "retest moments after": "3 ", "review of a push": "3 ",
		"later review of a push": "2 bobg", "on a file of a push": "2 bobg", "early file comment": "3 ", "early review": "3 "}
	for _, gc := range e.g.comments {
		for text, w := range want {
			if strings.Contains(gc.Message, text) && !strings.Contains(gc.Message, "later "+text) {
				if got := fmt.Sprintf("%d %s", gc.PatchSet, gc.Author.Username); got != w {
					t.Errorf("%q posted on patch set and as %q, want %q", text, got, w)
				}
				delete(want, text)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("not mirrored: %v", want)
	}
	e.settled(t)

	// The branch went back to patch set 1's commit (a known head: nothing
	// is imported or recorded but the head), or on to one never imported:
	// a retest now is not for patch set 2 (seventh audit).
	// So did a branch GitHub has moved since the head was last recorded:
	// the comment sync reads it after the comments.
	for _, back := range []string{"sha1", "unimported", "not recorded"} {
		e = newCommentsEnv(t)
		e.runAs()
		if back == "not recorded" {
			e.h.head = "sha1"
		} else {
			e.pushed(t, back, t0.Add(3*time.Hour))
		}
		e.h.ics = append(e.h.ics, ic(5, "retest", t0.Add(4*time.Hour)))
		e.sync(t)
		want := map[string]string{"sha1": "1 bobg", "unimported": "2 ", "not recorded": "2 "}[back]
		if gc := e.g.comments[len(e.g.comments)-1]; fmt.Sprintf("%d %s", gc.PatchSet, gc.Author.Username) != want {
			t.Errorf("head back at %s: retest posted on patch set %d as %q, want %s", back, gc.PatchSet, gc.Author.Username, want)
		}
		if st, _ := e.st.Load(ctx, 5); st.Heads[len(st.Heads)-1].SHA != e.h.head {
			t.Errorf("head back at %s: heads %+v, want the PR's head last", back, st.Heads)
		}
	}

	// No head recorded (state from before they were kept): the first is
	// seen now, and nothing before it is as its author.
	e = newCommentsEnv(t)
	e.runAs()
	updateStateFn(ctx, e.st, 5, "heads", func(s *state.Change) { s.Heads = nil })
	e.h.ics = append(e.h.ics, ic(1, "retest one", t0.Add(time.Minute)))
	e.sync(t)
	if gc := e.g.comments[len(e.g.comments)-1]; gc.PatchSet != 2 || gc.Author.Username != "" {
		t.Errorf("posted on patch set %d as %q, want 2 as the bridge", gc.PatchSet, gc.Author.Username)
	}
}

// Gerrit comments as long as Gerrit allows, made longer by defusing, fit
// GitHub; one GitHub refuses anyway is recorded as refused, not retried,
// and does not stop the rest of the change's sync.
func TestGerritCommentsGitHubWouldRefuse(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, strings.Repeat("@", 16000), "", false)
	e.g.user("c2", gerrit.PatchSetLevel, 2, 0, "refuse me", "", false)
	e.g.user("c3", gerrit.PatchSetLevel, 2, 0, "Fine.", "", false)
	e.g.user("c4", "nowhere.txt", 2, 3, "refuse me too", "", false) // ends up a PR comment
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = append(e.g.messages,
		gerrit.ChangeMessage{ID: "m1", Author: alice, RevisionNumber: 2, Message: "Patch Set 2:\n\n" + strings.Repeat("@", 16000)},
		gerrit.ChangeMessage{ID: "m2", Author: alice, RevisionNumber: 2, Message: "Patch Set 2:\n\nrefuse me, a message"})
	e.h.refuse = "refuse me"
	e.sync(t)
	var long, fine bool
	for _, ic := range e.h.ics {
		long = long || (strings.HasPrefix(ic.Body, "**Alice Trusted (alice)**") && strings.Contains(ic.Body, tooLongForGitHub) &&
			strings.HasSuffix(ic.Body, gerritFooter(5, "c1")) && len(ic.Body) <= maxGitHubComment)
		if strings.HasSuffix(ic.Body, gerritFooter(5, "m1")) && (len(ic.Body) > maxGitHubComment || !strings.Contains(ic.Body, tooLongForGitHub)) {
			t.Errorf("long message: %d bytes", len(ic.Body))
		}
		fine = fine || strings.Contains(ic.Body, "Fine.")
	}
	if !long || !fine {
		t.Errorf("long posted %v, fine posted %v", long, fine)
	}
	st, _ := e.st.Load(context.Background(), 5)
	for _, id := range []string{"c2", "c4", "m2"} {
		if !slices.ContainsFunc(st.Comments, func(c state.Comment) bool { return c.GerritID == id && c.Kind == kindRefused }) {
			t.Errorf("refusal of %s not recorded: %+v", id, st.Comments)
		}
	}
	if !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool { return strings.HasSuffix(ic.Body, gerritFooter(5, "m1")) }) {
		t.Error("long message not posted")
	}
	e.settled(t)
	// A quote of a huge line is bounded too.
	e.g.files["big.txt"] = strings.Repeat("x", 100000) + "\n"
	if q := e.c.quote(context.Background(), 5, &gerrit.CommentInfo{Path: "big.txt", PatchSet: 2, Line: 1}, "big.txt"); len(q) > 1000 {
		t.Errorf("quote is %d bytes", len(q))
	}
	e.g.files["evil.txt"] = "</a><a href=\"https://evil\">Approved</a>\n```\n"
	q := e.c.quote(context.Background(), 5, &gerrit.CommentInfo{Path: "evil.txt", PatchSet: 2, Line: 1, Range: &gerrit.Range{StartLine: 1, EndLine: 2}}, "evil.txt")
	if !strings.Contains(q, "````\n</a>") || !strings.HasSuffix(q, "````") {
		t.Errorf("quote = %q, want the lines in a fence they cannot close", q)
	}
}

// One Gerrit author's comments past their budget wait for a later sync.
func TestGerritCommentsWithinTheAuthorsBudget(t *testing.T) {
	e := newCommentsEnv(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	e.c.PostBudget = &Budget{N: 2, Per: time.Hour, now: func() time.Time { return now }}
	for _, id := range []string{"c1", "c2", "c3"} {
		e.g.user(id, gerrit.PatchSetLevel, 2, 0, "Spam "+id, "", false)
	}
	e.sync(t)
	if len(e.h.ics) != 2 {
		t.Errorf("%d posted, want 2", len(e.h.ics))
	}
	now = now.Add(2 * time.Hour)
	e.sync(t)
	if len(e.h.ics) != 3 {
		t.Errorf("%d posted, want the third later", len(e.h.ics))
	}
	// Messages count too.
	now = now.Add(2 * time.Hour)
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	for _, id := range []string{"m1", "m2", "m3"} {
		e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: id, Author: alice, RevisionNumber: 2, Message: "Patch Set 2:\n\nSpam " + id})
	}
	e.sync(t)
	if len(e.h.ics) != 5 {
		t.Errorf("%d posted, want 2 more", len(e.h.ics))
	}
}

func TestCommentEditedBeforeMirroringIsNotPostedAsTheAuthor(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	created := time.Unix(1700000000, 0)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 9, Body: "retest", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-9"})
	id := e.h.human("a.txt", 3, "sha2", "retest", 0)
	e.h.edits = map[int64]string{9: "mallory", id: ""} // "": a deleted account
	// GitHub also moves updated_at without an edit: that alone is no edit.
	e.h.human("a.txt", 3, "sha2", "fine as is", 0)
	own := e.h.human("a.txt", 3, "sha2", "fixed my typo", 0)
	for i := range e.h.rcs {
		e.h.rcs[i].CreatedAt, e.h.rcs[i].UpdatedAt = created, created.Add(time.Second)
	}
	e.h.edits[own] = "bob" // edited by its own author: still theirs
	e.sync(t)
	byBob := map[string]bool{}
	for _, gc := range e.g.comments {
		if gc.Author.Username == "bobg" {
			byBob[gc.Message] = true
		}
	}
	if byBob["retest"] || !byBob["fine as is"] || !byBob["fixed my typo"] {
		t.Errorf("posted as bob: %v; want only the comments nobody else edited", byBob)
	}
}

func TestMessageVotes(t *testing.T) {
	for msg, want := range map[string]string{
		"Patch Set 3: Code-Review+2\n\nLGTM":            "Code-Review +2",
		"Patch Set 3: Code-Review-1 Verified+1\n\nHmm":  "Code-Review -1, Verified +1",
		"Patch Set 2: -Code-Review\n\nOops":             "Code-Review removed",
		"Patch Set 2:\n\n(1 comment)":                   "",
		"Patch Set 4: Code-Review+2 (2 comments)\n\nok": "Code-Review +2",
		"Uploaded patch set 2.":                         "",
	} {
		if got := messageVotes(msg); got != want {
			t.Errorf("messageVotes(%q) = %q, want %q", msg, got, want)
		}
	}
}

func (h *fakeGitHub) DismissReview(_ context.Context, _ int, id int64, message string) error {
	h.dismiss(id, "bridge", time.Now())
	h.dismissed = append(h.dismissed, fmt.Sprintf("%d: %s", id, message))
	return nil
}

// dismiss dismisses review id as login, as GitHub would, noting it in the
// PR's events.
func (h *fakeGitHub) dismiss(id int64, login string, at time.Time) {
	if h.dismissals == nil {
		h.dismissals = map[int64]github.Dismissal{}
	}
	for i := range h.reviews {
		if h.reviews[i].ID == id {
			h.dismissals[id] = github.Dismissal{State: strings.ToLower(h.reviews[i].State), By: login, At: at}
			h.reviews[i].State = "DISMISSED"
		}
	}
}

func (h *fakeGitHub) Dismissals(context.Context, int) (map[int64]github.Dismissal, error) {
	if h.onDismissals != nil {
		h.onDismissals()
	}
	return maps.Clone(h.dismissals), nil
}

// A dismissed approval's vote is read again just before it is withdrawn:
// the reviewer may have vetoed in Gerrit while the bridge read every page
// of the PR's issue events, which anyone can pad, and withdrawing their
// "approval" would then remove the veto (seventh audit).
func TestDismissalDoesNotWithdrawAVetoGivenMeanwhile(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Fatalf("votes = %v", e.g.labels)
	}
	e.h.dismiss(70, "maint", e.g.now())
	e.h.onDismissals = func() {
		e.h.onDismissals = nil
		e.g.vote("bobg", "Code-Review", -2)
	}
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != -2 {
		t.Errorf("bob's veto, given while the dismissal was read, is now Code-Review %+d (votes %v)", v, e.g.votes)
	}
}

// A change request that cast no vote (on a commit Gerrit lacks, or only
// just pushed) is not dismissed for disagreeing with the reviewer's
// older +2: their objection would be gone from both sides (seventh
// audit).
func TestUncountedChangeRequestIsNotDismissed(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.h.reviews = append(e.h.reviews, github.Review{ID: 71, State: "CHANGES_REQUESTED", CommitID: "unimported", User: github.User{Login: "bob"},
		Body: "Wait, no.", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71", SubmittedAt: time.Now()})
	e.sync(t)
	e.settled(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 || len(e.h.dismissed) != 0 {
		t.Errorf("votes %v, dismissed %v; want bob's +2 and his change request both standing", e.g.labels, e.h.dismissed)
	}
}

// Design §6.4: GitHub shows what Gerrit counts. An approval whose vote
// was changed in Gerrit, or reset by a new patch set, is dismissed.
func TestStaleApprovalIsDismissed(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.settled(t)
	if len(e.h.dismissed) != 0 {
		t.Fatalf("dismissed an approval Gerrit counts: %v", e.h.dismissed)
	}
	// bob changes his mind in Gerrit.
	e.g.vote("bobg", "Code-Review", -2)
	e.sync(t)
	if len(e.h.dismissed) != 1 || !strings.HasPrefix(e.h.dismissed[0], "70: ") ||
		!strings.Contains(e.h.dismissed[0], "@bob is now Code-Review -2 on patch set 2") {
		t.Fatalf("dismissed = %v", e.h.dismissed)
	}
	e.settled(t)
	if len(e.h.dismissed) != 1 {
		t.Errorf("dismissed again: %v", e.h.dismissed)
	}
	// A change request agrees with -2: kept. Then a reset (no vote): dismissed.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 71, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "No.", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71"})
	e.sync(t)
	e.g.vote("bobg", "Code-Review", -1) // what the change request cast
	e.settled(t)
	if len(e.h.dismissed) != 1 {
		t.Fatalf("dismissed a change request Gerrit agrees with: %v", e.h.dismissed)
	}
	e.g.vote("bobg", "Code-Review", 0)
	e.sync(t)
	if len(e.h.dismissed) != 2 || !strings.Contains(e.h.dismissed[1], "@bob has no Code-Review vote") {
		t.Errorf("dismissed = %v", e.h.dismissed)
	}
	// Reviews by people without a Gerrit account never voted: left alone.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 72, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "stranger"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-72"})
	e.sync(t)
	e.settled(t)
	if len(e.h.dismissed) != 2 {
		t.Errorf("dismissed an unmapped reviewer's approval: %v", e.h.dismissed)
	}
}

// Dismissing an approval on GitHub withdraws the Code-Review vote it cast
// in Gerrit (the owner's call, 2026-10-19), as its reviewer, once; a vote
// the reviewer gives again in Gerrit afterwards stays, and a dismissed
// change request, or an unmapped reviewer's approval, changes nothing.
func TestDismissedApprovalWithdrawsItsVote(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Fatalf("votes = %v", e.g.labels)
	}
	e.h.dismiss(70, "maint", e.g.now())
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 || e.g.votes[len(e.g.votes)-1] != "bobg:Code-Review=0" {
		t.Fatalf("bobg's Code-Review = %d (votes %v), want withdrawn", v, e.g.votes)
	}
	e.settled(t)
	// bob approves again in Gerrit: his.
	bobg := fakeAs{e.g, gerrit.AccountInfo{AccountID: 7, Username: "bobg"}}
	if err := bobg.SetReview(context.Background(), 1, "2", gerrit.ReviewInput{Labels: map[string]int{"Code-Review": 2}}); err != nil {
		t.Fatal(err)
	}
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Errorf("bob's new vote was withdrawn too")
	}
	// GitHub's own stale-approval dismissal on a push, or a bot's, is not
	// a person's call: the vote is left to Gerrit's copyCondition (§6.4).
	for i, d := range []github.Dismissal{{By: "pusher", Commit: "abc"}, {By: "app[bot]", Bot: true}} {
		id := int64(80 + i)
		e.h.reviews = append(e.h.reviews, github.Review{ID: id, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#pullrequestreview-%d", id), SubmittedAt: time.Now().Add(time.Duration(i) * time.Second)})
		e.sync(t)
		e.h.dismiss(id, d.By, e.g.now())
		dd := e.h.dismissals[id]
		dd.Commit, dd.Bot = d.Commit, d.Bot
		e.h.dismissals[id] = dd
		e.sync(t)
		if e.g.labels["Code-Review"]["bobg"] != 2 {
			t.Errorf("%+v: withdrew the vote", d)
		}
	}
	// A dismissed change request withdraws nothing.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 71, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "No.", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71", SubmittedAt: time.Now().Add(time.Hour)}) // bob's latest
	e.sync(t)
	e.g.vote("bobg", "Code-Review", 2)
	e.h.dismiss(71, "maint", e.g.now())
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Errorf("a dismissed change request withdrew bob's +2")
	}
}

// A review names the PR's head when it is submitted unless its client says
// otherwise (GitHub's API, gh pr review --approve): one pushed moments
// before was not what was read, so it neither votes on it nor puts a
// "retest" on it (sixth audit), as for comments and /submit. A minute on,
// it does.
func TestReviewOfAHeadPushedMomentsBeforeIsNotAboutIt(t *testing.T) {
	for _, after := range []time.Duration{5 * time.Second, minSeen + time.Second} {
		e := newCommentsEnv(t)
		e.runAs()
		now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		clock := now.Add(after + time.Second)
		e.c.Now = func() time.Time { return clock }
		if err := updateStateFn(context.Background(), e.st, 5, "revisions", func(s *state.Change) {
			s.Revisions = []state.Revision{
				{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "gh1", At: now.Add(-time.Hour)},
				{PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "gh2", At: now},
			}
			s.Heads = []state.Head{{SHA: "gh1", At: now.Add(-time.Hour)}, {SHA: "gh2", At: now}}
		}); err != nil {
			t.Fatal(err)
		}
		e.h.head = "gh2"
		e.h.reviews = append(e.h.reviews,
			github.Review{ID: 70, State: "APPROVED", CommitID: "gh2", User: github.User{Login: "bob"},
				HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70", SubmittedAt: now.Add(after)},
			github.Review{ID: 71, State: "COMMENTED", Body: "retest", CommitID: "gh2", User: github.User{Login: "bob"},
				HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71", SubmittedAt: now.Add(after)})
		// A retest posted as its author waits editSettle (it may yet be
		// edited by someone else).
		var later *errLater
		if err := e.c.Sync(context.Background(), 5); err != nil && !errors.As(err, &later) {
			t.Fatal(err)
		}
		clock = clock.Add(editSettle)
		e.sync(t)
		early := after < minSeen
		if v := e.g.labels["Code-Review"]["bobg"]; (v == 2) == early {
			t.Errorf("approved %v after patch set 2 reached GitHub: Code-Review %+d", after, v)
		}
		retest := false
		for _, gc := range e.g.comments {
			retest = retest || gc.Message == "retest" && gc.Author.Username == "bobg" && gc.PatchSet == 2
		}
		if retest == early {
			t.Errorf("retest %v after patch set 2 reached GitHub: posted on it as bob %v", after, retest)
		}
		said := slices.ContainsFunc(e.g.comments, func(gc gerrit.CommentInfo) bool { return strings.Contains(gc.Message, "less than a minute before it") })
		if said != early {
			t.Errorf("approved %v after: said it is not counted %v", after, said)
		}
	}
}

// A review mirrored late (the bridge busy or down) is older than the
// reviewer's own vote in Gerrit since, which stands: an old approval does
// not undo their veto, nor an old change request their +2 (seventh
// audit). Their own vote before it, or the bridge's, does not count.
func TestOldReviewDoesNotReplaceANewerGerritVote(t *testing.T) {
	for _, tc := range []struct {
		state, msg string
		before     bool // the Gerrit vote is older than the review
		tag        string
		gerrit     int // the vote cast in Gerrit
		want       int
	}{
		{"APPROVED", "Patch Set 2: Code-Review-2\n\nNo: this breaks the build.", false, "", -2, -2},
		{"CHANGES_REQUESTED", "Patch Set 2: Code-Review+2", false, "", 2, 2},
		{"APPROVED", "Removed Code-Review+1 by Bob G <bob@example.com>", false, "", 0, 0},
		{"APPROVED", "Patch Set 2: Code-Review-2", true, "", -2, 2},
		{"APPROVED", "Patch Set 2: Code-Review-1", false, mirrorTag, -1, 2},
	} {
		e := newCommentsEnv(t)
		e.runAs()
		e.g.vote("bobg", "Code-Review", tc.gerrit)
		e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "v", Author: gerrit.AccountInfo{AccountID: 7, Username: "bobg"},
			Tag: tc.tag, Date: e.g.tick(), RevisionNumber: 2, Message: tc.msg})
		submitted := e.g.now().Add(-24 * time.Hour)
		if tc.before {
			submitted = e.g.now().Add(time.Hour)
		}
		e.h.reviews = append(e.h.reviews, github.Review{ID: 50, State: tc.state, CommitID: "sha2", User: github.User{Login: "bob"},
			SubmittedAt: submitted, HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-50"})
		e.sync(t)
		if v := e.g.labels["Code-Review"]["bobg"]; v != tc.want {
			t.Errorf("%s, then %q in Gerrit (before %v, tag %q): Code-Review %+d, want %+d", tc.state, tc.msg, tc.before, tc.tag, v, tc.want)
		}
	}
}

// What the bridge posts in Gerrit as itself carries no retest line CI
// would take: were its account ever trusted, any GitHub user's "retest"
// would start CI with secrets (seventh audit).
func TestBridgePostsCarryNoRetestLine(t *testing.T) {
	ci := regexp.MustCompile(`(?mi)^retest$`) // gerrit-buildkite's, in any case
	// However Gerrit or a client trims a message's ends (Java's trim takes
	// every control character; eighth audit) or reads its line breaks.
	javaTrim := func(s string) string { return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' }) }
	newlines := strings.NewReplacer("\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	reads := func(m string) []string {
		return []string{m, javaTrim(m), strings.TrimSpace(m), javaTrim(newlines.Replace(m))}
	}
	for _, body := range []string{
		"Please\nretest\n Retest\r\n",
		"Please\nRETEST",
		"Please\n\u00a0retest",
		"Please\nretest\x01",
		"Please\rretest\rnow",
		"Please\u2028retest",
	} {
		e := newCommentsEnv(t)
		e.h.ics = append(e.h.ics, github.IssueComment{ID: 1, Body: body, User: github.User{Login: "stranger"},
			HTMLURL: "https://github.com/o/r/pull/1#issuecomment-1"})
		e.sync(t)
		gc := e.g.comments[len(e.g.comments)-1]
		if gc.Author.AccountID != bridgeID || !strings.Contains(gc.Message, "Please") {
			t.Errorf("%q posted by %d: %q", body, gc.Author.AccountID, gc.Message)
		}
		for _, m := range reads(gc.Message) {
			if ci.MatchString(m) {
				t.Errorf("%q: CI reads a retest in %q", body, m)
			}
		}
	}
}

// Whatever the bridge or Gerrit trims or cuts on the way, a comment that
// may reach Gerrit as a "retest" line waits before it is posted as its
// author: a no-break space or a control character around it, or a cut
// at the size limit just after it (eighth audit).
func TestAnyRetestWaits(t *testing.T) {
	long := strings.Repeat("x", maxGerritComment-len("retest")-1) + "\nretest" + strings.Repeat("y", 100)
	for name, body := range map[string]string{
		"nbsp": "\u00a0retest", "vtab": "retest\v", "formfeed": "\fretest", "ideographic": "retest\u3000",
		"control": "retest\x01", "zero width": "\u200bretest", "cut": long, "prose": "Please retest it",
	} {
		e := newCommentsEnv(t)
		e.runAs()
		now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		e.c.Now = func() time.Time { return now }
		e.h.ics = append(e.h.ics, github.IssueComment{ID: 1, Body: body, User: github.User{Login: "bob"}, CreatedAt: now.Add(-5 * time.Second),
			HTMLURL: "https://github.com/o/r/pull/1#issuecomment-1"})
		var later *errLater
		err := e.c.Sync(context.Background(), 5)
		for _, gc := range e.g.comments {
			if gc.Author.Username == "bobg" {
				t.Errorf("%s: posted as bob at once: %q", name, gc.Message)
			}
		}
		if !errors.As(err, &later) {
			t.Errorf("%s: err = %v, want a wait", name, err)
		}
	}
}

// The wait is for any comment that would start CI as its author: one
// whose "retest" Gerrit or the bridge may trim spaces from, and a review
// body, which GitHub gives no edit time for, from when the bridge first
// saw it as it is (seventh audit).
func TestRetestInAnyFormOrAReviewWaits(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	e.c.Now = func() time.Time { return now }
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 1, Body: "LGTM\r\n Retest \r\n", User: github.User{Login: "bob"}, CreatedAt: now.Add(-5 * time.Second)})
	e.h.reviews = append(e.h.reviews, github.Review{ID: 71, State: "COMMENTED", Body: "retest", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-71", SubmittedAt: now.Add(-time.Hour)})
	posted := func() []string {
		var out []string
		for _, gc := range e.g.comments {
			if gc.Author.Username == "bobg" {
				out = append(out, strings.TrimSpace(gc.Message))
			}
		}
		return out
	}
	var later *errLater
	if err := e.c.Sync(context.Background(), 5); !errors.As(err, &later) || len(posted()) != 0 {
		t.Fatalf("err = %v, posted as bob %q; want both to wait", err, posted())
	}
	// Edited meanwhile: its wait starts again.
	now = now.Add(editSettle / 2)
	e.h.reviews[0].Body = "retest\nPlease."
	now = now.Add(editSettle / 2)
	if err := e.c.Sync(context.Background(), 5); !errors.As(err, &later) || len(posted()) != 1 {
		t.Fatalf("err = %v, posted as bob %q; want the issue comment, and the edited review to wait", err, posted())
	}
	now = now.Add(editSettle)
	e.sync(t)
	if got := posted(); len(got) != 2 || !strings.Contains(got[1], "Please.") {
		t.Errorf("posted as bob %q once settled, want both", got)
	}
}

// A review counts for the commit it names only if GitHub had shown that
// commit all along for a minute before: not one it went back to just
// now, after an older patch set's commit (or the bridge added to it), for
// which a record from long ago says nothing (seventh audit).
func TestReviewOfAHeadThatJustCameBackIsNotAboutIt(t *testing.T) {
	for _, after := range []time.Duration{5 * time.Second, minSeen + time.Second} {
		e := newCommentsEnv(t)
		e.runAs()
		now := time.Now()
		// gh2 has long been patch set 2's; the branch went back to gh1,
		// and now forward to gh2 again.
		if err := updateStateFn(context.Background(), e.st, 5, "revisions", func(s *state.Change) {
			s.Revisions = []state.Revision{
				{PatchSet: 1, GerritSHA: "sha1", GitHubSHA: "gh1", At: now.Add(-2 * time.Hour)},
				{PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "gh2", At: now.Add(-time.Hour)},
			}
			s.Heads = []state.Head{{SHA: "gh1", At: now.Add(-2 * time.Hour)}, {SHA: "gh2", At: now.Add(-time.Hour)},
				{SHA: "gh1", At: now.Add(-30 * time.Minute)}, {SHA: "gh2", At: now}}
		}); err != nil {
			t.Fatal(err)
		}
		e.h.head = "gh2"
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "gh2", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70", SubmittedAt: now.Add(after)})
		e.sync(t)
		if v := e.g.labels["Code-Review"]["bobg"]; (v == 2) != (after > minSeen) {
			t.Errorf("approved %v after gh2 came back: Code-Review %+d", after, v)
		}
	}
}

// A "retest" starts CI as whoever it is posted as, and anyone with write
// access can edit anyone's comment, which GraphQL may say only a little
// later: one changed moments ago waits until it has been unchanged for
// editSettle before it is posted as its author (sixth audit). Others go
// at once.
func TestFreshRetestWaitsBeforePostingAsItsAuthor(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	e.c.Now = func() time.Time { return now }
	e.h.ics = append(e.h.ics,
		github.IssueComment{ID: 1, Body: "retest", User: github.User{Login: "bob"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-5 * time.Second)},
		github.IssueComment{ID: 2, Body: "Nice.", User: github.User{Login: "bob"}, CreatedAt: now.Add(-5 * time.Second)})
	e.h.human("a.txt", 3, "sha2", "retest", 0)
	e.h.rcs[len(e.h.rcs)-1].CreatedAt = now.Add(-5 * time.Second)
	var later *errLater
	if err := e.c.Sync(context.Background(), 5); !errors.As(err, &later) || later.after != editSettle-5*time.Second {
		t.Fatalf("err = %v, want a wait of %v", err, editSettle-5*time.Second)
	}
	posted := func(msg string) bool {
		return slices.ContainsFunc(e.g.comments, func(gc gerrit.CommentInfo) bool { return gc.Message == msg && gc.Author.Username == "bobg" })
	}
	if posted("retest") || !posted("Nice.") {
		t.Fatalf("comments %+v: want only Nice. posted yet", e.g.comments)
	}
	now = now.Add(editSettle)
	e.sync(t)
	if n := len(slices.DeleteFunc(slices.Clone(e.g.comments), func(gc gerrit.CommentInfo) bool { return gc.Message != "retest" })); n != 2 {
		t.Errorf("%d retests posted once settled, want both", n)
	}
}

// A batch is posted as the account its text was written for, looked up
// once: looked up again, it could come back otherwise (a lookup error, a
// rename), and the author's own words, written to be posted as them,
// would be posted as the bridge's (sixth audit).
func TestBatchIsPostedAsTheAccountItWasWrittenFor(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.c.Users = &Users{} // bob no longer maps to anyone
	comments := map[string][]gerrit.CommentInput{"a.txt": {{Line: 3, Message: "cc @admin, from the bridge"}}}
	if err := e.c.postBatch(context.Background(), 5, "bob", "bobg", 2, comments, comments, map[string]int{"Code-Review": 2}); err != nil {
		t.Fatal(err)
	}
	gc := e.g.comments[len(e.g.comments)-1]
	if gc.Author.Username != "bobg" || e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Errorf("posted as %q (votes %v), want as bobg, who it was written for", gc.Author.Username, e.g.labels)
	}
}

// A review votes as its author's Gerrit account only if GitHub's numeric
// id for them is the one the account is pinned to: not the account that
// took a renamed reviewer's login moments ago (sixth audit).
func TestReviewVotesOnlyAsThePinnedGitHubAccount(t *testing.T) {
	for _, id := range []int64{1, 2} {
		e := newCommentsEnv(t)
		e.runAs()
		users := e.c.Users.(*Users)
		users.Pins = &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
		users.GitHub = fakeIDs{"bob": 1}
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob", ID: id},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
		e.sync(t)
		if voted := e.g.labels["Code-Review"]["bobg"] == 2; voted != (id == 1) {
			t.Errorf("GitHub id %d: voted %v", id, voted)
		}
	}
}

// A thread resolved on GitHub is resolved in Gerrit as whoever resolved
// it only if GitHub's numeric id for them is the one their account is
// pinned to; GraphQL says it (seventh audit: by login alone, from the
// cache, the login's new holder resolved as bob).
func TestResolutionOnlyAsThePinnedGitHubAccount(t *testing.T) {
	for _, id := range []int64{1, 2, 0} {
		e := newCommentsEnv(t)
		e.runAs()
		users := e.c.Users.(*Users)
		users.Pins = &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
		gh := fakeIDs{"bob": 1}
		users.GitHub = gh
		if user, ok := users.GerritUserFor(context.Background(), github.User{Login: "bob", ID: 1}); !ok || user != "bobg" {
			t.Fatalf("bob = %q %v", user, ok)
		}
		gh["bob"] = 2 // bob renamed; someone else took the login
		e.g.user("c1", "a.txt", 2, 2, "Please fix.", "", true)
		e.sync(t)
		root := e.h.rcs[0].ID
		e.h.resolved[root] = true
		e.h.resolvedBy = map[int64]github.User{root: {Login: "bob", ID: map[int64]int64{1: 1, 2: 2, 0: 0}[id]}}
		gh["bob"] = map[int64]int64{1: 1, 2: 2, 0: 2}[id]
		e.sync(t)
		gc := e.g.comments[len(e.g.comments)-1]
		if gc.Message != "Resolved on GitHub." || (gc.Author.Username == "bobg") != (id == 1) {
			t.Errorf("GitHub id %d: resolution posted as %q: %+v", id, gc.Author.Username, gc)
		}
	}
}

// A dismissed approval's vote is withdrawn however it looks: Gerrit dates
// a vote it copies to a new patch set by that patch set, as if given
// again, and one dismissed while it was being mirrored is never mapped
// (sixth audit). Only the reviewer's own vote in Gerrit since keeps it.
func TestDismissedApprovalsCopiedOrUnmappedVoteIsWithdrawn(t *testing.T) {
	approve := func(e *commentsEnv) {
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	}
	// Copied to a new patch set after the dismissal; and the bridge's own
	// post of a GitHub vote, which may land after it, is not bob's again.
	// bob's own vote in Gerrit before the dismissal, and a comment of his
	// after it, do not keep it either.
	e := newCommentsEnv(t)
	e.runAs()
	approve(e)
	e.sync(t)
	bobg := fakeAs{e.g, gerrit.AccountInfo{AccountID: 7, Username: "bobg"}}
	if err := bobg.SetReview(context.Background(), 1, "2", gerrit.ReviewInput{Labels: map[string]int{"Code-Review": 2}}); err != nil {
		t.Fatal(err)
	}
	e.h.dismiss(70, "maint", e.g.now())
	e.g.voted["Code-Review/bobg"] = e.g.tick()
	for _, in := range []gerrit.ReviewInput{{Labels: map[string]int{"Code-Review": 2}, Tag: mirrorTag}, {Message: "Code-Review+2 still?"}} {
		if err := bobg.SetReview(context.Background(), 1, "2", in); err != nil {
			t.Fatal(err)
		}
	}
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 {
		t.Errorf("copied after the dismissal: Code-Review %+d stands", v)
	}
	// Dismissed between the vote's post and its mapping.
	e = newCommentsEnv(t)
	e.runAs()
	approve(e)
	lists := 0
	e.g.onList = func() {
		if lists++; lists == 2 {
			e.h.dismiss(70, "maint", e.g.now())
		}
	}
	e.sync(t)
	e.g.onList = nil
	if !slices.Contains(e.g.votes, "bobg:Code-Review=2") {
		t.Fatalf("votes %v: none was cast", e.g.votes)
	}
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 {
		t.Errorf("dismissed while mirrored: Code-Review %+d stands", v)
	}
	// Its text refused by Gerrit (the change's limits), so cast alone.
	e = newCommentsEnv(t)
	e.runAs()
	e.g.refuse = "Approved on GitHub"
	approve(e)
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Fatalf("votes = %v: none was cast", e.g.labels)
	}
	e.h.dismiss(70, "maint", e.g.now())
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 {
		t.Errorf("text refused: Code-Review %+d stands", v)
	}
	// Dismissed by an account since deleted: still a person's call.
	e = newCommentsEnv(t)
	e.runAs()
	approve(e)
	e.sync(t)
	e.h.dismiss(70, "", e.g.now())
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 {
		t.Errorf("dismissed by a deleted account: Code-Review %+d stands", v)
	}
}

// A dismissed approval's vote Gerrit will not reset (anyone can fill a
// change up to its message limits) is deleted. One the bridge cannot
// withdraw (Gerrit refuses that too, or the reviewer's login no longer
// maps to the account it voted as) is said to stand, once, on the PR and
// the change.
func TestDismissedApprovalsVoteThatStaysIsSaid(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.g.refuseReview = true
	e.h.dismiss(70, "maint", e.g.now())
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 0 || e.g.votes[len(e.g.votes)-1] != "bobg:Code-Review deleted" {
		t.Errorf("over the change's limits: Code-Review %+d stands (votes %v)", v, e.g.votes)
	}
	for _, why := range []string{"refused", "unmapped"} {
		e := newCommentsEnv(t)
		e.c.BridgeLogin = "bridge"
		e.runAs()
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
		e.sync(t)
		if why == "refused" {
			e.g.refuseRunAs = true
		} else {
			e.c.Users = &Users{}
		}
		// Anyone's text the bridge mirrors, on either side, saying what
		// the notice says, does not keep it from being said (seventh
		// audit).
		plant := "could not withdraw <!-- bridge:withdraw/70 --> (GitHub review 70)"
		e.g.user("c9", gerrit.PatchSetLevel, 2, 0, plant, "", false)
		e.h.ics = append(e.h.ics, github.IssueComment{ID: 99, Body: plant, User: github.User{Login: "stranger"},
			HTMLURL: "https://github.com/o/r/pull/1#issuecomment-99"})
		e.sync(t)
		// As a change message of the bridge's, should Gerrit put a
		// mirrored comment's text in one.
		e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "planted", Author: gerrit.AccountInfo{AccountID: bridgeID}, Tag: commentTag,
			Date: e.g.tick(), RevisionNumber: 2, Message: "Patch Set 2:\n\n" + plant})
		e.h.dismiss(70, "maint", e.g.now())
		posts := e.h.posts
		e.sync(t)
		if e.g.labels["Code-Review"]["bobg"] != 2 {
			t.Fatalf("%s: votes %v", why, e.g.labels)
		}
		said := 0
		for _, ic := range e.h.ics {
			if strings.Contains(ic.Body, "could not withdraw its") {
				said++
			}
		}
		last := e.g.messages[len(e.g.messages)-1]
		if said != 1 || e.h.posts != posts+1 || last.Author.AccountID != bridgeID || !strings.Contains(last.Message, "could not withdraw its Code-Review +2") {
			t.Errorf("%s: said %d times on GitHub (%d posts); Gerrit's last message %+v", why, said, e.h.posts-posts, last)
		}
		e.settled(t)
	}
}

// Gerrit text posted on GitHub under the bridge's name can neither mention
// anyone nor plant bridge markers that would be read back as the bridge's.
func TestGerritTextIsDefusedOnGitHub(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "cc @admin <!-- bridge:cmd/12 --> <!-- bridge:gerrit/5/c9 -->", "", true)
	e.sync(t)
	if len(e.h.rcs) != 1 {
		t.Fatalf("mirrored %+v", e.h.rcs)
	}
	b := e.h.rcs[0].Body
	if strings.Contains(b, "@admin") || strings.Contains(b, "bridge:cmd/") || strings.Count(b, "<!-- bridge:") != 1 ||
		!strings.HasSuffix(b, "<!-- bridge:gerrit/5/c1 -->") {
		t.Errorf("body = %q", b)
	}
	// The marker is trusted only as the bridge writes it, at the end.
	e.h.rcs[0].Body = strings.Replace(b, "<!-- bridge:gerrit/5/c1 -->", "<!-- bridge:gerrit/5/c9 --> x\n\n<!-- bridge:gerrit/5/c1 -->", 1)
	m := newCommentMap(nil)
	e.c.recover(5, &commentSnapshot{rcs: e.h.rcs, byID: map[string]*gerrit.CommentInfo{}}, m)
	if m.byGerrit["c9"] != nil || m.byGerrit["c1"] == nil {
		t.Errorf("recovered %v", m.byGerrit)
	}
}

// A marker edited into someone's comment by someone else does not keep it
// out of Gerrit.
func TestMarkerEditedIntoACommentStillMirrors(t *testing.T) {
	e := newCommentsEnv(t)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 30, Body: "Real concern. <!-- bridge:notice -->", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-30"})
	e.h.edits = map[int64]string{30: "mallory"}
	e.sync(t)
	found := false
	for _, gc := range e.g.comments {
		found = found || strings.Contains(gc.Message, "Real concern.")
	}
	if !found {
		t.Errorf("not mirrored: %+v", e.g.comments)
	}
}

// What the bridge posts in Gerrit as itself does not mention anyone.
func TestUnmappedGitHubTextDoesNotMentionInGerrit(t *testing.T) {
	if got := gerritBody("stranger", "u", "hey @admin", false); strings.Contains(got, "@admin") {
		t.Errorf("as the bridge: %q", got)
	}
	if got := gerritBody("bob", "u", "hey @admin", true); got != "hey @admin" {
		t.Errorf("as the author: %q", got)
	}
}

// A comment too long for Gerrit is shortened, with a link to the rest.
func TestLongGitHubCommentIsShortened(t *testing.T) {
	e := newCommentsEnv(t)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 30, Body: strings.Repeat("é", 15000), User: github.User{Login: "stranger"},
		HTMLURL: "https://github.com/o/r/pull/1#issuecomment-30"})
	e.sync(t)
	found := false
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "too long for Gerrit") {
			found = true
			if len(gc.Message) > 16384 || !utf8.ValidString(gc.Message) || !strings.Contains(gc.Message, "issuecomment-30") {
				t.Errorf("posted %d bytes, valid %v", len(gc.Message), utf8.ValidString(gc.Message))
			}
		}
	}
	if !found {
		t.Error("not mirrored")
	}
}

// A comment Gerrit will never take is recorded as refused, not retried
// forever, and does not stop the rest of the change's sync.
func TestRefusedGitHubCommentDoesNotStopTheSync(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.refuse = "poison"
	e.h.ics = append(e.h.ics,
		github.IssueComment{ID: 30, Body: "poison", User: github.User{Login: "stranger"}, HTMLURL: "https://github.com/o/r/pull/1#issuecomment-30"},
		github.IssueComment{ID: 31, Body: "fine", User: github.User{Login: "stranger"}, HTMLURL: "https://github.com/o/r/pull/1#issuecomment-31"})
	e.sync(t) // fails the test on an error
	var fine bool
	for _, gc := range e.g.comments {
		fine = fine || strings.Contains(gc.Message, "fine")
	}
	if !fine {
		t.Error("the other comment was not mirrored")
	}
	attempts := e.g.attempts
	e.sync(t)
	if e.g.attempts != attempts {
		t.Errorf("the refused comment was tried again (%d more calls)", e.g.attempts-attempts)
	}
}

// A suggestion holding backticks gets a longer fence, so the suggested
// text cannot close it and stays code: exact, and mentioning nobody.
func TestSuggestionWithBackticksStaysCode(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "Like this.", "", true)
	e.g.comments[0].FixSuggestions = []gerrit.FixSuggestion{{Replacements: []gerrit.FixReplacement{{
		Path: "a.txt", Range: gerrit.Range{StartLine: 2, EndLine: 3}, Replacement: "```\n@everyone\n"}}}}
	e.sync(t)
	if len(e.h.rcs) != 1 {
		t.Fatalf("got %+v", e.h.rcs)
	}
	if b := e.h.rcs[0].Body; !strings.Contains(b, "````suggestion\n```\n@everyone\n````") {
		t.Errorf("body:\n%s", b)
	}
}

// A dismissal of an approval that cast no vote (not counted) leaves the
// reviewer's own Gerrit vote alone: only the approval's vote is withdrawn.
func TestDismissalLeavesAVoteTheApprovalNeverCast(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	bobg := fakeAs{e.g, gerrit.AccountInfo{AccountID: 7, Username: "bobg"}}
	if err := bobg.SetReview(context.Background(), 5, "2", gerrit.ReviewInput{Labels: map[string]int{"Code-Review": 2}}); err != nil {
		t.Fatal(err)
	}
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha1", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70", SubmittedAt: e.g.now().Add(time.Second)})
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != 2 {
		t.Fatalf("votes = %v", e.g.labels)
	}
	e.h.dismiss(70, "maint", e.g.now().Add(2*time.Second))
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 2 {
		t.Errorf("the dismissal of an approval that cast no vote withdrew bob's own Gerrit +2: now %+d (votes %v)", v, e.g.votes)
	}
}

// The same when the reviewer's own Gerrit vote came after the approval
// (which then needs to cast none): still not a vote the approval cast.
func TestDismissalLeavesAVoteGivenAfterAnApprovalThatCastNone(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha1", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70", SubmittedAt: e.g.now().Add(-time.Second)})
	bobg := fakeAs{e.g, gerrit.AccountInfo{AccountID: 7, Username: "bobg"}}
	if err := bobg.SetReview(context.Background(), 5, "2", gerrit.ReviewInput{Labels: map[string]int{"Code-Review": 2}}); err != nil {
		t.Fatal(err)
	}
	e.sync(t)
	if !slices.Equal(e.g.votes, []string{"bobg:Code-Review=2"}) {
		t.Fatalf("votes = %v, want only bob's own", e.g.votes)
	}
	// Since the approval, the bridge posted as bob without a vote, and
	// cast someone else's vote: neither is the approval's vote.
	e.g.messages = append(e.g.messages,
		gerrit.ChangeMessage{ID: "m-bob", Author: gerrit.AccountInfo{AccountID: 7, Username: "bobg"}, Tag: mirrorTag, Date: e.g.tick(), Message: "Patch Set 2:\n\nCode-Review+2 from me later."},
		gerrit.ChangeMessage{ID: "m-carol", Author: gerrit.AccountInfo{AccountID: 8, Username: "carol"}, Tag: mirrorTag, Date: e.g.tick(), Message: "Patch Set 2: Code-Review+1"})
	e.h.dismiss(70, "maint", e.g.now().Add(2*time.Second))
	e.sync(t)
	if v := e.g.labels["Code-Review"]["bobg"]; v != 2 {
		t.Errorf("the dismissal of an approval that cast no vote withdrew bob's own Gerrit +2: now %+d (votes %v)", v, e.g.votes)
	}
}
