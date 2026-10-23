package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Dispatcher routes each key to the code mirror or comment sync. Both take
// the target branch's lock, so they never interleave on one change's state:
// the code mirror alone, comment syncs shared with one another (one at a
// time per change), but a /submit alone.
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

	mu      sync.Mutex
	changes map[int]*sync.Mutex // comment syncs of one change, one at a time
}

// changeLock takes change n for its comment sync: comments/N and a PR's
// pr-comments key both sync it, and may run at once.
func (d *Dispatcher) changeLock(n int) func() {
	d.mu.Lock()
	if d.changes == nil {
		d.changes = map[int]*sync.Mutex{}
	}
	m := d.changes[n]
	if m == nil {
		m = &sync.Mutex{}
		d.changes[n] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
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
	later, alone, err := d.syncShared(ctx, n, st.State.Branch)
	if err != nil {
		return err
	}
	if !alone {
		return later
	}
	// A /submit is due: it runs with the branch to itself (PrepareSubmit
	// moves PRs and their bases), reading everything again.
	defer d.Code.lock(st.State.Branch)()
	if err := d.Commands.Run(ctx, n); errors.As(err, new(*errLater)) {
		return waitFor(max(laterOf(err), laterOf(later)))
	} else if err != nil {
		return err
	}
	return later
}

// syncShared syncs change n's comments, commands and checks with branch
// shared. alone says a command needs it alone (errNeedsBranch).
func (d *Dispatcher) syncShared(ctx context.Context, n int, branch string) (later error, alone bool, err error) {
	defer d.Code.rlock(branch)()
	defer d.changeLock(n)()
	// What waits (errLater) does not hold up the rest.
	var l *errLater
	if err := d.Comments.Sync(ctx, n); errors.As(err, &l) {
		later = err
	} else if err != nil {
		return nil, false, err
	}
	if d.Commands != nil {
		if err := d.Commands.run(ctx, n, false); errors.Is(err, errNeedsBranch) {
			alone = true
		} else if errors.As(err, &l) {
			later = waitFor(max(l.after, laterOf(later)))
		} else if err != nil {
			return nil, false, err
		}
	}
	// Votes and CI results arrive as replies, so they ride along.
	if d.Checks != nil {
		if err := d.Checks.Sync(ctx, n); err != nil {
			return nil, false, err
		}
	}
	return later, alone, nil
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
