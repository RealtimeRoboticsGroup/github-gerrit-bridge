package reconcile

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

// ResyncTally counts what resyncs caught: runs that only a resync asked
// for, and of those, the ones that changed something (trace.Wrote), which
// an event should have done. Few catches say resyncs could be rarer.
type ResyncTally struct {
	mu     sync.Mutex
	since  time.Time
	counts map[ResyncKind]*ResyncCount
	caught []Caught // oldest first
}

// ResyncKind is what a count is of: why the resync ran ("periodic",
// "connected") and what kind of key ("branch", "comments").
type ResyncKind struct{ Reason, Key string }

// ResyncCount is a kind's runs, how many of them wrote, and how many
// failed.
type ResyncCount struct{ Runs, Caught, Failed int }

// Caught is one run a resync asked for that wrote.
type Caught struct {
	At     time.Time
	Key    Key
	Reason string
	// Writes names what it wrote, up to trace.MaxWrites; N counts them all.
	Writes []string
	N      int
}

// maxCaught is how many catches a tally keeps.
const maxCaught = 50

// NewResyncTally starts counting now.
func NewResyncTally() *ResyncTally {
	return &ResyncTally{since: time.Now(), counts: map[ResyncKind]*ResyncCount{}}
}

// record counts a run of k a resync asked for (why), which t traced and
// which failed with err. It reports whether the run wrote. A nil tally
// counts nothing.
func (r *ResyncTally) record(why string, k Key, t *trace.Trace, err error) bool {
	if r == nil {
		return false
	}
	writes, n := t.Writes()
	kind, _, _ := strings.Cut(k.String(), "/")
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counts[ResyncKind{why, kind}]
	if c == nil {
		c = &ResyncCount{}
		r.counts[ResyncKind{why, kind}] = c
	}
	c.Runs++
	if err != nil {
		c.Failed++
	}
	if n == 0 {
		return false
	}
	c.Caught++
	r.caught = append(r.caught, Caught{At: time.Now(), Key: k, Reason: why, Writes: writes, N: n})
	if len(r.caught) > maxCaught {
		r.caught = slices.Delete(r.caught, 0, len(r.caught)-maxCaught)
	}
	return true
}

// Since is when the tally started counting.
func (r *ResyncTally) Since() time.Time { return r.since }

// Counts returns the counts by kind.
func (r *ResyncTally) Counts() map[ResyncKind]ResyncCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[ResyncKind]ResyncCount{}
	for k, c := range r.counts {
		out[k] = *c
	}
	return out
}

// Caught returns the last catches, newest first.
func (r *ResyncTally) Caught() []Caught {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.caught)
	slices.Reverse(out)
	return out
}
