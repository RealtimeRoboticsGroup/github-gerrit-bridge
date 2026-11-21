package ldap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Searcher runs a search (Server, or a test's).
type Searcher interface {
	Search(ctx context.Context, base, class string, attrs []string) ([]Entry, error)
}

// Account is an entry's statement that a GitHub account (by numeric id)
// is a Gerrit user's.
type Account struct {
	GerritUser  string
	GitHubLogin string
	GitHubID    int64
	DN          string
}

// Directory is the GitHub accounts an LDAP directory lists (design §7.1,
// the identity.ldap source): each entry of ObjectClass under BaseDN gives
// its uid (the Gerrit username) a GitHub account, githubLogin and
// githubId. Only an administrator writes them, so an entry is as trusted
// as an identity.github_to_gerrit override.
//
// It is read whole every Refresh interval (Run), not per lookup; a
// lookup's answer is that read's. Anything the read finds wrong is left
// out, entry by entry (an invalid login or id, a value given twice), and
// every entry in a clash (one GitHub id, login or Gerrit user in several
// entries) is left out with all the others in it, whatever the server's
// unique overlay says: nobody is matched by a guess. A read that fails,
// or none for two intervals, is no answer: Lookup then says so, and the
// caller matches nobody.
type Directory struct {
	Searcher    Searcher
	BaseDN      string
	ObjectClass string
	Refresh     time.Duration
	Logger      *slog.Logger

	mu   sync.Mutex
	snap *snapshot
	err  error // the last read's failure
	now  func() time.Time
}

type snapshot struct {
	byID   map[int64]Account
	byUser map[string]Account // lower-cased uid
	at     time.Time
	gen    int
}

// Attribute names read (and written by the directory's admins).
const (
	attrUID   = "uid"
	attrLogin = "githublogin"
	attrID    = "githubid"
)

// ErrNoDirectory is a lookup with no read of the directory to answer it.
var ErrNoDirectory = errors.New("ldap: the directory has not been read, or not lately")

// gitHubLogin is GitHub's rule for logins: letters, digits and single
// hyphens between them, at most 39 characters (slapd does not check it).
var gitHubLogin = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// gitHubID is a positive decimal integer, as GitHub's ids are.
var gitHubID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

func (d *Directory) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// Read reads the directory now. A failure throws the last read away: who
// was removed must stop being matched, and an old read cannot say they
// were not.
func (d *Directory) Read(ctx context.Context) error {
	entries, err := d.Searcher.Search(ctx, d.BaseDN, d.ObjectClass, []string{"uid", "githubLogin", "githubId"})
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.snap, d.err = nil, err
		d.log().Error("reading GitHub accounts from LDAP; matching nobody by it until it can be read", "base_dn", d.BaseDN, "err", err)
		return err
	}
	snap := build(entries, d.log())
	snap.at = d.clock()
	if d.snap != nil {
		snap.gen = d.snap.gen + 1
		d.logChanges(d.snap, snap)
	}
	d.snap, d.err = snap, nil
	return nil
}

func (d *Directory) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// logChanges says who gained, lost or changed GitHub accounts between two
// reads.
func (d *Directory) logChanges(old, cur *snapshot) {
	for user, a := range cur.byUser {
		if was, ok := old.byUser[user]; !ok {
			d.log().Info("LDAP gives a Gerrit user a GitHub account", "gerrit_user", a.GerritUser, "github_id", a.GitHubID, "github_login", a.GitHubLogin)
		} else if was.GitHubID != a.GitHubID {
			d.log().Warn("LDAP gives a Gerrit user another GitHub account", "gerrit_user", a.GerritUser,
				"github_id", a.GitHubID, "github_login", a.GitHubLogin, "was_id", was.GitHubID, "was_login", was.GitHubLogin)
		}
	}
	for user, a := range old.byUser {
		if _, ok := cur.byUser[user]; !ok {
			d.log().Info("LDAP no longer gives a Gerrit user a GitHub account", "gerrit_user", a.GerritUser, "was_id", a.GitHubID)
		}
	}
}

