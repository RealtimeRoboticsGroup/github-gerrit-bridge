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

// withHead is heads, in time order, with sha seen at at, unless the one
// before at is sha already. at is when GitHub was read, which may be well
// before the bridge gets to record it (an upload takes a while): placed by
// that, a head read before one recorded since cannot hide that one.
func withHead(heads []state.Head, sha string, at time.Time) []state.Head {
	i := len(heads)
	for i > 0 && heads[i-1].At.After(at) {
		i--
	}
	if sha == "" || (i > 0 && heads[i-1].SHA == sha) {
		return heads
	}
	heads = slices.Insert(slices.Clone(heads), i, state.Head{SHA: sha, At: at})
	if len(heads) > maxHeads {
		heads = heads[len(heads)-maxHeads:]
	}
	return heads
}

// noteHead records that change n's PR had head sha when GitHub was read,
// at at.
func noteHead(ctx context.Context, store *state.Store, n int, sha string, at time.Time) error {
	st, err := store.Load(ctx, n)
	if err != nil {
		return err
	}
	if slices.Equal(withHead(st.Heads, sha, at), st.Heads) {
		return nil
	}
	return updateStateFn(ctx, store, n, "PR head "+short(sha), func(st *state.Change) { st.Heads = withHead(st.Heads, sha, at) })
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
