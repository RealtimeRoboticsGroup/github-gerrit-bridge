// Package queue is a keyed, de-duplicating work queue.
//
// Events are only hints that something about a key (a change, a stack)
// may have changed; reconciliation reads the current state itself. So a key
// that is added many times before a worker picks it up is processed once,
// and a key that is added while it is being processed is processed once
// more afterwards, never concurrently.
package queue

import (
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Queue is a keyed work queue. The zero value is not usable; call New.
type Queue[K comparable] struct {
	mu   sync.Mutex
	cond *sync.Cond

	order      []K            // keys waiting for a worker, FIFO
	low        []K            // keys waiting only for AddLow, FIFO
	lowOnly    map[K]string   // dirty keys only AddLow asked for, and why
	sinceLow   int            // keys handed out from order since one from low
	dirty      map[K]struct{} // keys that need processing (queued or re-queued)
	processing map[K]struct{} // keys a worker holds
	failures   map[K]int      // consecutive failures, for Retry backoff
	started    map[K]time.Time
	queuedAt   map[K]time.Time // when a dirty key was first added
	waited     map[K]time.Duration
	errs       map[K]string // last error of a key that is failing
	details    map[K]any    // what a running key's run attached (Attach)
	ids        map[K]uint64 // a running key's run id
	resyncs    map[K]string // why a running key runs, if only for AddLow
	lastID     uint64
	recent     []Entry[K] // the last runs, oldest first (Snapshot)
	shutdown   bool

	// MinRetry and MaxRetry bound the exponential backoff used by Retry.
	MinRetry, MaxRetry time.Duration
	// MinInterval is the least time between the starts of two runs of
	// one key: a key added again and again (a stranger commenting
	// non-stop) runs at most this often, so it cannot spend the whole API
	// budget. Others are not delayed.
	MinInterval time.Duration
	// Throttled, if set, says which keys MinInterval applies to (say,
	// those strangers can cause); nil means all.
	Throttled func(K) bool
	// Urgent, if set, says which keys go to the next free worker ahead
	// of the others (in order among themselves).
	Urgent func(K) bool
}

// New returns an empty queue with a 1s..5m retry backoff.
func New[K comparable]() *Queue[K] {
	q := &Queue[K]{
		dirty:      map[K]struct{}{},
		lowOnly:    map[K]string{},
		processing: map[K]struct{}{},
		failures:   map[K]int{},
		started:    map[K]time.Time{},
		queuedAt:   map[K]time.Time{},
		waited:     map[K]time.Duration{},
		errs:       map[K]string{},
		details:    map[K]any{},
		ids:        map[K]uint64{},
		resyncs:    map[K]string{},
		MinRetry:   time.Second,
		MaxRetry:   5 * time.Minute,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add marks k as needing processing. It never blocks.
func (q *Queue[K]) Add(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return
	}
	if _, ok := q.dirty[k]; ok {
		if q.lowOnly[k] != "" {
			// Now an event's too: ahead of the rest of a resync.
			delete(q.lowOnly, k)
			if i := slices.Index(q.low, k); i >= 0 {
				q.low = slices.Delete(q.low, i, i+1)
				q.order = append(q.order, k)
			}
		}
		return
	}
	q.mark(k)
}

// AddLow is Add for a key nothing is known to have changed (a periodic
// resync visits them all), for why (non-empty: "periodic", say; a key
// that only AddLow queued runs with it as its Resync): it waits behind
// those that Add queued, but
// for one key in every lowShare handed out, so neither waits for good.
// Throttled keys cannot hold it back, then, nor it them (in the
// rehearsal, each resync held the PR events after it back minutes).
func (q *Queue[K]) AddLow(k K, why string) {
	if q.Urgent != nil && q.Urgent(k) {
		q.Add(k)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.dirty[k]; ok || q.shutdown {
		return
	}
	if why == "" {
		why = "resync"
	}
	q.lowOnly[k] = why
	q.mark(k)
}

// lowShare: one key in this many goes to AddLow's while both wait.
const lowShare = 4

// mark makes k dirty and queues it, unless a worker has it or it ran
// moments ago. q.mu is held.
func (q *Queue[K]) mark(k K) {
	q.dirty[k] = struct{}{}
	q.queuedAt[k] = time.Now()
	if _, ok := q.processing[k]; ok {
		// Done will re-queue it.
		return
	}
	if wait := q.wait(k); wait > 0 {
		// Ran moments ago: still dirty while it waits, so further Adds
		// do not queue it sooner.
		time.AfterFunc(wait, func() { q.requeue(k) })
		return
	}
	q.enqueue(k)
}

// enqueue puts dirty k in line for a worker. q.mu is held.
func (q *Queue[K]) enqueue(k K) {
	if q.lowOnly[k] != "" {
		q.low = append(q.low, k)
	} else {
		q.order = append(q.order, k)
	}
	q.cond.Broadcast() // GetUrgent waiters may not take it
}

// wait is how long throttled k must still wait since its last start, if
// MinInterval has not passed. q.mu is held.
func (q *Queue[K]) wait(k K) time.Duration {
	last, ok := q.started[k]
	if !ok || q.MinInterval <= 0 || (q.Throttled != nil && !q.Throttled(k)) {
		return 0
	}
	if w := q.MinInterval - time.Since(last); w > 0 {
		return w
	}
	if _, running := q.processing[k]; !running {
		delete(q.started, k) // long enough ago: forget it
	}
	return 0
}

// AddAfter adds k once d has elapsed.
func (q *Queue[K]) AddAfter(k K, d time.Duration) {
	if d <= 0 {
		q.Add(k)
		return
	}
	time.AfterFunc(d, func() { q.Add(k) })
}

// Retry re-adds k after an exponential, jittered backoff that grows with
// each consecutive Retry of k. Call Forget once k succeeds.
func (q *Queue[K]) Retry(k K) time.Duration {
	q.mu.Lock()
	n := q.failures[k]
	q.failures[k] = n + 1
	q.mu.Unlock()

	d := q.MinRetry
	for i := 0; i < n && d < q.MaxRetry; i++ {
		d *= 2
	}
	d = min(d, q.MaxRetry)
	// Jitter in [d/2, d) so retries of many keys after an outage spread out.
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	q.AddAfter(k, d)
	return d
}

// Forget clears k's Retry backoff.
func (q *Queue[K]) Forget(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.failures, k)
	delete(q.errs, k)
}

// Attach keeps d (a trace of the run, say) with k's current run, for
// Snapshot: while it runs, and among the recent runs after.
func (q *Queue[K]) Attach(k K, d any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.processing[k]; ok {
		q.details[k] = d
	}
}

// Failed records why k's last run failed, for Snapshot, until Forget.
func (q *Queue[K]) Failed(k K, err string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.errs[k] = err
}

// Get blocks until a key is available, and hands it to the caller, who must
// call Done(k) when finished. ok is false once the queue is shut down.
func (q *Queue[K]) Get() (k K, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) == 0 && len(q.low) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if q.shutdown {
		return k, false
	}
	if i := q.urgent(); i >= 0 {
		return q.take(i), true
	}
	if len(q.low) > 0 && (len(q.order) == 0 || q.sinceLow >= lowShare-1) {
		k = q.low[0]
		q.low = q.low[1:]
		q.sinceLow = 0
		return q.hand(k), true
	}
	q.sinceLow++
	return q.take(0), true
}

// GetUrgent is Get for Urgent keys only: it waits until one is queued. A
// worker that only takes these is never busy with something else when
// one comes.
func (q *Queue[K]) GetUrgent() (k K, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.shutdown {
			return k, false
		}
		if i := q.urgent(); i >= 0 {
			return q.take(i), true
		}
		q.cond.Wait()
	}
}

