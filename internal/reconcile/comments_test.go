package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

const bridgeID = 99

type fakeGerrit struct {
	// noFixes makes SetReview fail like a site without the fix
	// suggestions feature flag.
	noFixes bool
	// votes records labels set by SetReview, as "user:Label=value";
	// labels holds the resulting votes, label -> user -> value.
	votes       []string
	labels      map[string]map[string]int
	refuseRunAs bool
	maxVote     int
	messages    []gerrit.ChangeMessage
	comments    []gerrit.CommentInfo
	revs        map[string]int // sha -> ps
	current     string
	files       map[string]string
	clock       int
	nextID      int
	reviews     int
}

// vote sets user's vote on the current patch set, as Gerrit would.
func (f *fakeGerrit) vote(user, label string, v int) {
	if f.labels == nil {
		f.labels = map[string]map[string]int{}
	}
	if f.labels[label] == nil {
		f.labels[label] = map[string]int{}
	}
	f.labels[label][user] = v
}

func (f *fakeGerrit) tick() string {
	f.clock++
	return fmt.Sprintf("2026-10-04 00:00:%02d.000000000", f.clock)
}

func (f *fakeGerrit) ListComments(context.Context, int) ([]gerrit.CommentInfo, error) {
	return append([]gerrit.CommentInfo(nil), f.comments...), nil
}

func (f *fakeGerrit) SetReview(ctx context.Context, n int, rev string, in gerrit.ReviewInput) error {
	return f.setReview(ctx, gerrit.AccountInfo{AccountID: bridgeID, Name: "GitHub Bridge"}, n, rev, in)
}

// fakeAs is the fake Gerrit acting as another user (Run-As).
type fakeAs struct {
	*fakeGerrit
	user gerrit.AccountInfo
}

func (a fakeAs) SetReview(ctx context.Context, n int, rev string, in gerrit.ReviewInput) error {
	if a.refuseRunAs {
		return &gerrit.Error{Status: 403, Body: "not permitted to run as"}
	}
	if in.Labels["Code-Review"] > a.maxVote {
		return &gerrit.Error{Status: 403, Body: "Applying label \"Code-Review\": 2 is restricted"}
	}
	return a.setReview(ctx, a.user, n, rev, in)
}

func (f *fakeGerrit) setReview(_ context.Context, author gerrit.AccountInfo, _ int, rev string, in gerrit.ReviewInput) error {
	if f.noFixes && hasFixes(in.Comments) {
		return &gerrit.Error{Status: 500, Body: "Internal server error"}
	}
	f.reviews++
	for l, v := range in.Labels {
		f.votes = append(f.votes, fmt.Sprintf("%s:%s=%d", author.Username, l, v))
		f.vote(author.Username, l, v)
	}
	ts := f.tick()
	var ps int
	fmt.Sscan(rev, &ps)
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
			l.All = append(l.All, gerrit.ApprovalInfo{AccountInfo: gerrit.AccountInfo{Username: u}, Value: byUser[u]})
		}
		ci.Labels[label] = l
	}
	for sha, ps := range f.revs {
		ci.Revisions[sha] = gerrit.RevisionInfo{Number: ps}
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
	edits map[int64]string // edited comment id -> editor
	// reviewEdits is edits for review bodies.
	reviewEdits map[int64]string
	// dismissed records DismissReview calls, as "id: message".
	dismissed []string
	// afterReview runs once, when the bridge reads back a review it posted.
	afterReview func()
	rcs         []github.ReviewComment
	ics         []github.IssueComment
	reviews     []github.Review
	resolved    map[int64]bool // thread root -> resolved
	// diff lines per path that accept line comments; diffAt overrides it
	// per "commit/path".
	diff     map[string]map[int]bool
	diffAt   map[string]map[int]bool
	files    map[string]string // "commit/path" -> content
	ancestor map[string]bool   // "a..b"
	nextID   int64
	posts    int
}

func (f *fakeGitHub) id() int64 { f.nextID++; return 1000 + f.nextID }

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
	return append([]github.Review(nil), f.reviews...), nil
}
func (f *fakeGitHub) ListIssueComments(context.Context, int) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.ics...), nil
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
func (f *fakeGitHub) ReviewThreads(context.Context, int) ([]github.ReviewThread, error) {
	var out []github.ReviewThread
	for _, rc := range f.rcs {
		if rc.InReplyTo == 0 {
			out = append(out, github.ReviewThread{ID: fmt.Sprintf("T%d", rc.ID), RootID: rc.ID, IsResolved: f.resolved[rc.ID]})
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
	f.posts++
	rid := f.id()
	for _, d := range r.Comments {
		id := f.id()
		f.rcs = append(f.rcs, github.ReviewComment{ID: id, ReviewID: rid, Path: d.Path, Line: d.Line, OriginalLine: d.Line,
			Side: d.Side, CommitID: r.CommitID, OriginalCommitID: r.CommitID, Body: d.Body, SubjectType: "line",
			HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#discussion_r%d", id), User: github.User{Login: "bridge"}})
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
		OriginalCommitID: rc.CommitID, Body: rc.Body, SubjectType: st, User: github.User{Login: "bridge"}}
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
		CommitID: root.CommitID, OriginalCommitID: root.OriginalCommitID, Body: body, User: github.User{Login: "bridge"}}
	f.rcs = append(f.rcs, c)
	return &c, nil
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, _ int, body string) (*github.IssueComment, error) {
	f.posts++
	c := github.IssueComment{ID: f.id(), Body: body, User: github.User{Login: "bridge"}}
	f.ics = append(f.ics, c)
	return &c, nil
}

func (f *fakeGitHub) SetThreadResolved(_ context.Context, id string, resolved bool) error {
	var root int64
	fmt.Sscanf(id, "T%d", &root)
	f.resolved[root] = resolved
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
	if err := store.Save(ctx, 5, &state.Change{State: state.ChangeState{PR: 1, Branch: "main", Lifecycle: "NEW"}}, "link"); err != nil {
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

func TestOutOfHunkFallsBackToFileLevel(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 5, "Line five is outside the diff.", "", true)
	e.sync(t)
	if len(e.h.rcs) != 1 || e.h.rcs[0].SubjectType != "file" {
		t.Fatalf("got %+v", e.h.rcs)
	}
	if !strings.Contains(e.h.rcs[0].Body, "`a.txt` L5:\n> five") {
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
	for i := range h.reviews {
		if h.reviews[i].ID == id {
			h.reviews[i].State = "DISMISSED"
		}
	}
	h.dismissed = append(h.dismissed, fmt.Sprintf("%d: %s", id, message))
	return nil
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
