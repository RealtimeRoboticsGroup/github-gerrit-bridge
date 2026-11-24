package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// readOrder wraps comment sync's fakes to check the order of its reads:
// Gerrit's comments, then its change, before any GitHub list; the three
// lists overlap (each waits, the first time, until all have started);
// the PR, threads and edits only after all three returned; the pushes
// only after the PR.
type readOrder struct {
	t      *testing.T
	gerrit GerritAPI
	GitHubAPI

	mu        sync.Mutex
	started   int
	all       chan struct{} // closed once the three lists have started
	commented bool          // Gerrit's comments read this pass
	changed   bool          // Gerrit's change read this pass
	listed    int           // lists returned this pass
	pulled    bool          // the PR read since the pushes were last read
	pulls     int
	failList  error // ListIssueComments' answer, if set
	sawCtx    []error
}

type orderGerrit struct {
	GerritAPI
	o *readOrder
}

func newReadOrder(t *testing.T, e *commentsEnv) *readOrder {
	o := &readOrder{t: t, gerrit: e.c.Gerrit, GitHubAPI: e.c.GitHub, all: make(chan struct{})}
	e.c.GitHub = o
	e.c.Gerrit = orderGerrit{GerritAPI: e.c.Gerrit, o: o}
	return o
}

// first runs one of the GitHub lists.
func (o *readOrder) first(ctx context.Context, read func() error) error {
	o.mu.Lock()
	if !o.changed {
		o.t.Error("a GitHub list read before Gerrit's change")
	}
	if o.started++; o.started == 3 {
		close(o.all)
	}
	o.mu.Unlock()
	select {
	case <-o.all:
	case <-time.After(10 * time.Second):
		o.t.Error("the lists are not read alongside each other")
	}
	err := read()
	o.mu.Lock()
	o.listed++
	o.mu.Unlock()
	return err
}

// afterLists checks that what reads runs after all three lists returned.
func (o *readOrder) afterLists(what string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.listed != 3 {
		o.t.Errorf("%s read after %d of the 3 lists returned", what, o.listed)
	}
}

func (g orderGerrit) ListComments(ctx context.Context, n int) ([]gerrit.CommentInfo, error) {
	g.o.mu.Lock()
	g.o.commented, g.o.changed, g.o.listed = true, false, 0
	g.o.mu.Unlock()
	return g.GerritAPI.ListComments(ctx, n)
}

func (g orderGerrit) GetChange(ctx context.Context, n int, opts ...string) (*gerrit.ChangeInfo, error) {
	g.o.mu.Lock()
	if !g.o.commented {
		g.o.t.Error("Gerrit's change read before its comments")
	}
	g.o.commented = false
	g.o.mu.Unlock()
	ch, err := g.GerritAPI.GetChange(ctx, n, opts...)
	g.o.mu.Lock()
	g.o.changed = true
	g.o.mu.Unlock()
	return ch, err
}

func (o *readOrder) ListReviewComments(ctx context.Context, n int) (out []github.ReviewComment, err error) {
	err = o.first(ctx, func() (err error) { out, err = o.GitHubAPI.ListReviewComments(ctx, n); return })
	return
}

func (o *readOrder) ListIssueComments(ctx context.Context, n int) (out []github.IssueComment, err error) {
	err = o.first(ctx, func() (err error) {
		if o.failList != nil {
			return o.failList
		}
		out, err = o.GitHubAPI.ListIssueComments(ctx, n)
		return
	})
	return
}

func (o *readOrder) ListReviews(ctx context.Context, n int) (out []github.Review, err error) {
	err = o.first(ctx, func() (err error) {
		if o.failList != nil {
			// Waits for the failure to cancel it.
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			o.mu.Lock()
			o.sawCtx = append(o.sawCtx, ctx.Err())
			o.mu.Unlock()
		}
		out, err = o.GitHubAPI.ListReviews(ctx, n)
		return
	})
	return
}

func (o *readOrder) ReviewThreads(ctx context.Context, n int) ([]github.ReviewThread, error) {
	o.afterLists("the threads")
	return o.GitHubAPI.ReviewThreads(ctx, n)
}

func (o *readOrder) Edits(ctx context.Context, n int) (map[int64]string, error) {
	o.afterLists("the edits")
	return o.GitHubAPI.Edits(ctx, n)
}

