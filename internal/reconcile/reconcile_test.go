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
	// change-abandoned: code only.
	want := []string{"branch/main", "comments/5", "comments/7", "branch/rel", "pr/9", "pr-comments/9"}
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

func TestPushersFromWebhooks(t *testing.T) {
	q := queue.New[Key]()
	r := NewRouter("p", q, slog.Default())
	r.Pushers = &Pushers{}
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "synchronize", PR: 3,
		Payload: []byte(`{"sender":{"login":"bob"},"pull_request":{"number":3,"head":{"sha":"abc"}}}`)})
	r.Delivery(&github.Delivery{Event: "push",
		Payload: []byte(`{"sender":{"login":"carol"},"after":"def","ref":"refs/heads/x"}`)})
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "edited", PR: 3,
		Payload: []byte(`{"sender":{"login":"mallory"},"pull_request":{"number":3,"head":{"sha":"abc"}}}`)})
	// Reopening is not pushing, and pushing someone's commit again does not
	// make it yours.
	r.Delivery(&github.Delivery{Event: "pull_request", Action: "reopened", PR: 4,
		Payload: []byte(`{"sender":{"login":"maint"},"pull_request":{"number":4,"head":{"sha":"ghi"}}}`)})
	r.Delivery(&github.Delivery{Event: "push",
		Payload: []byte(`{"sender":{"login":"maint"},"after":"abc","ref":"refs/heads/test-it"}`)})
	if r.Pushers.Lookup("abc") != "bob" || r.Pushers.Lookup("def") != "carol" || r.Pushers.Lookup("ghi") != "" {
		t.Errorf("abc=%q def=%q ghi=%q", r.Pushers.Lookup("abc"), r.Pushers.Lookup("def"), r.Pushers.Lookup("ghi"))
	}
	p := &Pushers{}
	for i := range maxPushers + 10 {
		p.Record(fmt.Sprint(i), "x")
	}
	if p.Lookup("0") != "" || p.Lookup(fmt.Sprint(maxPushers+9)) != "x" || len(p.bySHA) != maxPushers {
		t.Error("pusher log not bounded")
	}
}
