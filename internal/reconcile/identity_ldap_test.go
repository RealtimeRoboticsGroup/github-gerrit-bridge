package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// ldapSearcher is a directory server's answer: its entries, or err.
type ldapSearcher struct {
	entries []ldap.Entry
	err     error
}

func (f *ldapSearcher) Search(context.Context, string, string, []string) ([]ldap.Entry, error) {
	return f.entries, f.err
}

func ldapEntry(uid, login, id string) ldap.Entry {
	return ldap.Entry{DN: "uid=" + uid + ",ou=People,dc=spacecookies,dc=dev",
		Attrs: map[string][]string{"uid": {uid}, "githublogin": {login}, "githubid": {id}}}
}

// ldapUsers is Users with a directory (read once) and Gerrit accounts
// austin, james, bob and alice.
func ldapUsers(t *testing.T, s *ldapSearcher) (*Users, *ldap.Directory) {
	t.Helper()
	d := &ldap.Directory{Searcher: s, BaseDN: "ou=People,dc=spacecookies,dc=dev", ObjectClass: "githubAccount", Refresh: time.Hour}
	d.Read(context.Background())
	accts := fakeAccounts{users: []string{"austin", "james", "bob", "alice", "1000000", "github-bridge"}, inactive: map[string]bool{}}
	// Overrides and username matching set too: with a directory, neither
	// is asked.
	u := &Users{Directory: d, Gerrit: accts, Active: accts, Overrides: map[string]string{"octocat": "alice"},
		GitHub: fakeIDs{"austinschuh": 294043, "james": 777, "jamesongh": 1234}}
	return u, d
}

func gerritUser(u *Users, login string, id int64) string {
	user, _ := u.GerritUserFor(context.Background(), github.User{Login: login, ID: id})
	return user
}

