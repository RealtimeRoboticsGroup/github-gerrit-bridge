package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
)

func drain(q *queue.Queue[Key]) []string {
	var got []string
	for q.Len() > 0 {
		k, _ := q.Get()
		got = append(got, k.String())
		q.Done(k)
	}
	return got
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parse(t *testing.T, s string) *events.Event {
	t.Helper()
	e, err := events.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRouterEvents(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	for _, s := range []string{
		`{"type":"patchset-created","change":{"project":"p","branch":"main","number":5}}`,
		`{"type":"comment-added","change":{"project":"p","branch":"rel","number":7}}`,
		`{"type":"change-abandoned","change":{"project":"p","branch":"rel","number":7}}`,
		`{"type":"change-merged","change":{"project":"p","branch":"rel","number":8}}`,
		`{"type":"comment-added","change":{"project":"other","number":6}}`,
		`{"type":"comment-added","change":{"project":"p","number":5}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/heads/main"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/changes/34/1234/meta"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/bridge/changes/34/1234"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/tags/v1"}}`,
	} {
		r.Event(parse(t, s))
	}
	r.Delivery(&github.Delivery{Event: "pull_request", PR: 9})
	r.Delivery(&github.Delivery{Event: "pull_request_review_comment", PR: 9})
	r.Delivery(&github.Delivery{Event: "push"})
	// patchset-created: code and comments; comment-added: comments only;
	// change-abandoned: code only; change-merged: its PRs first, then code.
	want := []string{"branch/main", "comments/5", "comments/7", "branch/rel", "merged/8", "pr/9", "pr-comments/9"}
	if got := drain(q); !equal(got, want) {
		t.Errorf("queued %v, want %v", got, want)
	}
}

type fakeLister struct {
	mu    sync.Mutex
	fails int
	calls int
}

func (f *fakeLister) ResyncKeys(context.Context) ([]Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("gerrit down")
	}
	return []Key{{Comments: 1}, {Comments: 2}, {Comments: 3}}, nil
}

func TestResyncCoalescesAndQueues(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	// Many requests before the loop runs collapse into one.
	r.Resync("connected")
	r.Resync("dropped-output")
	r.Resync("connected")
	l := &fakeLister{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunResync(ctx, l)
	deadline := time.Now().Add(5 * time.Second)
	for q.Len() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := drain(q); !equal(got, []string{"comments/1", "comments/2", "comments/3"}) {
		t.Errorf("queued %v", got)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls != 1 {
		t.Errorf("listed %d times, want 1", l.calls)
	}
}

// A resync's keys wait behind events': the next event goes first (in the
// rehearsal, each periodic resync held the PR events after it back
// minutes).
func TestResyncWaitsBehindEvents(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunResync(ctx, &fakeLister{})
	r.Resync("periodic")
	deadline := time.Now().Add(5 * time.Second)
	for q.Len() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "closed", PR: 9})
	if got := drain(q); !equal(got, []string{"pr/9", "comments/1", "comments/2", "comments/3"}) {
		t.Errorf("queued %v, want the PR's event first", got)
	}
}

func TestResyncRetries(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	l := &fakeLister{fails: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunResync(ctx, l)
	r.Resync("connected")
	deadline := time.Now().Add(5 * time.Second)
	for q.Len() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q.Len() != 3 {
		t.Fatalf("after a failed listing, queued %d keys, want 3", q.Len())
	}
}

type flaky struct {
	mu    sync.Mutex
	calls map[Key]int
}

func (f *flaky) Reconcile(_ context.Context, k Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[k]++
	switch {
	case k.Comments == 1 && f.calls[k] == 1:
		return errors.New("transient")
	case k.Comments == 2 && f.calls[k] == 1:
		panic("bug")
	}
	return nil
}

// blocker holds every non-urgent key until released.
type blocker struct {
	release chan struct{}
	merged  chan struct{}
}

func (b *blocker) Reconcile(_ context.Context, k Key) error {
	if k.Merged != 0 {
		close(b.merged)
		return nil
	}
	<-b.release
	return nil
}

// A merge does not wait for a worker to come free: one worker takes only
// urgent keys.
func TestUrgentKeysHaveAWorker(t *testing.T) {
	q := queue.New[Key]()
	q.Urgent = Urgent
	b := &blocker{release: make(chan struct{}), merged: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		Work(context.Background(), q, b, 1, slog.Default())
		close(done)
	}()
	q.Add(Key{Comments: 1}) // keeps the one ordinary worker busy
	time.Sleep(20 * time.Millisecond)
	q.Add(Key{Merged: 2})
	select {
	case <-b.merged:
	case <-time.After(5 * time.Second):
		t.Error("the merge waited for the busy worker")
	}
	close(b.release)
	q.ShutDown()
	<-done
}

func TestWorkRetries(t *testing.T) {
	q := queue.New[Key]()
	q.MinRetry, q.MaxRetry = time.Millisecond, time.Millisecond
	f := &flaky{calls: map[Key]int{}}
	done := make(chan struct{})
	go func() {
		Work(context.Background(), q, f, 2, slog.Default())
		close(done)
	}()
	q.Add(Key{Comments: 1})
	q.Add(Key{Comments: 2})
	q.Add(Key{Comments: 3})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		ok := f.calls[Key{Comments: 1}] == 2 && f.calls[Key{Comments: 2}] == 2 && f.calls[Key{Comments: 3}] == 1
		f.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	q.ShutDown()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls[Key{Comments: 1}] != 2 || f.calls[Key{Comments: 2}] != 2 || f.calls[Key{Comments: 3}] != 1 {
		t.Errorf("calls = %v; want an error and a panic each retried once", f.calls)
	}
}

// laterOnce waits once, then is done.
type laterOnce struct {
	mu    sync.Mutex
	calls int
}

func (l *laterOnce) Reconcile(context.Context, Key) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls++; l.calls == 1 {
		return &errLater{after: time.Millisecond}
	}
	return nil
}

// A reconcile that leaves work for later comes back then, not after a
// failure's backoff.
func TestWorkComesBackForWhatWaits(t *testing.T) {
	q := queue.New[Key]()
	q.MinRetry, q.MaxRetry = time.Hour, time.Hour
	l := &laterOnce{}
	done := make(chan struct{})
	go func() {
		Work(context.Background(), q, l, 1, slog.Default())
		close(done)
	}()
	q.Add(Key{Comments: 1})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		calls := l.calls
		l.mu.Unlock()
		if calls == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	q.ShutDown()
	<-done
	if l.calls != 2 {
		t.Errorf("calls = %d, want the wait and the run after it", l.calls)
	}
}

func TestPushersFromWebhooks(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	r.Pushers = &Pushers{}
	head := func(ref, sha string) string {
		return `"head":{"ref":"` + ref + `","sha":"` + sha + `","repo":{"full_name":"fork/r"}}`
	}
	// What a synchronize pushed is its after: the PR's head may already
	// be a later push's.
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "synchronize", PR: 3,
		Payload: []byte(`{"sender":{"login":"bob","id":2},"before":"aaa","after":"abc","pull_request":{"number":3,` + head("b", "later") + `}}`)})
	r.Delivery(&github.Delivery{Event: "push",
		Payload: []byte(`{"sender":{"login":"carol","id":3},"before":"ddd","after":"def","ref":"refs/heads/x","repository":{"full_name":"O/R"}}`)})
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "edited", PR: 3,
		Payload: []byte(`{"sender":{"login":"mallory"},"pull_request":{"number":3,` + head("b", "abc") + `}}`)})
	// Opening or reopening a PR is not pushing its head (anyone may open
	// one from someone else's branch), and a push to another branch is
	// not a push to this one.
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "opened", PR: 4,
		Payload: []byte(`{"sender":{"login":"maint"},"pull_request":{"number":4,` + head("g", "ghi") + `}}`)})
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "reopened", PR: 4,
		Payload: []byte(`{"sender":{"login":"maint"},"pull_request":{"number":4,` + head("g", "ghi") + `}}`)})
	r.Delivery(&github.Delivery{Event: "push",
		Payload: []byte(`{"sender":{"login":"maint"},"after":"abc","ref":"refs/heads/test-it","repository":{"full_name":"fork/r"}}`)})
	for _, c := range []struct {
		repo, ref, sha string
		want           github.Push
	}{
		{"fork/r", "refs/heads/b", "abc", github.Push{Before: "aaa", After: "abc", Actor: github.User{Login: "bob", ID: 2}}},
		{"o/r", "refs/heads/x", "def", github.Push{Before: "ddd", After: "def", Actor: github.User{Login: "carol", ID: 3}}},
		{"fork/r", "refs/heads/g", "ghi", github.Push{}},
		{"fork/r", "refs/heads/test-it", "abc", github.Push{After: "abc", Actor: github.User{Login: "maint"}}},
		{"o/r", "refs/heads/b", "abc", github.Push{}},
		{"fork/r", "refs/heads/b", "later", github.Push{}},
	} {
		if got, ok := r.Pushers.Lookup(c.repo, c.ref, c.sha); got != c.want || ok != (c.want != github.Push{}) {
			t.Errorf("%s %s %s: %+v %v, want %+v", c.repo, c.ref, c.sha, got, ok, c.want)
		}
	}
	p := &Pushers{}
	pusher := func(sha string) string {
		got, _ := p.Lookup("o/r", "refs/heads/x", sha)
		return got.Actor.Login
	}
	for i := range maxPushers + 10 {
		p.Record("o/r", "refs/heads/x", "", fmt.Sprint(i), github.User{Login: "x"})
	}
	if pusher("0") != "" || pusher(fmt.Sprint(maxPushers+9)) != "x" || len(p.byPush) != maxPushers || len(p.order) != maxPushers {
		t.Error("pusher log not bounded")
	}
	// The latest push wins, as in GitHub's activity log, and takes no
	// more room.
	p.Record("o/r", "refs/heads/x", "", fmt.Sprint(maxPushers+9), github.User{Login: "y"})
	if pusher(fmt.Sprint(maxPushers+9)) != "y" || pusher("10") != "x" {
		t.Error("a second push of the same commit to the same branch was not its pusher's")
	}
}

// Everything but a merge's PRs is throttled: anyone can cause events on
// either side, and a merge races replication.
func TestThrottled(t *testing.T) {
	for k, want := range map[Key]bool{{PR: 5}: true, {PRComments: 5}: true, {Branch: "main"}: true, {Comments: 7}: true, {Merged: 7}: false} {
		if Throttled(k) != want {
			t.Errorf("Throttled(%+v) = %v", k, !want)
		}
	}
}

// A merge's PR work waits for whoever holds the branch, but not for
// anyone else waiting: while it waits, nobody else gets the lock.
func TestUrgentLockGoesFirst(t *testing.T) {
	c := &Code{}
	l := c.branchLock("main")
	l.mu.Lock()
	l.urgent = 1 // a merge waiting
	l.mu.Unlock()
	got := make(chan struct{})
	go func() { u := c.lock("main"); close(got); u() }()
	select {
	case <-got:
		t.Fatal("a comment sync took the lock ahead of a waiting merge")
	case <-time.After(50 * time.Millisecond):
	}
	u := c.lockUrgent("main") // the merge gets it, and lets go
	l.mu.Lock()
	l.urgent-- // the simulated waiter gives up
	l.mu.Unlock()
	u()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the comment sync never got the lock")
	}
}
