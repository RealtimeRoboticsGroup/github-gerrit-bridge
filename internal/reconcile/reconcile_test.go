package reconcile

import (
	"context"
	"errors"
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
		`{"type":"comment-added","change":{"project":"other","number":6}}`,
		`{"type":"comment-added","change":{"project":"p","number":5}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/heads/main"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/changes/34/1234/meta"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/bridge/changes/34/1234"}}`,
		`{"type":"ref-updated","refUpdate":{"project":"p","refName":"refs/tags/v1"}}`,
	} {
		r.Event(parse(t, s))
	}
	r.Delivery(&github.Delivery{PR: 9})
	r.Delivery(&github.Delivery{Event: "push"})
	want := []string{"branch/main", "branch/rel", "change/5", "pr/9"}
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
	return []Key{{Change: 1}, {Change: 2}, {Change: 3}}, nil
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
	if got := drain(q); !equal(got, []string{"change/1", "change/2", "change/3"}) {
		t.Errorf("queued %v", got)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls != 1 {
		t.Errorf("listed %d times, want 1", l.calls)
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
	case k.Change == 1 && f.calls[k] == 1:
		return errors.New("transient")
	case k.Change == 2 && f.calls[k] == 1:
		panic("bug")
	}
	return nil
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
	q.Add(Key{Change: 1})
	q.Add(Key{Change: 2})
	q.Add(Key{Change: 3})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		ok := f.calls[Key{Change: 1}] == 2 && f.calls[Key{Change: 2}] == 2 && f.calls[Key{Change: 3}] == 1
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
	if f.calls[Key{Change: 1}] != 2 || f.calls[Key{Change: 2}] != 2 || f.calls[Key{Change: 3}] != 1 {
		t.Errorf("calls = %v; want an error and a panic each retried once", f.calls)
	}
}