// Design §7: the directory maps by the numeric id GitHub says the actor
// with, whatever the login (renamed); the GitHub user who owns the login
// "james" is not our james; an actor said without an id is looked up
// never (nobody); and whoever the directory does not list is nobody, whatever an
// override or a matching username would say: everyone must be in it (the
// owner's call, 2026-11-21).
func TestDirectoryMapsByGitHubID(t *testing.T) {
	u, _ := ldapUsers(t, &ldapSearcher{entries: []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043"), ldapEntry("james", "jamesonGH", "1234")}})
	for _, tc := range []struct {
		login string
		id    int64
		want  string
	}{
		{"AustinSchuh", 294043, "austin"},
		{"AustinRenamed", 294043, "austin"}, // by id
		{"jamesonGH", 1234, "james"},
		{"james", 777, ""},       // GitHub's "james", not ours: username matching may not hand it over
		{"AustinSchuh", 555, ""}, // the login, now someone else's
		{"AustinSchuh", 0, ""},   // no id said: by login is not by account (fourteenth review)
		{"bob", 31, ""},          // not in the directory: a matching username is no fallback
		{"octocat", 32, ""},      // nor is an override
	} {
		if got := gerritUser(u, tc.login, tc.id); got != tc.want {
			t.Errorf("%s (%d) = %q, want %q", tc.login, tc.id, got, tc.want)
		}
	}
}

// Fail closed: a directory that cannot be read matches nobody (overrides
// included: it might say they are someone else's); entries in a clash
// match nobody; the others still do.
func TestDirectoryFailsClosed(t *testing.T) {
	s := &ldapSearcher{entries: []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043"),
		ldapEntry("james", "jamesonGH", "1234"), ldapEntry("bob", "bobGH", "1234"), // one id, two users
		ldapEntry("alice", "aliceGH", "x5")}} // not a number
	u, d := ldapUsers(t, s)
	u.Overrides = map[string]string{"carol-gh": "alice"}
	u.GitHub = fakeIDs{"carol-gh": 66}
	if got := gerritUser(u, "AustinSchuh", 294043); got != "austin" {
		t.Errorf("austin = %q", got)
	}
	for _, id := range []int64{1234, 5} {
		if got := gerritUser(u, "someone", id); got != "" {
			t.Errorf("id %d (a bad entry's) = %q", id, got)
		}
	}
	s.err = errors.New("ldap.spacecookies.dev: connection refused")
	d.Read(context.Background())
	for _, who := range []github.User{{Login: "AustinSchuh", ID: 294043}, {Login: "carol-gh", ID: 66}, {Login: "bob", ID: 31}} {
		if got, ok := u.GerritUserFor(context.Background(), who); ok {
			t.Errorf("directory down: %s = %q", who.Login, got)
		}
	}
}

// Removed from the directory, or given a new GitHub account: the next
// read stops the old one acting (cached answers included), and the pins
// follow the directory, which an administrator curates: it wins over a
// pin made earlier, and over another Gerrit user's pin to that account.
func TestDirectoryChangesAndPins(t *testing.T) {
	ctx := context.Background()
	s := &ldapSearcher{entries: []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043")}}
	u, d := ldapUsers(t, s)
	pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{
		"austin": {GitHubID: 555, GitHubLogin: "OldAccount"}, // pinned before, to another account
		"bob":    {GitHubID: 294043, GitHubLogin: "AustinSchuh"},
		"alice":  {GitHubID: 9, GitHubLogin: "alice"},
	}}}
	u.Pins = pins
	if got := gerritUser(u, "AustinSchuh", 294043); got != "austin" {
		t.Fatalf("austin = %q", got)
	}
	if p := pins.ids.ByUser["austin"]; p.GitHubID != 294043 {
		t.Errorf("austin pinned to %d, want the directory's 294043", p.GitHubID)
	}
	if _, ok := pins.ids.ByUser["bob"]; ok {
		t.Error("bob still pinned to the account the directory gives austin")
	}
	if p := pins.ids.ByUser["alice"]; p.GitHubID != 9 {
		t.Errorf("alice's pin changed: %+v", p)
	}
	// An administrator gives austin a new GitHub account.
	s.entries = []ldap.Entry{ldapEntry("austin", "AustinNew", "999")}
	d.Read(ctx)
	if got := gerritUser(u, "AustinSchuh", 294043); got != "" {
		t.Errorf("the old account still acts: %q", got)
	}
	if got := gerritUser(u, "AustinNew", 999); got != "austin" || pins.ids.ByUser["austin"].GitHubID != 999 {
		t.Errorf("the new account: %q, pinned %+v", got, pins.ids.ByUser["austin"])
	}
	// Removed.
	s.entries = nil
	d.Read(ctx)
	if got := gerritUser(u, "AustinNew", 999); got != "" {
		t.Errorf("removed, still acts: %q", got)
	}
	// A dry run writes no pins.
	s.entries = []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043")}
	d.Read(ctx)
	u.DryRun = true
	if got := gerritUser(u, "AustinSchuh", 294043); got != "austin" || pins.ids.ByUser["austin"].GitHubID != 999 {
		t.Errorf("dry run: %q, pinned %+v", got, pins.ids.ByUser["austin"])
	}
}

