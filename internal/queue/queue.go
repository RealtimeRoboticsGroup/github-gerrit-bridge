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
	"sync"
	"time"
)

// Queue is a keyed work queue. The zero value is not usable; call New.
type Queue[K comparable] struct {
	mu   sync.Mutex
	cond *sync.Cond

	order      []K            // keys waiting for a worker, FIFO
	dirty      map[K]struct{} // keys that need processing (queued or re-queued)
	processing map[K]struct{} // keys a worker holds
	failures   map[K]int      // consecutive failures, for Retry backoff
	started    map[K]time.Time
	queuedAt   map[K]time.Time // when a dirty key was first added
	waited     map[K]time.Duration
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
}

// New returns an empty queue with a 1s..5m retry backoff.
func New[K comparable]() *Queue[K] {
	q := &Queue[K]{
		dirty:      map[K]struct{}{},
		processing: map[K]struct{}{},
		failures:   map[K]int{},
		started:    map[K]time.Time{},
		queuedAt:   map[K]time.Time{},
		waited:     map[K]time.Duration{},
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
		return
	}
	q.dirty[k] = struct{}{}
	q.queuedAt[k] = time.Now()
	if _, ok := q.processing[k]; ok {
		// Done will re-queue it.
		return
	}
	q.order = append(q.order, k)
	q.cond.Signal()
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
}

// Get blocks until a key is available, and hands it to the caller, who must
// call Done(k) when finished. ok is false once the queue is shut down.
func (q *Queue[K]) Get() (k K, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if q.shutdown {
		return k, false
	}
	k = q.order[0]
	var zero K
	q.order[0] = zero
	q.order = q.order[1:]
	delete(q.dirty, k)
	q.processing[k] = struct{}{}
	q.started[k] = time.Now()
	q.waited[k] = q.started[k].Sub(q.queuedAt[k])
	delete(q.queuedAt, k)
	return k, true
}

// Waited is how long the key a worker holds waited to be handed out,
// from the first Add that made it dirty.
func (q *Queue[K]) Waited(k K) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waited[k]
}

// Done releases k. If k was added while it was being processed, it is
// queued again.
func (q *Queue[K]) Done(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, k)
	started := q.started[k]
	delete(q.started, k)
	delete(q.waited, k)
	if _, ok := q.dirty[k]; ok && !q.shutdown {
		// Still dirty while it waits, so Add does not queue it sooner.
		if wait := q.MinInterval - time.Since(started); wait > 0 && (q.Throttled == nil || q.Throttled(k)) {
			time.AfterFunc(wait, func() { q.requeue(k) })
			return
		}
		q.order = append(q.order, k)
		q.cond.Signal()
	}
}

func (q *Queue[K]) requeue(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.dirty[k]; ok && !q.shutdown {
		q.order = append(q.order, k)
		q.cond.Signal()
	}
}

// Len is the number of keys waiting for a worker.
func (q *Queue[K]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// ShutDown makes Get return false in every worker, and drops pending keys.
func (q *Queue[K]) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	q.cond.Broadcast()
}
