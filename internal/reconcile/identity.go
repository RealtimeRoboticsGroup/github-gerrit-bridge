package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
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
	// Pins, with GitHub, pin each Gerrit account to the GitHub account
	// (numeric id) it was first matched to (design §7.1). Nil: no pinning.
	Pins   Pins
	GitHub GitHubUsers
	DryRun bool // check pins, but write none
	Logger *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedUser
	pinMu sync.Mutex
}

// Pins stores identity pins (state.Store).
type Pins interface {
	LoadIdentities(ctx context.Context) (*state.Identities, error)
	SaveIdentities(ctx context.Context, ids *state.Identities, msg string) error
}

// GitHubUsers resolves logins to GitHub's numeric account ids.
type GitHubUsers interface {
	UserID(ctx context.Context, login string) (int64, error)
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
// a new GitHub login to Gerrit is picked up within it, and a login that
// changes hands stops acting within it.
const userCacheTTL = 2 * time.Minute

// GerritUser implements Identity.
func (u *Users) GerritUser(ctx context.Context, login string) (string, bool) {
	if login == "" {
		return "", false
	}
	key := strings.ToLower(login)
	u.mu.Lock()
	if c, ok := u.cache[key]; ok && time.Since(c.at) < userCacheTTL {
		u.mu.Unlock()
		return c.user, c.ok
	}
	u.mu.Unlock()

	user, ok, err := u.lookup(ctx, login)
	if err == nil && ok && u.Pins != nil {
		ok, err = u.pinned(ctx, login, user)
	}
	if err != nil {
		// Don't cache errors; fall back to the bridge this time.
		if u.Logger != nil {
			u.Logger.Warn("looking up Gerrit account", "github_login", login, "err", err)
		}
		return "", false
	}
	if !ok {
		user = ""
	}
	u.mu.Lock()
	if u.cache == nil {
		u.cache = map[string]cachedUser{}
	}
	u.cache[key] = cachedUser{user: user, ok: ok, at: time.Now()}
	u.mu.Unlock()
	return user, ok
}

// lookup finds login's Gerrit account by override or username.
func (u *Users) lookup(ctx context.Context, login string) (string, bool, error) {
	for gh, user := range u.Overrides {
		if strings.EqualFold(gh, login) {
			return user, true, nil
		}
	}
	if u.Gerrit == nil {
		return "", false, nil
	}
	accts, err := u.Gerrit.QueryAccounts(ctx, "username:"+gerrit.QueryString(login)+" is:active")
	if err != nil {
		return "", false, err
	}
	user := ""
	for _, a := range accts {
		if strings.EqualFold(a.Username, login) {
			user = a.Username
		}
	}
	if user == "" {
		return "", false, nil
	}
	if u.excluded(ctx, user) {
		if u.Logger != nil {
			u.Logger.Warn("GitHub login matches an excluded Gerrit account; not acting as it", "github_login", login, "gerrit_user", user)
		}
		return "", false, nil
	}
	return user, true, nil
}

// pinned checks that login is still the GitHub account the Gerrit account
// user was first seen as (by numeric id), pinning it on first sight.
// Logins are freed by renames, and whoever takes one must not inherit the
// Gerrit account.
func (u *Users) pinned(ctx context.Context, login, user string) (bool, error) {
	id, err := u.GitHub.UserID(ctx, login)
	if err != nil {
		return false, err
	}
	u.pinMu.Lock()
	defer u.pinMu.Unlock()
	for range 3 {
		ids, err := u.Pins.LoadIdentities(ctx)
		if err != nil {
			return false, err
		}
		key := strings.ToLower(user)
		if p, ok := ids.ByUser[key]; ok {
			if p.GitHubID == id {
				return true, nil
			}
			if u.Logger != nil {
				u.Logger.Warn("GitHub login now belongs to a different GitHub account than the one pinned to its Gerrit account; not acting as it",
					"github_login", login, "github_id", id, "gerrit_user", user, "pinned_id", p.GitHubID, "pinned_login", p.GitHubLogin)
			}
			return false, nil
		}
		if u.DryRun {
			return true, nil
		}
		ids.ByUser[key] = state.Identity{GitHubID: id, GitHubLogin: login, Pinned: time.Now().UTC()}
		err = u.Pins.SaveIdentities(ctx, ids, fmt.Sprintf("Pin %s to GitHub @%s (id %d)", user, login, id))
		if errors.Is(err, state.ErrConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		if u.Logger != nil {
			u.Logger.Info("pinned Gerrit account to GitHub id", "gerrit_user", user, "github_login", login, "github_id", id)
		}
		return true, nil
	}
	return false, fmt.Errorf("pinning %s: too many conflicts", user)
}
