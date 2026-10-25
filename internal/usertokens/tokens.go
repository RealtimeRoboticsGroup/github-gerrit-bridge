package usertokens

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Tokens hands out linked users' tokens, renewing them as they expire.
type Tokens struct {
	Store *Store
	App   *github.UserApp
	// Check reports whether GitHub account gh maps to Gerrit user now, by
	// the identity mapping (pins, exclusions) as it is: a record is used
	// only while it does, so an account excluded or re-pinned since its
	// link stops posting as its GitHub account.
	Check  func(ctx context.Context, gh github.User, gerritUser string) bool
	Logger *slog.Logger

	mu  sync.Mutex // one refresh at a time: each voids the last token
	now func() time.Time
}

// renewBefore is how long before it expires a token is renewed.
const renewBefore = 5 * time.Minute

func (t *Tokens) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *Tokens) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// For returns the token to post Gerrit account's content on GitHub with,
// and the GitHub account it acts as. ok is false if the account has no
// usable link (none, its mapping changed, or its refresh token is no
// longer good, which drops the link); err is a failure to renew the
// token that may pass.
func (t *Tokens) For(ctx context.Context, account int) (token string, gh github.User, ok bool, err error) {
	if t == nil || t.Store == nil || account == 0 {
		return "", github.User{}, false, nil
	}
	rec, found := t.byAccount(account)
	if !found {
		return "", github.User{}, false, nil
	}
	if t.Check != nil && !t.Check(ctx, rec.GitHub, rec.GerritUser) {
		t.logger().Info("linked GitHub account no longer maps to its Gerrit account; posting as the bridge",
			"github_login", rec.GitHub.Login, "github_id", rec.GitHub.ID, "gerrit_user", rec.GerritUser)
		return "", github.User{}, false, nil
	}
	if rec.Token.AccessExpires.IsZero() || t.clock().Add(renewBefore).Before(rec.Token.AccessExpires) {
		return rec.Token.Access, rec.GitHub, true, nil
	}
	return t.renew(ctx, rec.GitHub.ID)
}

func (t *Tokens) byAccount(account int) (Record, bool) {
	for _, r := range t.Store.All() {
		if r.GerritAccount == account {
			return r, true
		}
	}
	return Record{}, false
}

func (t *Tokens) renew(ctx context.Context, id int64) (string, github.User, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, found := t.Store.Get(id)
	if !found {
		return "", github.User{}, false, nil
	}
	now := t.clock()
	if now.Add(renewBefore).Before(rec.Token.AccessExpires) {
		return rec.Token.Access, rec.GitHub, true, nil // renewed meanwhile
	}
	drop := func(why string) (string, github.User, bool, error) {
		t.logger().Warn("dropping a linked GitHub account: "+why, "github_login", rec.GitHub.Login, "github_id", rec.GitHub.ID)
		if err := t.Store.Delete(id); err != nil {
			t.logger().Error("saving user tokens", "err", err)
		}
		return "", github.User{}, false, nil
	}
	if rec.Token.Refresh == "" || (!rec.Token.RefreshExpires.IsZero() && !now.Before(rec.Token.RefreshExpires)) {
		return drop("its token expired; they can link again")
	}
	tok, err := t.App.Refresh(ctx, rec.Token.Refresh)
	if errors.Is(err, github.ErrBadRefresh) {
		return drop("GitHub no longer takes its refresh token (the App was revoked?); they can link again")
	}
	if err != nil {
		return "", github.User{}, false, err
	}
	// The old refresh token is void now: keep the new one first.
	rec.Token = tok
	if err := t.Store.Put(rec); err != nil {
		t.logger().Error("saving a renewed user token; it lasts until the bridge restarts", "github_login", rec.GitHub.Login, "err", err)
	}
	u, err := t.App.TokenUser(ctx, tok.Access)
	if err != nil {
		return "", github.User{}, false, err
	}
	if u.ID != rec.GitHub.ID {
		return drop("its token acts as another account")
	}
	if u.Login != rec.GitHub.Login {
		rec.GitHub.Login = u.Login // renamed on GitHub
		if err := t.Store.Put(rec); err != nil {
			t.logger().Error("saving user tokens", "err", err)
		}
		if t.Check != nil && !t.Check(ctx, rec.GitHub, rec.GerritUser) {
			return "", github.User{}, false, nil
		}
	}
	return tok.Access, rec.GitHub, true, nil
}

// Revoke drops GitHub account id's link if its token is still access,
// which GitHub refused (401: the user revoked the App, say).
func (t *Tokens) Revoke(id int64, access string) {
	if t == nil || t.Store == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, found := t.Store.Get(id)
	if !found || rec.Token.Access != access {
		return
	}
	t.logger().Warn("GitHub refused a linked user's token; dropping the link", "github_login", rec.GitHub.Login, "github_id", id)
	if err := t.Store.Delete(id); err != nil {
		t.logger().Error("saving user tokens", "err", err)
	}
}
