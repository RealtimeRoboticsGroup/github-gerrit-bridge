package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

const bridgeID = 99

type fakeGerrit struct {
	messages []gerrit.ChangeMessage
	comments []gerrit.CommentInfo
	revs     map[string]int // sha -> ps
	current  string
	files    map[string]string
	clock    int
	nextID   int
	reviews  int
}

func (f *fakeGerrit) tick() string {
	f.clock++
	return fmt.Sprintf("2026-10-04 00:00:%02d.000000000", f.clock)
}

func (f *fakeGerrit) ListComments(context.Context, int) ([]gerrit.CommentInfo, error) {
	return append([]gerrit.CommentInfo(nil), f.comments...), nil
}

func (f *fakeGerrit) SetReview(_ context.Context, _ int, rev string, in gerrit.ReviewInput) error {
	f.reviews++
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
				Author: gerrit.AccountInfo{AccountID: bridgeID, Name: "GitHub Bridge"}, Unresolved: ci.Unresolved != nil && *ci.Unresolved,
			})
		}
	}
	return nil
}

func (f *fakeGerrit) GetChange(context.Context, int, ...string) (*gerrit.ChangeInfo, error) {
	ci := &gerrit.ChangeInfo{CurrentRevision: f.current, Revisions: map[string]gerrit.RevisionInfo{}, Messages: f.messages}
	for sha, ps := range f.revs {
		ci.Revisions[sha] = gerrit.RevisionInfo{Number: ps}
	}
	return ci, nil
}

func (f *fakeGerrit) FileContent(_ context.Context, _ int, _ string, path string) ([]byte, error) {
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
	rcs      []github.ReviewComment
	ics      []github.IssueComment
	reviews  []github.Review
	resolved map[int64]bool // thread root -> resolved
	// diff lines per path that accept line comments.
	diff   map[string]map[int]bool
	nextID int64
	posts  int
}

func (f *fakeGitHub) id() int64 { f.nextID++; return 1000 + f.nextID }

func (f *fakeGitHub) ListReviewComments(context.Context, int) ([]github.ReviewComment, error) {
	return append([]github.ReviewComment(nil), f.rcs...), nil
}
func (f *fakeGitHub) ListReviews(context.Context, int) ([]github.Review, error) {
	return append([]github.Review(nil), f.reviews...), nil
}
func (f *fakeGitHub) ListIssueComments(context.Context, int) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.ics...), nil
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
		if !f.diff[d.Path][d.Line] {
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

func (f *fakeGitHub) CreateReviewComment(_ context.Context, _ int, rc github.NewReviewComment) (*github.ReviewComment, error) {
	if rc.SubjectType == "file" {
		if f.diff[rc.Path] == nil {
			return nil, errUnprocessable
		}
	} else if !f.diff[rc.Path][rc.Line] {
		return nil, errUnprocessable
	}
	f.posts++
	id := f.id()
	st := rc.SubjectType
	if st == "" {
		st = "line"
	}
	c := github.ReviewComment{ID: id, Path: rc.Path, Line: rc.Line, OriginalLine: rc.Line, Side: rc.Side, CommitID: rc.CommitID,
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
	h := &fakeGitHub{resolved: map[int64]bool{}, diff: map[string]map[int]bool{"a.txt": {2: true, 3: true}}}
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
	for _, want := range []string{"**Alice Trusted**", "Why two?", "<!-- bridge:gerrit/5/c1 -->", "https://g/c/p/+/5/comment/c1/"} {
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
	if strings.Contains(b, "Code-Review") {
		t.Errorf("vote header leaked:\n%s", b)
	}
	e.settled(t)
}
