package reconcile

import (
	"context"
	"slices"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// What GitHub showed when someone commented, reviewed or posted /submit
// is what they can have read. A revision is recorded once, when it first
// reached GitHub, so it cannot say that: a PR's branch can go back to an
// older patch set's commit, or on to one the bridge does not import,
// which records nothing (seventh audit: a maintainer reading the older
// commit's diff had their retest build the newer one). Heads are every
// commit the branch was seen at, from when.

// maxHeads bounds the heads a change's state keeps.
const maxHeads = 100

// withHead is heads with sha appended, seen at at, unless sha is the last
// one already.
func withHead(heads []state.Head, sha string, at time.Time) []state.Head {
	if sha == "" || (len(heads) > 0 && heads[len(heads)-1].SHA == sha) {
		return heads
	}
	heads = append(heads, state.Head{SHA: sha, At: at})
	if len(heads) > maxHeads {
		heads = heads[len(heads)-maxHeads:]
	}
	return heads
}

// noteHead records that change n's PR has head sha, as seen now.
func noteHead(ctx context.Context, store *state.Store, n int, sha string, now time.Time) error {
	st, err := store.Load(ctx, n)
	if err != nil {
		return err
	}
	if len(st.Heads) > 0 && st.Heads[len(st.Heads)-1].SHA == sha {
		return nil
	}
	return updateStateFn(ctx, store, n, "PR head "+short(sha), func(st *state.Change) { st.Heads = withHead(st.Heads, sha, now) })
}

// headThroughout is the commit heads say the branch was at from from to
// to, and "" if it was seen at another in that time, or at none by from.
// A head is seen once it is there, so it may have been there for a while
// before: only "it was there all along" is known.
func headThroughout(heads []state.Head, from, to time.Time) string {
	heads = slices.Clone(heads)
	slices.SortStableFunc(heads, func(a, b state.Head) int { return a.At.Compare(b.At) })
	sha := ""
	for _, h := range heads {
		switch {
		case !h.At.After(from):
			sha = h.SHA
		case !h.At.After(to) && h.SHA != sha:
			return ""
		}
	}
	return sha
}
