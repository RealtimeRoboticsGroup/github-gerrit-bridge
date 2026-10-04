package reconcile

import (
	"context"
	"log/slog"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Dispatcher routes each key to the code mirror or comment sync. Both take
// the target branch's lock, so they never interleave on one change's state.
type Dispatcher struct {
	Code     *Code
	Comments *Comments
	Store    *state.Store
	Logger   *slog.Logger
}

// Reconcile implements Reconciler.
func (d *Dispatcher) Reconcile(ctx context.Context, k Key) error {
	switch {
	case k.Branch != "":
		return d.Code.Reconcile(ctx, k)
	case k.Comments != 0:
		return d.syncComments(ctx, k.Comments)
	case k.PR != 0, k.PRComments != 0:
		pr := max(k.PR, k.PRComments)
		n := d.Store.ChangeForPR(pr)
		if n == 0 {
			// Not a mirrored PR (GitHub-born PRs come with milestone 5).
			d.Logger.Debug("PR not linked to a Gerrit change; ignoring", "pr", pr)
			return nil
		}
		if k.PRComments != 0 {
			return d.syncComments(ctx, n)
		}
		st, err := d.Store.Load(ctx, n)
		if err != nil {
			return err
		}
		return d.Code.Reconcile(ctx, Key{Branch: st.State.Branch})
	}
	return nil
}

func (d *Dispatcher) syncComments(ctx context.Context, n int) error {
	st, err := d.Store.Load(ctx, n)
	if err != nil || st.State.PR == 0 {
		return err
	}
	defer d.Code.lock(st.State.Branch)()
	return d.Comments.Sync(ctx, n)
}
