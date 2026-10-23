// Package trace records what one run of a key spent its time on: each
// request to Gerrit or GitHub, each git command, each wait for a lock,
// and the phases a reconcile marks, for the status page's timeline of a
// run. A Trace rides in the run's context; with none, recording costs
// nothing.
package trace

import (
	"context"
	"sync"
	"time"
)

// MaxSpans bounds what one Trace keeps; the rest are counted.
const MaxSpans = 1000

// Span is one thing a run did.
type Span struct {
	// Start is how long after the run started it began; Took how long it
	// took (-1 while it has not finished).
	Start, Took time.Duration
	// Depth is how many regions it is inside.
	Depth int
	// What it was ("github GET /repos/o/r/pulls/7 → 200"), and its error.
	What, Err string
}

// Trace is one run's spans.
type Trace struct {
	mu      sync.Mutex
	start   time.Time
	spans   []Span
	depth   int
	dropped int
}

// New starts a Trace now.
func New() *Trace { return &Trace{start: time.Now()} }

type key struct{}

// With returns ctx carrying t.
func With(ctx context.Context, t *Trace) context.Context { return context.WithValue(ctx, key{}, t) }

// From is ctx's Trace, or nil.
func From(ctx context.Context) *Trace {
	t, _ := ctx.Value(key{}).(*Trace)
	return t
}

// Add records that what began at start and took took, failing with err.
// A nil Trace records nothing.
func (t *Trace) Add(start time.Time, took time.Duration, what string, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.spans) >= MaxSpans {
		t.dropped++
		return
	}
	s := Span{Start: start.Sub(t.start), Took: took, Depth: t.depth, What: what}
	if err != nil {
		s.Err = err.Error()
	}
	t.spans = append(t.spans, s)
}

// Region records what from now until the returned func is called, and
// everything recorded meanwhile as inside it.
func Region(ctx context.Context, what string) func() {
	t := From(ctx)
	if t == nil {
		return func() {}
	}
	start := time.Now()
	t.mu.Lock()
	i := -1
	if len(t.spans) < MaxSpans {
		i = len(t.spans)
		t.spans = append(t.spans, Span{Start: start.Sub(t.start), Took: -1, Depth: t.depth, What: what})
	} else {
		t.dropped++
	}
	t.depth++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.depth--
		if i >= 0 {
			t.spans[i].Took = time.Since(start)
		}
	}
}

// Spans returns a copy of what t recorded, in the order begun, and how
// many it dropped.
func (t *Trace) Spans() ([]Span, int) {
	if t == nil {
		return nil, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Span(nil), t.spans...), t.dropped
}

// Started is when the run began.
func (t *Trace) Started() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.start
}
