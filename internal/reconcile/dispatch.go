package reconcile

import (
	"context"
	"log/slog"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Dispatcher routes each key to the code mirror or comment sync. Both take
// the target branch's lock, so they never interleave on one change's state.
type Dispatcher struct {
	Code     *Code
	Comments *Comments
	Commands *Commands
	Checks   *Checks
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
			// Maybe a new GitHub-born PR: reconcile the branch its chain
			// targets, which imports it.
			branch, err := d.targetBranch(ctx, pr)
			if err != nil || branch == "" {
				return err
			}
			// Only branches Gerrit has: a PR stacked on some other
			// GitHub branch is not the bridge's business.
			if ok, err := d.Code.Gerrit.BranchExists(ctx, d.Code.Project, branch); err != nil || !ok {
				if err == nil {
					d.Logger.Debug("PR targets a branch Gerrit does not have; ignoring", "pr", pr, "branch", branch)
				}
				return err
			}
			return d.Code.Reconcile(ctx, Key{Branch: branch})
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
	if err := d.Comments.Sync(ctx, n); err != nil {
		return err
	}
	if d.Commands != nil {
		if err := d.Commands.Run(ctx, n); err != nil {
			return err
		}
	}
	// Votes and CI results arrive as replies, so they ride along.
	return d.Checks.Sync(ctx, n)
}

// targetBranch follows PR bases down a stack to the branch the chain
// targets: the first base that is neither another open PR's head nor a
// bridge branch.
func (d *Dispatcher) targetBranch(ctx context.Context, pr int) (string, error) {
	p, err := d.Code.GitHub.GetPull(ctx, pr)
	if err != nil {
		return "", err
	}
	if p.State != "open" {
		return "", nil
	}
	open, err := d.Code.GitHub.ListPulls(ctx, "open")
	if err != nil {
		return "", err
	}
	baseOf := map[string]string{}
	for _, o := range open {
		baseOf[o.Head.Ref] = o.Base.Ref
	}
	base := p.Base.Ref
	for i := 0; i < 50; i++ {
		switch next, ok := baseOf[base]; {
		case strings.HasPrefix(base, headPrefix) || strings.HasPrefix(base, frozenPrefix):
			// On a bridge branch: find the Gerrit change behind it.
			for _, b := range d.Store.Branches() {
				for _, n := range d.Store.ChangesOnBranch(b) {
					if st, err := d.Store.Load(ctx, n); err == nil && st.State.HeadBranch == base {
						return b, nil
					}
				}
			}
			return "", nil
		case ok:
			base = next
		default:
			return base, nil
		}
	}
	return "", nil
}
