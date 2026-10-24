package queue

import (
	"slices"
	"strings"
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

func TestMinIntervalOnlyForThrottledKeys(t *testing.T) {
	q := New[string]()
	q.MinInterval = time.Hour
	q.Throttled = func(k string) bool { return k == "spam" }
	q.Add("urgent")
	k, _ := q.Get()
	q.Add("urgent")
	q.Done(k)
	if q.Len() != 1 {
		t.Errorf("an unthrottled key waited: %d queued", q.Len())
	}
}

func TestWaited(t *testing.T) {
	q := New[string]()
	q.Add("k")
	time.Sleep(20 * time.Millisecond)
	q.Add("k") // still the first Add that counts
	k, _ := q.Get()
	if w := q.Waited(k); w < 20*time.Millisecond {
		t.Errorf("waited %v", w)
	}
	q.Done(k)
}

// GetUrgent hands out only urgent keys, waiting for one, and wakes for
// one queued behind others.
func TestGetUrgent(t *testing.T) {
	q := New[string]()
	q.Urgent = func(k string) bool { return strings.HasPrefix(k, "!") }
	got := make(chan string)
	go func() {
		for {
			k, ok := q.GetUrgent()
			if !ok {
				close(got)
				return
			}
			got <- k
			q.Done(k)
		}
	}()
	time.Sleep(20 * time.Millisecond) // GetUrgent waits first
	plain := make(chan string)
	go func() {
		k, _ := q.Get()
		q.Done(k)
		plain <- k
	}()
	time.Sleep(20 * time.Millisecond)
	q.Add("a") // wakes the plain Get, not just GetUrgent
	select {
	case k := <-got:
		t.Fatalf("GetUrgent took %q", k)
	case k := <-plain:
		if k != "a" {
			t.Fatalf("Get = %q", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not wake")
	}
	q.Add("a")
	q.Add("!m")
	select {
	case k := <-got:
		if k != "!m" {
			t.Fatalf("got %q", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetUrgent did not wake for an urgent key")
	}
	if k, _ := q.Get(); k != "a" {
		t.Errorf("Get = %q", k)
	}
	q.ShutDown()
	if _, ok := <-got; ok {
		t.Error("GetUrgent handed out a key after shutdown")
	}
}

func TestUrgentKeysFirst(t *testing.T) {
	q := New[string]()
	q.Urgent = func(k string) bool { return strings.HasPrefix(k, "!") }
	for _, k := range []string{"a", "b", "!merge1", "c", "!merge2"} {
		q.Add(k)
	}
	var got []string
	for q.Len() > 0 {
		k, _ := q.Get()
		got = append(got, k)
		q.Done(k)
	}
	if strings.Join(got, " ") != "!merge1 !merge2 a b c" {
		t.Errorf("order = %v", got)
	}
}

// Adds that come just after a run are throttled too: a key cannot run
// more often than MinInterval by timing its events between runs.
func TestMinIntervalBetweenRuns(t *testing.T) {
	q := New[string]()
	q.MinInterval = 200 * time.Millisecond
	q.Add("spam")
	k, _ := q.Get()
	start := time.Now()
	q.Done(k)
	q.Add("spam") // just after the run
	if q.Len() != 0 {
		t.Fatal("re-queued at once")
	}
	k, _ = q.Get()
	if time.Since(start) < q.MinInterval {
		t.Errorf("ran again after %v", time.Since(start))
	}
	q.Done(k)
}

// AddLow's keys wait behind Add's, but for one in every lowShare handed
// out; an Add of one waiting puts it with Add's.
func TestAddLow(t *testing.T) {
	q := New[int]()
	for k := 100; k < 104; k++ {
		q.AddLow(k, "periodic")
	}
	for k := 1; k <= 7; k++ {
		q.Add(k)
	}
	q.Add(103)
	q.AddLow(1, "periodic") // queued already
	var got []int
	for q.Len() > 0 {
		k, _ := q.Get()
		got = append(got, k)
		q.Done(k)
	}
	want := []int{1, 2, 3, 100, 4, 5, 6, 101, 7, 103, 102}
	if !slices.Equal(got, want) {
		t.Errorf("handed out %v, want %v", got, want)
	}
}

// Added low while a worker has it, it comes back low; added by Add too,
// with Add's.
func TestAddLowWhileProcessing(t *testing.T) {
	for _, add := range []bool{false, true} {
		q := New[int]()
		q.Add(1)
		k, _ := q.Get()
		q.AddLow(1, "periodic")
		if add {
			q.Add(1)
		}
		q.Done(k)
		q.Add(2)
		if k, _ := q.Get(); (k == 1) != add {
			t.Errorf("Add too %v: got %d first", add, k)
		}
	}
}

// Snapshot shows each key where it is: running, waiting in the order
// workers take them (events before a resync's), held, failing with its
// error, and done, newest first.
func TestSnapshot(t *testing.T) {
	q := New[string]()
	q.MinInterval = time.Hour
	q.Add("a")
	q.Add("b")
	q.AddLow("r", "periodic")
	q.Add("c")
	if k, _ := q.Get(); k != "a" {
		t.Fatalf("got %q", k)
	}
	q.Add("a") // again while it runs
	s := q.Snapshot()
	keys := func(es []Entry[string]) (out []string) {
		for _, e := range es {
			out = append(out, e.Key)
		}
		return out
	}
	if got := keys(s.Running); !slices.Equal(got, []string{"a"}) || s.Running[0].At.IsZero() {
		t.Errorf("running %v", s.Running)
	}
	if got := keys(s.Waiting); !slices.Equal(got, []string{"b", "c", "r"}) || s.Waiting[0].Resync != "" || s.Waiting[2].Resync != "periodic" {
		t.Errorf("waiting %+v", s.Waiting)
	}
	if len(s.Held) != 1 || s.Held[0].Key != "a" || !s.Held[0].Again {
		t.Errorf("held %+v", s.Held)
	}

	q.Failed("a", "boom")
	q.Retry("a")
	q.Done("a") // ran at most an hour ago: held, not in line
	s = q.Snapshot()
	if len(s.Failing) != 1 || s.Failing[0].Key != "a" || s.Failing[0].Failures != 1 || s.Failing[0].Err != "boom" {
		t.Errorf("failing %+v", s.Failing)
	}
	if len(s.Held) != 1 || s.Held[0].Key != "a" || s.Held[0].Again || time.Until(s.Held[0].Ready) < 59*time.Minute {
		t.Errorf("held after its run %+v; want it ready an hour after it started", s.Held)
	}
	k, _ := q.Get()
	q.Forget(k)
	q.Done(k)
	s = q.Snapshot()
	if got := keys(s.Recent); !slices.Equal(got, []string{k, "a"}) || s.Recent[1].Err != "boom" || s.Recent[0].Err != "" || s.Recent[1].Took <= 0 {
		t.Errorf("recent %+v", s.Recent)
	}
	if len(s.Failing) != 1 {
		t.Errorf("a's failure forgotten with %s's success: %+v", k, s.Failing)
	}
	for range maxRecent + 5 {
		q.Add("x")
		k, _ := q.Get()
		q.Done(k)
		q.mu.Lock()
		delete(q.started, k) // not throttled
		q.mu.Unlock()
	}
	if n := len(q.Snapshot().Recent); n != maxRecent {
		t.Errorf("%d recent runs kept", n)
	}

	// What a run attaches stays with that run, and each run has its id.
	q = New[string]()
	q.Add("k")
	q.Get()
	q.Attach("k", "first")
	if r := q.Snapshot().Running; len(r) != 1 || r[0].Detail != "first" || r[0].ID == 0 {
		t.Errorf("running %+v", r)
	}
	q.Done("k")
	q.Attach("k", "late") // not running: kept nowhere
	q.Add("k")
	q.Get()
	q.Done("k")
	if r := q.Snapshot().Recent; len(r) != 2 || r[0].Detail != nil || r[1].Detail != "first" || r[0].ID == r[1].ID {
		t.Errorf("recent %+v", r)
	}

	// A run that succeeds after a failure shows no error.
	q = New[string]()
	q.Add("k")
	q.Get()
	q.Failed("k", "boom")
	q.Done("k")
	q.Add("k")
	q.Get()
	q.Forget("k")
	q.Done("k")
	if r := q.Snapshot().Recent; len(r) != 2 || r[0].Err != "" || r[1].Err != "boom" {
		t.Errorf("recent after a success %+v", r)
	}
}

// A run only AddLow asked for carries its reason, running and after; one
// an Add asked for too does not.
func TestResyncReason(t *testing.T) {
	q := New[string]()
	q.AddLow("r", "periodic")
	q.AddLow("e", "connected")
	q.Add("e") // an event's too
	q.AddLow("d", "")
	got := map[string]string{}
	for range 3 {
		k, _ := q.Get()
		got[k] = q.Resync(k)
		if s := q.Snapshot(); s.Running[0].Resync != got[k] {
			t.Errorf("%s running as %q, Resync says %q", k, s.Running[0].Resync, got[k])
		}
		q.Done(k)
	}
	if got["r"] != "periodic" || got["e"] != "" || got["d"] != "resync" {
		t.Errorf("reasons %q", got)
	}
	if q.Resync("r") != "" {
		t.Error("a finished run still has a reason")
	}
	for _, e := range q.Snapshot().Recent {
		if e.Resync != got[e.Key] {
			t.Errorf("recent %s: %q, want %q", e.Key, e.Resync, got[e.Key])
		}
	}
}
