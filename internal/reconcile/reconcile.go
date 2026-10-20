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
	"errors"
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
	// Branch: mirror the code (branches, PRs, stacks) of a target branch.
	Branch string
	// Comments: sync the comments of a Gerrit change.
	Comments int
	// PR: something about a PR changed; mirror its change's branch.
	PR int
	// PRComments: a PR's reviews or comments changed.
	PRComments int
	// Merged: a change merged in Gerrit; ready its PRs for replication
	// before anything else (design §6.7).
	Merged int
}

func (k Key) String() string {
	switch {
	case k.Comments != 0:
		return "comments/" + strconv.Itoa(k.Comments)
	case k.PR != 0:
		return "pr/" + strconv.Itoa(k.PR)
	case k.PRComments != 0:
		return "pr-comments/" + strconv.Itoa(k.PRComments)
	case k.Merged != 0:
		return "merged/" + strconv.Itoa(k.Merged)
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
	// Pushers, if set, records who pushed what from webhooks.
	Pushers *Pushers

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
	if e.Change != nil {
		// Code is reconciled a whole target branch at a time, so chains
		// and stacks are planned together.
		if e.Type == "change-merged" && e.Change.Number > 0 {
			r.Queue.Add(Key{Merged: e.Change.Number})
		}
		if e.Change.Branch != "" && e.Type != "comment-added" {
			r.Queue.Add(Key{Branch: e.Change.Branch})
		}
		// New comments come with replies and with uploads that publish
		// drafts.
		if e.Change.Number > 0 && (e.Type == "comment-added" || e.Type == "patchset-created") {
			r.Queue.Add(Key{Comments: e.Change.Number})
		}
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

// Urgent reports whether k races something: a merge's PRs must be ready
// before replication pushes it.
func Urgent(k Key) bool { return k.Merged != 0 }

// Throttled reports whether k runs at most every queue.MinInterval:
// everything but a merge's PRs. Anyone can cause events on either side
// (GitHub accounts, Gerrit sign-ups), and a merge has its own key, so
// nothing else races replication.
func Throttled(k Key) bool { return !Urgent(k) }

// Delivery queues the key a GitHub webhook is about.
func (r *Router) Delivery(d *github.Delivery) {
	if r.Pushers != nil {
		r.Pushers.FromDelivery(d)
	}
	if d.PR == 0 {
		r.Logger.Debug("webhook without a PR", "event", d.Event, "action", d.Action)
		return
	}
	switch d.Event {
	case "pull_request_review", "pull_request_review_comment", "pull_request_review_thread", "issue_comment":
		r.Queue.Add(Key{PRComments: d.PR})
	default:
		r.Queue.Add(Key{PR: d.PR})
	}
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

// Work runs n workers reconciling keys from q until q is shut down, plus
// one that takes only urgent keys (a merge racing replication), so they
// never wait for a worker to come free.
func Work(ctx context.Context, q *queue.Queue[Key], rec Reconciler, n int, logger *slog.Logger) {
	done := make(chan struct{})
	worker := func(get func() (Key, bool)) {
		defer func() { done <- struct{}{} }()
		for {
			k, ok := get()
			if !ok {
				return
			}
			reconcileOne(ctx, q, rec, k, logger)
		}
	}
	for range n {
		go worker(q.Get)
	}
	go worker(q.GetUrgent)
	for range n + 1 {
		<-done
	}
}

// errLater says a reconcile left work it may do only later (a comment
// edited too recently to act on as its author): its key comes back then,
// which is not a failure.
type errLater struct{ after time.Duration }

func (e *errLater) Error() string { return fmt.Sprintf("some of it waits %v", e.after) }

// waitFor is the errLater for work that waits d (nil if d is not
// positive).
func waitFor(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	return &errLater{after: d}
}

// editSettle is how long a comment must be unchanged before the bridge
// acts on it as its author where that submits or starts CI: anyone with
// write access can edit anyone's comment, and GraphQL, which says who
// last did, may lag REST by a little (design §9; sixth audit).
const editSettle = 30 * time.Second

func reconcileOne(ctx context.Context, q *queue.Queue[Key], rec Reconciler, k Key, logger *slog.Logger) {
	defer q.Done(k)
	defer func() {
		if p := recover(); p != nil {
			logger.Error("reconcile panicked", "key", k.String(), "panic", fmt.Sprint(p))
			q.Retry(k)
		}
	}()
	start := time.Now()
	err := rec.Reconcile(ctx, k)
	// Merges race replication (15s on AOS): slow or late runs are worth
	// seeing.
	if took, waited := time.Since(start), q.Waited(k); took+waited > 3*time.Second {
		logger.Info("slow reconcile", "key", k.String(), "waited", waited.Round(time.Millisecond), "took", took.Round(time.Millisecond))
	} else {
		logger.Debug("reconciled", "key", k.String(), "waited", waited.Round(time.Millisecond), "took", took.Round(time.Millisecond))
	}
	var later *errLater
	if errors.As(err, &later) {
		q.Forget(k)
		q.AddAfter(k, later.after)
		logger.Debug("reconcile waits", "key", k.String(), "for", later.after)
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d := q.Retry(k)
		logger.Warn("reconcile failed", "key", k.String(), "err", err, "retry_in", d.Round(time.Millisecond))
		return
	}
	q.Forget(k)
}
