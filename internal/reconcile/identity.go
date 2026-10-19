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
	// service accounts and administrators). Gerrit must implement
	// GroupMembers; any lookup error matches nobody, including a 404,
	// which is also what Gerrit says for a group the bridge cannot see:
	// CheckGroups at startup makes sure it can see them all.
	NeverGroups []string
	// NeverLogins are GitHub logins never matched (the bridge's own: its
	// edits and pushes are its own, not those of whoever owns its token).
	NeverLogins []string
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
	UserID(ctx context.Context, login string) (int64, time.Time, error)
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

// CheckGroups makes sure the bridge can see every group in NeverGroups:
// Gerrit answers 404 both for a group that does not exist and for one the
// caller may not see (Administrators, by default), so an exclusion that
// cannot be checked must be caught before anyone is matched.
func (u *Users) CheckGroups(ctx context.Context) error {
	if u.Gerrit == nil || len(u.NeverGroups) == 0 {
		return nil
	}
	gl, ok := u.Gerrit.(groupLister)
	if !ok {
		return errors.New("cannot list Gerrit groups")
	}
	var errs []error
	for _, g := range u.NeverGroups {
		if _, err := gl.GroupMembers(ctx, g); err != nil {
			var ge *gerrit.Error
			if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
				err = errors.New("not found: it does not exist, or the bridge may not see it (make it visible to all registered users, or remove it from identity.never_groups)")
			}
			errs = append(errs, fmt.Errorf("identity.never_groups %q: %w", g, err))
		}
	}
	return errors.Join(errs...)
}

// GerritUser implements Identity.
func (u *Users) GerritUser(ctx context.Context, login string) (string, bool) {
	if login == "" {
		return "", false
	}
	for _, n := range u.NeverLogins {
		if strings.EqualFold(n, login) {
			return "", false
		}
	}
	key := strings.ToLower(login)
	u.mu.Lock()
	if c, ok := u.cache[key]; ok && time.Since(c.at) < userCacheTTL {
		u.mu.Unlock()
		return c.user, c.ok
	}
	u.mu.Unlock()

	user, account, ok, err := u.lookup(ctx, login)
	if err == nil && ok && u.Pins != nil {
		ok, err = u.pinned(ctx, login, user, account)
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

// lookup finds login's Gerrit account by override or username. account is
// the matched account's id, or 0 for an override.
func (u *Users) lookup(ctx context.Context, login string) (user string, account int, ok bool, err error) {
	for gh, user := range u.Overrides {
		if strings.EqualFold(gh, login) {
			if gerrit.AmbiguousUsername(user) {
				u.warn("override names a Gerrit username Gerrit would read as another account; not acting as it", login, user)
				return "", 0, false, nil
			}
			return user, 0, true, nil
		}
	}
	if u.Gerrit == nil {
		return "", 0, false, nil
	}
	accts, err := u.Gerrit.QueryAccounts(ctx, "username:"+gerrit.QueryString(login)+" is:active")
	if err != nil {
		return "", 0, false, err
	}
	var matches []gerrit.AccountInfo
	for _, a := range accts {
		if strings.EqualFold(a.Username, login) {
			matches = append(matches, a)
		}
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			u.warn("GitHub login matches several Gerrit accounts (by case); not acting as any", login, matches[0].Username)
		}
		return "", 0, false, nil
	}
	user, account = matches[0].Username, matches[0].AccountID
	// Gerrit reads "1000000" as account 1000000 and "me" as the caller,
	// wherever it takes an account: whoever registers that login on GitHub
	// would act as someone else.
	if gerrit.AmbiguousUsername(user) {
		u.warn("Gerrit would read this username as another account; not acting as it", login, user)
		return "", 0, false, nil
	}
	if u.excluded(ctx, user) {
		u.warn("GitHub login matches an excluded Gerrit account; not acting as it", login, user)
		return "", 0, false, nil
	}
	return user, account, true, nil
}

func (u *Users) warn(msg, login, user string) {
	if u.Logger != nil {
		u.Logger.Warn(msg, "github_login", login, "gerrit_user", user)
	}
}

// registeredLooker says when a Gerrit account was registered.
type registeredLooker interface {
	Registered(ctx context.Context, account int) (time.Time, error)
}

// pinned checks that login is still the GitHub account the Gerrit account
// user was first seen as (by numeric id), pinning it on first sight.
// Logins are freed by renames, and whoever takes one must not inherit the
// Gerrit account. Before pinning a username match (account != 0), the
// GitHub account must be older than the Gerrit account: one created after
// it cannot be the account it was registered with, so it is someone who
// took the login of an account the bridge had not yet seen.
func (u *Users) pinned(ctx context.Context, login, user string, account int) (bool, error) {
	id, created, err := u.GitHub.UserID(ctx, login)
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
		if account != 0 {
			rl, ok := u.Gerrit.(registeredLooker)
			if !ok {
				return false, errors.New("cannot tell when Gerrit accounts were registered")
			}
			registered, err := rl.Registered(ctx, account)
			if err != nil {
				return false, err
			}
			if created.IsZero() || created.After(registered) {
				if u.Logger != nil {
					u.Logger.Warn("GitHub account is newer than the Gerrit account with its login; not acting as it (add an override if it is theirs)",
						"github_login", login, "github_id", id, "github_created", created, "gerrit_user", user, "gerrit_registered", registered)
				}
				return false, nil
			}
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
