// Package reconcile decides what work to do and does it.
//
// Gerrit events and GitHub webhooks only say which key (change, PR, branch)
// may have changed; a worker then reconciles that key from the current state
// of both sides (design §4.1). This file holds the plumbing: keys, routing
// hints to keys, resync, and the worker loop. The sub-reconcilers that
// compute and apply actions come in later milestones; until then DryRun
// stands in.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
)

// Key is a unit of reconciliation. Exactly one field is set.
type Key struct {
	// Change is a Gerrit change number.
	Change int
	// PR is a GitHub PR number. The reconciler maps it to a change via
	// refs/bridge state, or imports it if it is new.
	PR int
	// Branch is a target branch that moved; changes stacked on it may
	// need their GitHub base updated.
	Branch string
}

func (k Key) String() string {
	switch {
	case k.Change != 0:
		return "change/" + strconv.Itoa(k.Change)
	case k.PR != 0:
		return "pr/" + strconv.Itoa(k.PR)
	default:
		return "branch/" + k.Branch
	}
}

// Reconciler brings one key into sync.
type Reconciler interface {
	Reconcile(ctx context.Context, k Key) error
}

// Lister lists the keys a full resync must visit.
type Lister interface {
	ResyncKeys(ctx context.Context) ([]Key, error)
}

// Router turns Gerrit events, GitHub webhooks and resync requests into
// queued keys. It implements events.Handler. None of its methods block.
type Router struct {
	Project string
	Queue   *queue.Queue[Key]
	Logger  *slog.Logger

	resync chan string
}

// NewRouter returns a Router for project feeding q.
func NewRouter(project string, q *queue.Queue[Key], logger *slog.Logger) *Router {
	return &Router{Project: project, Queue: q, Logger: logger, resync: make(chan string, 1)}
}

// Event queues the key a Gerrit event is about.
func (r *Router) Event(e *events.Event) {
	if e.Project() != r.Project {
		return
	}
	if e.Change != nil && e.Change.Branch != "" {
		// Changes are reconciled a whole target branch at a time, so
		// chains and stacks are planned together.
		r.Queue.Add(Key{Branch: e.Change.Branch})
		return
	}
	if e.Change != nil && e.Change.Number > 0 {
		r.Queue.Add(Key{Change: e.Change.Number})
		return
	}
	if e.RefUpdate == nil {
		return
	}
	ref := e.RefUpdate.RefName
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		r.Queue.Add(Key{Branch: strings.TrimPrefix(ref, "refs/heads/")})
	}
	// Everything else is ignored. Change refs (refs/changes/*, including
	// NoteDb meta) always come with a change event that names the branch.
	// refs/bridge/* are our own state writes, which must not wake us up.
}

// Resync requests a full resync. Requests made while one is pending are
// merged into it.
func (r *Router) Resync(reason string) {
	select {
	case r.resync <- reason:
	default:
	}
}

// Delivery queues the key a GitHub webhook is about.
func (r *Router) Delivery(d *github.Delivery) {
	if d.PR != 0 {
		r.Queue.Add(Key{PR: d.PR})
		return
	}
	r.Logger.Debug("webhook without a PR", "event", d.Event, "action", d.Action)
}

// RunResync performs requested resyncs until ctx is done: it lists every
// key to visit and queues it. A failed listing is retried with backoff, and
// keeps the request pending.
func (r *Router) RunResync(ctx context.Context, l Lister) error {
	const minBackoff, maxBackoff = time.Second, 2 * time.Minute
	backoff := minBackoff
	for {
		var reason string
		select {
		case <-ctx.Done():
			return ctx.Err()
		case reason = <-r.resync:
		}
		keys, err := l.ResyncKeys(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.Logger.Error("resync failed; retrying", "reason", reason, "err", err, "in", backoff)
			r.Resync(reason)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff
		for _, k := range keys {
			r.Queue.Add(k)
		}
		r.Logger.Info("resync queued", "reason", reason, "keys", len(keys))
	}
}

// Work runs n workers reconciling keys from q until q is shut down.
func Work(ctx context.Context, q *queue.Queue[Key], rec Reconciler, n int, logger *slog.Logger) {
	done := make(chan struct{})
	for range n {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				k, ok := q.Get()
				if !ok {
					return
				}
				reconcileOne(ctx, q, rec, k, logger)
			}
		}()
	}
	for range n {
		<-done
	}
}

func reconcileOne(ctx context.Context, q *queue.Queue[Key], rec Reconciler, k Key, logger *slog.Logger) {
	defer q.Done(k)
	defer func() {
		if p := recover(); p != nil {
			logger.Error("reconcile panicked", "key", k.String(), "panic", fmt.Sprint(p))
			q.Retry(k)
		}
	}()
	if err := rec.Reconcile(ctx, k); err != nil {
		if ctx.Err() != nil {
			return
		}
		d := q.Retry(k)
		logger.Warn("reconcile failed", "key", k.String(), "err", err, "retry_in", d.Round(time.Millisecond))
		return
	}
	q.Forget(k)
}