// urgent is the index of the first Urgent key queued, or -1. q.mu is held.
func (q *Queue[K]) urgent() int {
	if q.Urgent != nil {
		for i, o := range q.order {
			if q.Urgent(o) {
				return i
			}
		}
	}
	return -1
}

// take hands out the key at index i of order. q.mu is held.
func (q *Queue[K]) take(i int) K {
	k := q.order[i]
	q.order = append(q.order[:i:i], q.order[i+1:]...)
	return q.hand(k)
}

// hand hands out k, taken out of line. q.mu is held.
func (q *Queue[K]) hand(k K) K {
	delete(q.dirty, k)
	if why := q.lowOnly[k]; why != "" {
		q.resyncs[k] = why
		delete(q.lowOnly, k)
	}
	q.processing[k] = struct{}{}
	q.started[k] = time.Now()
	q.waited[k] = q.started[k].Sub(q.queuedAt[k])
	delete(q.queuedAt, k)
	q.lastID++
	q.ids[k] = q.lastID
	return k
}

// Waited is how long the key a worker holds waited to be handed out,
// from the first Add that made it dirty.
func (q *Queue[K]) Waited(k K) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waited[k]
}

// Resync is why the key a worker holds runs if only AddLow asked for
// the run (a periodic resync, say), or "".
func (q *Queue[K]) Resync(k K) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.resyncs[k]
}

