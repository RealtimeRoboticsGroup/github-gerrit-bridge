package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

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
	// Enqueue, if set, queues a key. A PR's event queues its branch's key
	// rather than reconciling the branch there and then: the queue merges
	// keys, so many PRs' events cost one branch reconcile, not one each
	// (fourth audit).
	Enqueue func(Key)
}

// branch reconciles branch, or queues it.
func (d *Dispatcher) branch(ctx context.Context, branch string) error {
	if d.Enqueue != nil {
		d.Enqueue(Key{Branch: branch})
		return nil
	}
	return d.Code.Reconcile(ctx, Key{Branch: branch})
}

// Reconcile implements Reconciler.
func (d *Dispatcher) Reconcile(ctx context.Context, k Key) error {
	switch {
	case k.Branch != "":
		return d.Code.Reconcile(ctx, k)
	case k.Comments != 0:
		return d.syncComments(ctx, k.Comments)
	case k.Merged != 0:
		// Just the PR heads and bases (Code.PrepareSubmit), ahead of
		// everything waiting: replication pushes the merge to GitHub
		// within seconds, and only PRs based on the branch, with Gerrit's
		// commits as heads, then show merged. The branch's full reconcile
		// follows.
		st, err := d.Store.Load(ctx, k.Merged)
		if err != nil || st.State.PR == 0 {
			return err
		}
		defer d.Code.lockUrgent(st.State.Branch)()
		return d.Code.PrepareSubmit(ctx, k.Merged)
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
			return d.branch(ctx, branch)
		}
		if k.PRComments != 0 {
			return d.syncComments(ctx, n)
		}
		st, err := d.Store.Load(ctx, n)
		if err != nil {
			return err
		}
		return d.branch(ctx, st.State.Branch)
	}
	return nil
}

func (d *Dispatcher) syncComments(ctx context.Context, n int) error {
	st, err := d.Store.Load(ctx, n)
	if err != nil || st.State.PR == 0 {
		return err
	}
	defer d.Code.lock(st.State.Branch)()
	// What waits (errLater) does not hold up the rest.
	var later error
	var l *errLater
	if err := d.Comments.Sync(ctx, n); errors.As(err, &l) {
		later = err
	} else if err != nil {
		return err
	}
	if d.Commands != nil {
		if err := d.Commands.Run(ctx, n); errors.As(err, &l) {
			later = waitFor(max(l.after, laterOf(later)))
		} else if err != nil {
			return err
		}
	}
	// Votes and CI results arrive as replies, so they ride along.
	if d.Checks != nil {
		if err := d.Checks.Sync(ctx, n); err != nil {
			return err
		}
	}
	return later
}

// laterOf is how long err, an errLater or nil, waits.
func laterOf(err error) time.Duration {
	var l *errLater
	if errors.As(err, &l) {
		return l.after
	}
	return 0
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
	// Too old to import: its events are nothing to the bridge, and must
	// not each cost a branch reconcile.
	if im := d.Code.Importer; im != nil && p.CreatedAt.Before(im.CreatedAfter) {
		return "", nil
	}
	open, err := d.Code.GitHub.ListPulls(ctx, "open")
	if err != nil {
		return "", err
	}
	// Only PRs from the repository's own branches can be bases; a fork's
	// branch of the same name must not redirect the walk.
	repo := d.Code.GitHub.Owner + "/" + d.Code.GitHub.Repo
	baseOf := map[string]string{}
	for _, o := range open {
		if o.HeadKey(repo) == o.Head.Ref {
			baseOf[o.Head.Ref] = o.Base.Ref
		}
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
