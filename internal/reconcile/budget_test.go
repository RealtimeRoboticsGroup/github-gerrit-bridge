package reconcile

import (
	"context"
	"testing"
	"time"
)

func TestBudget(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	b := &Budget{N: 2, Per: time.Hour, now: func() time.Time { return now },
		Trusted: func(_ context.Context, account int) (bool, bool) { return account == 7, true }}
	ctx := context.Background()
	if !b.Take(ctx, 1) || b.Left(ctx, 1) != 1 || !b.Take(ctx, 1) || b.Take(ctx, 1) || b.Left(ctx, 1) != 0 {
		t.Error("account 1 got more than 2 an hour")
	}
	if !b.Take(ctx, 2) {
		t.Error("account 2 limited by account 1")
	}
	for range 10 {
		if !b.Take(ctx, 7) {
			t.Fatal("a trusted account was limited")
		}
	}
	now = now.Add(time.Hour + time.Second)
	if !b.Take(ctx, 1) {
		t.Error("account 1 still limited an hour later")
	}
	var none *Budget
	if !none.Take(ctx, 1) || none.Left(ctx, 1) < 1000 {
		t.Error("no budget limits")
	}
}
