package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
)

// Identity maps GitHub logins to Gerrit usernames (design §7).
type Identity interface {
	GerritUser(ctx context.Context, githubLogin string) (string, bool)
}

// UserQuerier finds Gerrit accounts.
type UserQuerier interface {
	QueryAccounts(ctx context.Context, query string) ([]gerrit.AccountInfo, error)
}

// Users resolves GitHub logins to Gerrit accounts: an explicit override
// wins; otherwise, with Gerrit set, the active account whose username is
// the GitHub login. That is how Gerrit's github plugin names the accounts
// it creates at GitHub login (as on AOS), so nobody has to maintain a map.
type Users struct {
	Overrides map[string]string
	Gerrit    UserQuerier // nil: overrides only
	// Never are Gerrit usernames no GitHub login is matched to (the
	// bridge's own account, say): a GitHub user who registers that login
	// must not act as it.
	Never []string
	// NeverGroups are Gerrit groups whose members are never matched (the
	// service accounts: "Service Users", "Non-Interactive Users"). Gerrit
	// must implement GroupMembers; a lookup error matches nobody.
	NeverGroups []string
	Logger      *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedUser
}

// groupLister lists a Gerrit group's members.
type groupLister interface {
	GroupMembers(ctx context.Context, group string) ([]gerrit.AccountInfo, error)
}

// excluded reports whether username may not be matched to a GitHub login.
func (u *Users) excluded(ctx context.Context, username string) bool {
	for _, n := range u.Never {
		if strings.EqualFold(n, username) {
			return true
		}
	}
	if len(u.NeverGroups) == 0 {
		return false
	}
	gl, ok := u.Gerrit.(groupLister)
	if !ok {
		return true
	}
	for _, g := range u.NeverGroups {
		members, err := gl.GroupMembers(ctx, g)
		if err != nil {
			var ge *gerrit.Error
			if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
				continue // no such group on this site
			}
			if u.Logger != nil {
				u.Logger.Warn("listing excluded group", "group", g, "err", err)
			}
			return true
		}
		for _, m := range members {
			if strings.EqualFold(m.Username, username) {
				return true
			}
		}
	}
	return false
}

type cachedUser struct {
	user string
	ok   bool
	at   time.Time
}

// userCacheTTL bounds how long a lookup (including "no account") is reused:
// a new GitHub login to Gerrit is picked up within it.
const userCacheTTL = 10 * time.Minute

// GerritUser implements Identity.
func (u *Users) GerritUser(ctx context.Context, login string) (string, bool) {
	if login == "" {
		return "", false
	}
	for gh, user := range u.Overrides {
		if strings.EqualFold(gh, login) {
			return user, true
		}
	}
	if u.Gerrit == nil {
		return "", false
	}
	key := strings.ToLower(login)
	u.mu.Lock()
	if c, ok := u.cache[key]; ok && time.Since(c.at) < userCacheTTL {
		u.mu.Unlock()
		return c.user, c.ok
	}
	u.mu.Unlock()

	accts, err := u.Gerrit.QueryAccounts(ctx, "username:"+gerrit.QueryString(login)+" is:active")
	if err != nil {
		// Don't cache errors; fall back to the bridge this time.
		if u.Logger != nil {
			u.Logger.Warn("looking up Gerrit account", "github_login", login, "err", err)
		}
		return "", false
	}
	c := cachedUser{at: time.Now()}
	for _, a := range accts {
		if strings.EqualFold(a.Username, login) {
			c.user, c.ok = a.Username, true
		}
	}
	if c.ok && u.excluded(ctx, c.user) {
		if u.Logger != nil {
			u.Logger.Warn("GitHub login matches an excluded Gerrit account; not acting as it", "github_login", login, "gerrit_user", c.user)
		}
		c.user, c.ok = "", false
	}
	u.mu.Lock()
	if u.cache == nil {
		u.cache = map[string]cachedUser{}
	}
	u.cache[key] = c
	u.mu.Unlock()
	return c.user, c.ok
}
