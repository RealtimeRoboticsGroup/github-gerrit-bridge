package reconcile

import (
	"context"
	"sync"
	"time"
)

// Budget limits how much one Gerrit account can make the bridge write to
// GitHub: anyone can register on Gerrit and upload hundreds of changes or
// post hundreds of comments, each of which the bridge's token would write
// to GitHub, until GitHub's limits stall the bridge for everyone (third
// audit). Members of the trusted group are not limited.
type Budget struct {
	// N writes per Per, for each account.
	N   int
	Per time.Duration
	// Trusted, if set, exempts accounts in CI's trusted group.
	Trusted func(ctx context.Context, account int) (trusted, known bool)

	mu    sync.Mutex
	spent map[int][]time.Time // account -> writes within Per, oldest first
	now   func() time.Time
}

func (b *Budget) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// exempt reports whether account is not limited.
func (b *Budget) exempt(ctx context.Context, account int) bool {
	if b == nil || b.N <= 0 {
		return true
	}
	if b.Trusted != nil {
		if t, known := b.Trusted(ctx, account); t && known {
			return true
		}
	}
	return false
}

// Left is how many more writes account may cause now.
func (b *Budget) Left(ctx context.Context, account int) int {
	if b.exempt(ctx, account) {
		return 1 << 30
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.N - len(b.recent(account))
}

// Take spends one of account's writes, reporting false (and spending
// nothing) if none are left.
func (b *Budget) Take(ctx context.Context, account int) bool {
	if b.exempt(ctx, account) {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.recent(account)
	if len(r) >= b.N {
		return false
	}
	b.spent[account] = append(r, b.clock())
	return true
}

// recent is account's writes within Per. b.mu is held.
func (b *Budget) recent(account int) []time.Time {
	if b.spent == nil {
		b.spent = map[int][]time.Time{}
	}
	r := b.spent[account]
	cut := b.clock().Add(-b.Per)
	for len(r) > 0 && !r[0].After(cut) {
		r = r[1:]
	}
	if len(r) == 0 {
		delete(b.spent, account)
		return nil
	}
	b.spent[account] = r
	return r
}