func (o *readOrder) ReviewEdits(ctx context.Context, n int) (map[int64]string, error) {
	o.afterLists("the review edits")
	return o.GitHubAPI.ReviewEdits(ctx, n)
}

func (o *readOrder) GetPull(ctx context.Context, n int) (*github.PullRequest, error) {
	o.afterLists("the PR")
	p, err := o.GitHubAPI.GetPull(ctx, n)
	o.mu.Lock()
	o.pulled = true
	o.pulls++
	o.mu.Unlock()
	return p, err
}

func (o *readOrder) Pushes(ctx context.Context, repo, ref string) ([]github.Push, error) {
	o.mu.Lock()
	if !o.pulled {
		o.t.Error("the pushes read before the PR")
	}
	o.pulled = false
	o.mu.Unlock()
	return o.GitHubAPI.Pushes(ctx, repo, ref)
}

// Gerrit's comments, then its change; GitHub's lists alongside each
// other; the PR, threads and edits after every list, and the branch's
// pushes after the PR.
func TestObserveReadsInStages(t *testing.T) {
	e := newCommentsEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "Hmm?", "", true)
	e.h.human("a.txt", 3, "sha2", "Why?", 0)
	o := newReadOrder(t, e)
	e.sync(t)
	if o.pulls == 0 {
		t.Error("the PR was never read")
	}
}

// A read that fails stops the others; the sync answers with its error.
func TestObserveStopsAtTheFirstError(t *testing.T) {
	e := newCommentsEnv(t)
	o := newReadOrder(t, e)
	o.failList = errors.New("issue comments: 502")
	err := e.c.Sync(context.Background(), 5)
	if !errors.Is(err, o.failList) {
		t.Fatalf("sync: %v, want the failed read's error", err)
	}
	if len(o.sawCtx) != 1 || !errors.Is(o.sawCtx[0], context.Canceled) {
		t.Errorf("the other reads went on: %v", o.sawCtx)
	}
	if o.pulls != 0 {
		t.Error("the PR was read after a list failed")
	}
}

// replyMidRead is Gerrit, with alice replying (a message and its comment,
// written together) once the change has been read, if it is read first.
type replyMidRead struct {
	GerritAPI
	e       *commentsEnv
	changed chan struct{}
	once    sync.Once
}

func (g *replyMidRead) GetChange(ctx context.Context, n int, opts ...string) (*gerrit.ChangeInfo, error) {
	ch, err := g.GerritAPI.GetChange(ctx, n, opts...)
	g.once.Do(func() { close(g.changed) })
	return ch, err
}

func (g *replyMidRead) ListComments(ctx context.Context, n int) ([]gerrit.CommentInfo, error) {
	select {
	case <-g.changed:
	case <-time.After(time.Second): // the change is read after
	}
	fakeReads.Lock()
	if len(g.e.g.comments) == 0 {
		alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
		at := g.e.g.tick()
		g.e.g.messages = append(g.e.g.messages,
			gerrit.ChangeMessage{ID: "m0", Author: alice, Date: at, Message: "Patch Set 2:\n\n(1 comment)", RevisionNumber: 2})
		g.e.g.comments = append(g.e.g.comments,
			gerrit.CommentInfo{ID: "cA", Path: gerrit.PatchSetLevel, PatchSet: 2, Message: "Her own words.", Updated: at, Author: alice, ChangeMessageID: "m0"})
	}
	fakeReads.Unlock()
	return g.GerritAPI.ListComments(ctx, n)
}

// A reply posted while comment sync reads is still its author's: the
// change, read after the comments, has every comment's message (sixth
// review).
func TestReplyDuringReadIsStillHerOwn(t *testing.T) {
	e, _ := linkedEnv(t)
	e.c.Gerrit = &replyMidRead{GerritAPI: e.c.Gerrit, e: e, changed: make(chan struct{})}
	e.sync(t)
	for _, ic := range e.h.ics {
		if strings.Contains(ic.Body, "Her own words.") {
			if ic.User != aliceGH || strings.Contains(ic.Body, "Someone") {
				t.Errorf("alice's own reply posted by %+v: %q", ic.User, ic.Body)
			}
			return
		}
	}
	t.Errorf("not posted: %+v", e.h.ics)
}
