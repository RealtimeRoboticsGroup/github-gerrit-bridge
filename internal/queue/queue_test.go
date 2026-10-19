package queue

import (
	"sync"
	"testing"
	"time"
)

func TestDedup(t *testing.T) {
	q := New[int]()
	q.Add(1)
	q.Add(2)
	q.Add(1)
	if got := q.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	for _, want := range []int{1, 2} {
		k, ok := q.Get()
		if !ok || k != want {
			t.Fatalf("Get = %v, %v; want %v, true", k, ok, want)
		}
		q.Done(k)
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

func TestAddWhileProcessing(t *testing.T) {
	q := New[string]()
	q.Add("a")
	k, _ := q.Get()

	// Added while held: must not be handed to another worker yet.
	q.Add("a")
	q.Add("a")
	if got := q.Len(); got != 0 {
		t.Fatalf("Len while processing = %d, want 0", got)
	}

	q.Done(k)
	if got := q.Len(); got != 1 {
		t.Fatalf("Len after Done = %d, want 1 (re-queued once)", got)
	}
	k, _ = q.Get()
	q.Done(k)
	if got := q.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

func TestNeverConcurrent(t *testing.T) {
	q := New[int]()
	var mu sync.Mutex
	active := map[int]bool{}
	processed := 0

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k, ok := q.Get()
				if !ok {
					return
				}
				mu.Lock()
				if active[k] {
					t.Errorf("key %d processed concurrently", k)
				}
				active[k] = true
				processed++
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				active[k] = false
				mu.Unlock()
				q.Done(k)
			}
		}()
	}
	for i := range 1000 {
		q.Add(i % 3)
	}
	for q.Len() > 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	q.ShutDown()
	wg.Wait()
	if processed < 3 {
		t.Errorf("processed %d keys, want at least 3", processed)
	}
}

func TestShutDownUnblocksGet(t *testing.T) {
	q := New[int]()
	done := make(chan bool)
	go func() {
		_, ok := q.Get()
		done <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	q.ShutDown()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Get returned ok after ShutDown")
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not return after ShutDown")
	}
	q.Add(1)
	if q.Len() != 0 {
		t.Fatal("Add after ShutDown queued a key")
	}
}

func TestRetryBackoff(t *testing.T) {
	q := New[int]()
	q.MinRetry = 10 * time.Millisecond
	q.MaxRetry = 40 * time.Millisecond

	for i, wantMax := range []time.Duration{10, 20, 40, 40, 40} {
		wantMax *= time.Millisecond
		d := q.Retry(7)
		if d < wantMax/2 || d > wantMax {
			t.Errorf("retry %d: delay %v not in [%v, %v]", i, d, wantMax/2, wantMax)
		}
	}

	deadline := time.Now().Add(time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if k, ok := q.Get(); !ok || k != 7 {
		t.Fatalf("Get = %v, %v; want 7 after retry", k, ok)
	}

	q.Forget(7)
	if d := q.Retry(7); d > 10*time.Millisecond {
		t.Errorf("after Forget, delay %v > MinRetry", d)
	}
}

// A key added again and again runs at most once per MinInterval; other
// keys are not held up.
func TestMinInterval(t *testing.T) {
	q := New[string]()
	q.MinInterval = 200 * time.Millisecond
	q.Add("spam")
	k, _ := q.Get()
	start := time.Now()
	q.Add("spam") // while processing
	q.Done(k)
	q.Add("other")
	if k, _ := q.Get(); k != "other" {
		t.Fatalf("got %q first, want other (spam waits)", k)
	}
	q.Done("other")
	q.Add("spam") // while it waits: still once
	k, _ = q.Get()
	if k != "spam" || time.Since(start) < q.MinInterval {
		t.Errorf("got %q after %v", k, time.Since(start))
	}
	q.Done(k)
	if q.Len() != 0 {
		t.Errorf("queued %d more", q.Len())
	}
}
