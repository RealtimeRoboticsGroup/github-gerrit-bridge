package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// readOrder wraps comment sync's fakes to check the order of its reads:
// the five first reads overlap (each waits, the first time, until all
// have started), the PR is read only after all five returned, and the
// pushes only after the PR.
type readOrder struct {
	t      *testing.T
	gerrit GerritAPI
	GitHubAPI

	mu       sync.Mutex
	started  int
	all      chan struct{} // closed once the first five have started
	listed   int           // first reads returned since the PR was last read
	pulled   bool          // the PR read since the pushes were last read
	pulls    int
	failList error // ListIssueComments' answer, if set
	sawCtx   []error
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

// first runs one of the first reads.
func (o *readOrder) first(ctx context.Context, read func() error) error {
	o.mu.Lock()
	if o.started++; o.started == 5 {
		close(o.all)
	}
	o.mu.Unlock()
	select {
	case <-o.all:
	case <-time.After(10 * time.Second):
		o.t.Error("the first reads are not made alongside each other")
	}
	err := read()
	o.mu.Lock()
	o.listed++
	o.mu.Unlock()
	return err
}

func (g orderGerrit) ListComments(ctx context.Context, n int) (out []gerrit.CommentInfo, err error) {
	err = g.o.first(ctx, func() (err error) { out, err = g.GerritAPI.ListComments(ctx, n); return })
	return
}

func (g orderGerrit) GetChange(ctx context.Context, n int, opts ...string) (out *gerrit.ChangeInfo, err error) {
	err = g.o.first(ctx, func() (err error) { out, err = g.GerritAPI.GetChange(ctx, n, opts...); return })
	return
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

func (o *readOrder) GetPull(ctx context.Context, n int) (*github.PullRequest, error) {
	o.mu.Lock()
	if o.listed != 5 {
		o.t.Errorf("the PR read after %d of the 5 first reads returned", o.listed)
	}
	o.listed = 0
	o.mu.Unlock()
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

// The first reads go alongside each other; the PR is read after every
// list, and the branch's pushes after the PR.
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