// Done releases k. If k was added while it was being processed, it is
// queued again.
func (q *Queue[K]) Done(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if start, ok := q.started[k]; ok {
		q.recent = append(q.recent, Entry[K]{Key: k, ID: q.ids[k], At: start, Waited: q.waited[k], Took: time.Since(start), Resync: q.resyncs[k], Err: q.errs[k], Detail: q.details[k]})
		if len(q.recent) > maxRecent {
			q.recent = slices.Delete(q.recent, 0, len(q.recent)-maxRecent)
		}
	}
	delete(q.processing, k)
	delete(q.waited, k)
	delete(q.details, k)
	delete(q.ids, k)
	delete(q.resyncs, k)
	// started stays: it is k's last start, which throttles Adds that come
	// after this run as well as those during it. Entries are forgotten
	// once MinInterval has passed (wait), or swept here.
	if len(q.started) > 4096 {
		for o, t := range q.started {
			if _, running := q.processing[o]; !running && time.Since(t) >= q.MinInterval {
				delete(q.started, o)
			}
		}
	}
	if _, ok := q.dirty[k]; ok && !q.shutdown {
		// Still dirty while it waits, so Add does not queue it sooner.
		if wait := q.wait(k); wait > 0 {
			time.AfterFunc(wait, func() { q.requeue(k) })
			return
		}
		q.enqueue(k)
	}
}

func (q *Queue[K]) requeue(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.dirty[k]; ok && !q.shutdown {
		q.enqueue(k)
	}
}

// Len is the number of keys waiting for a worker.
func (q *Queue[K]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order) + len(q.low)
}

// maxRecent is how many finished runs Snapshot shows.
const maxRecent = 100

// Entry is one key in a Snapshot. At is when it started (running, or a
// recent run) or was first added (waiting or held).
type Entry[K comparable] struct {
	Key K
	// ID names a run (running, recent): each run of a key has its own.
	ID     uint64
	At     time.Time
	Waited time.Duration // before it started (running, recent)
	Took   time.Duration // recent
	// Resync is why, if only AddLow queued it (waiting, behind events)
	// or asked for the run (running, recent).
	Resync string
	// Again: held, it runs once more after its current run; else held,
	// it ran moments ago (MinInterval).
	Again bool
	// Failures and Err: consecutive failed runs, and the last one's error.
	Failures int
	Err      string
	// Detail is what the run attached (Attach).
	Detail any
}

// Snapshot is what the queue is doing.
type Snapshot[K comparable] struct {
	Running []Entry[K] // longest running first
	Waiting []Entry[K] // in the order workers will take them, mostly
	Held    []Entry[K] // added, but not in line yet
	Failing []Entry[K] // waiting to be retried, or failing again
	Recent  []Entry[K] // finished, newest first
}

// Snapshot reports what the queue is doing, for a status page.
func (q *Queue[K]) Snapshot() Snapshot[K] {
	q.mu.Lock()
	defer q.mu.Unlock()
	var s Snapshot[K]
	fail := func(e Entry[K]) Entry[K] {
		e.Failures, e.Err = q.failures[e.Key], q.errs[e.Key]
		return e
	}
	for k := range q.processing {
		s.Running = append(s.Running, fail(Entry[K]{Key: k, ID: q.ids[k], At: q.started[k], Waited: q.waited[k], Resync: q.resyncs[k], Detail: q.details[k]}))
	}
	slices.SortFunc(s.Running, func(a, b Entry[K]) int { return a.At.Compare(b.At) })
	inLine := map[K]bool{}
	for _, k := range q.order {
		inLine[k] = true
		s.Waiting = append(s.Waiting, fail(Entry[K]{Key: k, At: q.queuedAt[k]}))
	}
	for _, k := range q.low {
		inLine[k] = true
		s.Waiting = append(s.Waiting, fail(Entry[K]{Key: k, At: q.queuedAt[k], Resync: q.lowOnly[k]}))
	}
	for k := range q.dirty {
		if !inLine[k] {
			_, running := q.processing[k]
			s.Held = append(s.Held, fail(Entry[K]{Key: k, At: q.queuedAt[k], Again: running}))
		}
	}
	slices.SortFunc(s.Held, func(a, b Entry[K]) int { return a.At.Compare(b.At) })
	for k, n := range q.failures {
		if n > 0 {
			s.Failing = append(s.Failing, fail(Entry[K]{Key: k}))
		}
	}
	slices.SortFunc(s.Failing, func(a, b Entry[K]) int { return b.Failures - a.Failures })
	for i := len(q.recent) - 1; i >= 0; i-- {
		s.Recent = append(s.Recent, q.recent[i])
	}
	return s
}

// ShutDown makes Get return false in every worker, and drops pending keys.
func (q *Queue[K]) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	q.cond.Broadcast()
}