// build checks entries and indexes the good ones.
func build(entries []Entry, log *slog.Logger) *snapshot {
	var good []Account
	for _, e := range entries {
		a, err := account(e)
		if err != nil {
			log.Error("LDAP entry left out; matching nobody by it", "dn", e.DN, "err", err)
			continue
		}
		good = append(good, a)
	}
	// Clashes: every entry sharing an id, a login or a user is left out.
	ids, logins, users := map[int64]int{}, map[string]int{}, map[string]int{}
	for _, a := range good {
		ids[a.GitHubID]++
		logins[strings.ToLower(a.GitHubLogin)]++
		users[strings.ToLower(a.GerritUser)]++
	}
	s := &snapshot{byID: map[int64]Account{}, byUser: map[string]Account{}}
	for _, a := range good {
		var clash string
		switch {
		case ids[a.GitHubID] > 1:
			clash = "its githubId is another entry's too"
		case logins[strings.ToLower(a.GitHubLogin)] > 1:
			clash = "its githubLogin is another entry's too"
		case users[strings.ToLower(a.GerritUser)] > 1:
			clash = "its uid is another entry's too"
		}
		if clash != "" {
			log.Error("LDAP entry left out, with every entry it clashes with; matching nobody by them", "dn", a.DN, "err", clash,
				"github_id", a.GitHubID, "github_login", a.GitHubLogin, "gerrit_user", a.GerritUser)
			continue
		}
		s.byID[a.GitHubID] = a
		s.byUser[strings.ToLower(a.GerritUser)] = a
	}
	return s
}

// account checks one entry.
func account(e Entry) (Account, error) {
	one := func(attr string) (string, error) {
		v := e.Attrs[attr]
		if len(v) != 1 {
			return "", fmt.Errorf("%s has %d values, not one", attr, len(v))
		}
		return v[0], nil
	}
	uid, err := one(attrUID)
	if err != nil {
		return Account{}, err
	}
	login, err := one(attrLogin)
	if err != nil {
		return Account{}, err
	}
	id, err := one(attrID)
	if err != nil {
		return Account{}, err
	}
	if uid == "" || strings.TrimSpace(uid) != uid {
		return Account{}, fmt.Errorf("uid %q is not a username", uid)
	}
	if len(login) > 39 || !gitHubLogin.MatchString(login) {
		return Account{}, fmt.Errorf("githubLogin %q is not a GitHub login", login)
	}
	if !gitHubID.MatchString(id) {
		return Account{}, fmt.Errorf("githubId %q is not a GitHub id", id)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Account{}, fmt.Errorf("githubId %q: %w", id, err)
	}
	return Account{GerritUser: uid, GitHubLogin: login, GitHubID: n, DN: e.DN}, nil
}

// current is the last read, if it is fresh enough to answer with.
func (d *Directory) current() (*snapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snap == nil {
		if d.err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoDirectory, d.err)
		}
		return nil, ErrNoDirectory
	}
	if d.clock().Sub(d.snap.at) > 2*d.Refresh {
		return nil, fmt.Errorf("%w: last read at %s", ErrNoDirectory, d.snap.at.Format(time.RFC3339))
	}
	return d.snap, nil
}

// ByGitHubID is the account the directory gives GitHub id id, if any;
// gen says which read answered (it changes with every read).
func (d *Directory) ByGitHubID(id int64) (a Account, found bool, gen int, err error) {
	s, err := d.current()
	if err != nil {
		return Account{}, false, 0, err
	}
	a, found = s.byID[id]
	return a, found, s.gen, nil
}

// ByGerritUser is the account the directory gives Gerrit user user, if
// any.
func (d *Directory) ByGerritUser(user string) (a Account, found bool, err error) {
	s, err := d.current()
	if err != nil {
		return Account{}, false, err
	}
	a, found = s.byUser[strings.ToLower(user)]
	return a, found, nil
}

// Run reads the directory every Refresh until ctx ends, and once at the
// start; after a failed read it tries again within a minute.
func (d *Directory) Run(ctx context.Context) {
	for {
		wait := d.Refresh
		if err := d.Read(ctx); err != nil && wait > time.Minute {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