// What the directory names is never acted as if Gerrit would read it as
// another account, or if it is inactive or the bridge's own; never_groups
// do not apply (an entry names the account outright, as an override does).
func TestDirectoryNamesOnlyAccountsItMay(t *testing.T) {
	s := &ldapSearcher{entries: []ldap.Entry{
		ldapEntry("1000000", "numberGH", "10"),
		ldapEntry("james", "jamesonGH", "1234"),
		ldapEntry("github-bridge", "botGH", "20"),
		ldapEntry("alice", "aliceGH", "30"),
		ldapEntry("bob", "bobGH", "40"),
	}}
	u, _ := ldapUsers(t, s)
	accts := u.Active.(fakeAccounts)
	accts.inactive["james"] = true
	accts.groups = map[string][]string{"Administrators": {"alice"}}
	u.Gerrit, u.Active = accts, accts
	u.Never, u.NeverGroups = []string{"github-bridge"}, []string{"Administrators"}
	for id, want := range map[int64]string{10: "", 1234: "", 20: "", 30: "alice", 40: "bob"} {
		login := map[int64]string{10: "numberGH", 1234: "jamesonGH", 20: "botGH", 30: "aliceGH", 40: "bobGH"}[id]
		if got := gerritUser(u, login, id); got != want {
			t.Errorf("%s (%d) = %q, want %q", login, id, got, want)
		}
	}
	// The bridge's own GitHub login maps to nobody, whatever says.
	u.NeverLogins = []string{"aliceGH"}
	if got := gerritUser(u, "aliceGH", 30); got != "" {
		t.Errorf("the bridge's login = %q", got)
	}
}

// §7.2: a link is used only while the mapping still gives its GitHub
// account to its Gerrit account; with the directory, while it does.
func TestDirectoryKeepsLinksToItsMapping(t *testing.T) {
	s := &ldapSearcher{entries: []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043")}}
	u, d := ldapUsers(t, s)
	linked := func() bool { // as cmd/bridge's usertokens Check asks it
		user, ok := u.GerritUserFor(context.Background(), github.User{Login: "AustinSchuh", ID: 294043})
		return ok && user == "austin"
	}
	if !linked() {
		t.Fatal("the link is not used while the directory maps it")
	}
	s.entries = []ldap.Entry{ldapEntry("james", "AustinSchuh", "294043")}
	d.Read(context.Background())
	if linked() {
		t.Error("the link is still used after the directory gave the account to james")
	}
}

// Until the directory is read, or while a read fails, the mapping says it
// is not ready (fourteenth review: everyone looked unmapped then, and
// some of what the bridge did was for good); once read, it is.
func TestUsersReadyFollowsTheDirectory(t *testing.T) {
	s := &ldapSearcher{err: errors.New("connection refused")}
	d := &ldap.Directory{Searcher: s, BaseDN: "ou=People", ObjectClass: "githubAccount", Refresh: time.Hour}
	u := &Users{Directory: d}
	if u.Ready() == nil {
		t.Error("ready before any read")
	}
	d.Read(context.Background())
	if u.Ready() == nil {
		t.Error("ready after a failed read")
	}
	s.err = nil
	d.Read(context.Background())
	if err := u.Ready(); err != nil {
		t.Errorf("not ready after a read: %v", err)
	}
	if err := (&Users{}).Ready(); err != nil {
		t.Errorf("no directory: %v", err)
	}
}

// The dispatcher runs nothing while the mapping is not ready: each key
// waits, to be tried again.
func TestDispatcherWaitsForTheDirectory(t *testing.T) {
	ready := errors.New("the directory has not been read")
	d := &Dispatcher{Ready: func() error { return ready }} // no Code: running anything would panic
	err := d.Reconcile(context.Background(), Key{Branch: "main"})
	var later *errLater
	if !errors.As(err, &later) || later.after != identityRetry {
		t.Errorf("err %v", err)
	}
}

// What the directory names is checked every time, not reused: an account
// deactivated just now is not acted as (fourteenth review: it was, for
// two minutes); and its name is Gerrit's, whatever the directory's case.
func TestDirectoryChecksEveryTime(t *testing.T) {
	u, _ := ldapUsers(t, &ldapSearcher{entries: []ldap.Entry{ldapEntry("Austin", "AustinSchuh", "294043")}})
	if got := gerritUser(u, "AustinSchuh", 294043); got != "austin" {
		t.Fatalf("austin = %q (Gerrit's name, any case)", got)
	}
	u.Active.(fakeAccounts).inactive["austin"] = true
	if got := gerritUser(u, "AustinSchuh", 294043); got != "" {
		t.Errorf("deactivated, still acted as: %q", got)
	}
}
