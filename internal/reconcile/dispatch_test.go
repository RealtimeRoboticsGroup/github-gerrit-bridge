package reconcile

import (
	"context"
	"testing"
	"time"
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
