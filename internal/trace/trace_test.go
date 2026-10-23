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
