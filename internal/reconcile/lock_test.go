package reconcile

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// done reports whether f returns within d.
func done(d time.Duration, f func()) bool {
	c := make(chan struct{})
	go func() { f(); close(c) }()
	select {
	case <-c:
		return true
	case <-time.After(d):
		return false
	}
}

// Comment syncs share the branch with one another, never with its
// reconcile or a merge; and a writer waiting goes ahead of new readers.
func TestBranchLockShares(t *testing.T) {
	c := &Code{}
	r1 := c.rlock("main")
	if !done(time.Second, func() { c.rlock("main")() }) {
		t.Fatal("a second comment sync waited for the first")
	}
	wrote := make(chan struct{})
	go func() { c.lock("main")(); close(wrote) }()
	select {
	case <-wrote:
		t.Fatal("the branch's reconcile ran alongside a comment sync")
	case <-time.After(50 * time.Millisecond):
	}
	if done(50*time.Millisecond, func() { c.rlock("main")() }) {
		t.Error("a new comment sync went ahead of the waiting reconcile")
	}
	r1()
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("the reconcile never got the branch")
	}
	w := c.lockUrgent("main")
	if done(50*time.Millisecond, func() { c.rlock("main")() }) {
		t.Error("a comment sync ran alongside a merge")
	}
	w()
	l := c.branchLock("main")
	l.mu.Lock()
	l.urgent = 1 // a merge waiting
	l.mu.Unlock()
	if done(50*time.Millisecond, func() { c.rlock("main")() }) {
		t.Error("a comment sync went ahead of a waiting merge")
	}
	l.mu.Lock()
	l.urgent = 0
	l.cond.Broadcast()
	l.mu.Unlock()
	if !done(time.Second, func() { c.rlock("other")() }) {
		t.Error("another branch's comment sync waited")
	}
}

// A comment sync holds its branch shared, so the comment syncs of other
// changes run alongside it, but not two of one change's at once.
func TestCommentSyncHoldsTheBranchShared(t *testing.T) {
	e := newCommentsEnv(t) // change 5 on main
	d := &Dispatcher{Code: &Code{}, Comments: e.c, Store: e.st, Logger: slog.Default()}
	ctx := context.Background()
	release := d.Code.rlock("main") // another change's comment sync
	if !done(5*time.Second, func() {
		if err := d.Reconcile(ctx, Key{Comments: 5}); err != nil {
			t.Error(err)
		}
	}) {
		t.Fatal("the comment sync waited for another change's")
	}
	release()
	unlock := d.changeLock(5) // a pr-comments sync of the same change
	if done(100*time.Millisecond, func() { d.Reconcile(ctx, Key{Comments: 5}) }) {
		t.Error("two comment syncs of one change ran at once")
	}
	unlock()
}

// lockChecker is a submitter that records whether the branch was held
// alone when it submitted.
type lockChecker struct {
	*fakeSubmitter
	l     *branchLock
	alone []bool
}

func (c *lockChecker) SubmitAs(ctx context.Context, user string, change int, rev string) error {
	c.l.mu.Lock()
	c.alone = append(c.alone, c.l.held && c.l.readers == 0)
	c.l.mu.Unlock()
	return c.fakeSubmitter.SubmitAs(ctx, user, change, rev)
}

// A /submit found by a comment sync runs with the branch held alone.
func TestSubmitRunsWithTheBranchAlone(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	e.c.Now = func() time.Time { return now }
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 2, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: now.Add(-time.Hour)})
	code := &Code{}
	sub := &lockChecker{fakeSubmitter: &fakeSubmitter{}, l: code.branchLock("main")}
	d := &Dispatcher{Code: code, Comments: e.c, Store: e.st, Logger: slog.Default(),
		Commands: &Commands{GitHub: e.h, Submitter: sub, Store: e.st, Identity: &Users{Overrides: map[string]string{"AustinSchuh": "alice"}},
			BridgeLogin: "bridge", Logger: slog.Default(), Now: func() time.Time { return now }}}
	if err := d.Reconcile(context.Background(), Key{Comments: 5}); err != nil {
		t.Fatal(err)
	}
	if len(sub.alone) != 1 || !sub.alone[0] {
		t.Errorf("submits %v, with the branch alone %v", sub.calls, sub.alone)
	}
}
