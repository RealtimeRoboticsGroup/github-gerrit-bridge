package reconcile

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Events on PRs too old to import cost nothing more than the PR lookup.
func TestTargetBranchSkipsPRsTooOldToImport(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	old := s.pr(5, "feature", "abc", "main", "stranger")
	old.CreatedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Importer = &Importer{CreatedAfter: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	d := &Dispatcher{Code: c, Store: store}
	if b, err := d.targetBranch(ctx, 5); err != nil || b != "" {
		t.Errorf("old PR: %q %v", b, err)
	}
	old.CreatedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if b, err := d.targetBranch(ctx, 5); err != nil || b != "main" {
		t.Errorf("new PR: %q %v", b, err)
	}
}

// A PR's event queues its branch's key, which the queue merges with every
// other PR's, rather than reconciling the branch for each (fourth audit).
func TestPRKeysQueueTheirBranch(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	s.push("gerrit.git", s.commit("", "a.txt", "a\n", "base"), "refs/heads/main")
	s.pr(5, "feature", "abc", "main", "stranger")
	s.save(ctx, store, 7, &state.Change{State: state.ChangeState{Change: 7, PR: 70, Branch: "main"}})
	var queued []Key
	d := &Dispatcher{Code: c, Store: store, Enqueue: func(k Key) { queued = append(queued, k) }}
	for _, k := range []Key{{PR: 5}, {PR: 70}} {
		if err := d.Reconcile(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(queued, []Key{{Branch: "main"}, {Branch: "main"}}) || len(s.calls) != 0 {
		t.Errorf("queued %v, calls %v; want the branch queued for each, nothing reconciled", queued, s.calls)
	}
}
