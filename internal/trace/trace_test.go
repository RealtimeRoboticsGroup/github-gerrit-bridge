package trace

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Spans nest inside regions, a region still open says so, and a Trace
// keeps at most MaxSpans, counting the rest.
func TestTrace(t *testing.T) {
	tr := New()
	ctx := With(context.Background(), tr)
	end := Region(ctx, "observe")
	From(ctx).Add(time.Now(), 5*time.Millisecond, "github GET /pulls", nil)
	open := Region(ctx, "wait for branch main")
	spans, _ := tr.Spans()
	if len(spans) != 3 || spans[0].Took != -1 || spans[1].Depth != 1 || spans[2].Depth != 1 || spans[2].Took != -1 {
		t.Fatalf("spans %+v", spans)
	}
	open()
	end()
	From(ctx).Add(time.Now(), 0, "git fetch", errors.New("boom"))
	spans, _ = tr.Spans()
	if spans[0].Took < 0 || spans[2].Took < 0 || spans[3].Depth != 0 || spans[3].Err != "boom" {
		t.Errorf("spans %+v", spans)
	}
	for range MaxSpans {
		tr.Add(time.Now(), 0, "x", nil)
	}
	if spans, dropped := tr.Spans(); len(spans) != MaxSpans || dropped != 4 {
		t.Errorf("%d spans kept, %d dropped", len(spans), dropped)
	}
	// Without a Trace, nothing happens.
	var none *Trace
	none.Add(time.Now(), 0, "x", nil)
	Region(context.Background(), "x")()
}

// A run's writes are named up to MaxWrites and all counted; with no
// Trace, Wrote records nothing.
func TestWrites(t *testing.T) {
	tr := New()
	ctx := With(context.Background(), tr)
	Wrote(ctx, "github POST /pulls")
	if w, n := tr.Writes(); n != 1 || len(w) != 1 || w[0] != "github POST /pulls" {
		t.Errorf("writes %q, %d", w, n)
	}
	for range MaxWrites + 2 {
		Wrote(ctx, "x")
	}
	if w, n := tr.Writes(); n != MaxWrites+3 || len(w) != MaxWrites || w[0] != "github POST /pulls" {
		t.Errorf("%d writes named, %d counted", len(w), n)
	}
	Wrote(context.Background(), "x")
	var none *Trace
	if w, n := none.Writes(); w != nil || n != 0 {
		t.Errorf("nil trace writes %q, %d", w, n)
	}
}
