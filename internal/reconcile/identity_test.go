package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

type fakeAccounts struct {
	users  []string
	groups map[string][]string
	// includes lists each group's included groups, by id.
	includes map[string][]string
	// inactive accounts, by username.
	inactive map[string]bool
	// registered is when each account (by index+1 in users) was
	// registered; unset means 2020.
	registered map[int]time.Time
}

func (f fakeAccounts) QueryAccounts(_ context.Context, q string) ([]gerrit.AccountInfo, error) {
	var out []gerrit.AccountInfo
	for i, u := range f.users {
		if f.inactive[u] && strings.Contains(q, "is:active") {
			continue
		}
		if strings.Contains(strings.ToLower(q), `"`+strings.ToLower(u)+`"`) || strings.Contains(strings.ToLower(q), ":"+strings.ToLower(u)+" ") {
			out = append(out, gerrit.AccountInfo{AccountID: i + 1, Username: u})
		}
	}
	return out, nil
}

func (f fakeAccounts) Registered(_ context.Context, account int) (time.Time, error) {
	if t, ok := f.registered[account]; ok {
		return t, nil
	}
	return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), nil
}

func (f fakeAccounts) IncludedGroups(_ context.Context, group string) ([]gerrit.GroupInfo, error) {
	var out []gerrit.GroupInfo
	for _, g := range f.includes[group] {
		out = append(out, gerrit.GroupInfo{ID: g, Name: g})
	}
	return out, nil
}

func (f fakeAccounts) GroupMembers(_ context.Context, group string) ([]gerrit.AccountInfo, error) {
	m, ok := f.groups[group]
	if !ok {
		return nil, &gerrit.Error{Status: 404}
	}
	var out []gerrit.AccountInfo
	for _, u := range m {
		out = append(out, gerrit.AccountInfo{Username: u})
	}
	return out, nil
}

// Gerrit may read an inactive account's name as someone else's: an
// override naming one is not acted on.
func TestOverrideNeedsAnActiveAccount(t *testing.T) {
	u := &Users{Overrides: map[string]string{"carol-gh": "carol", "alice-gh": "alice"},
		Active: fakeAccounts{users: []string{"alice", "carol"}, inactive: map[string]bool{"carol": true}}}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "carol-gh"}); ok {
		t.Errorf("acts as inactive %q", user)
	}
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice-gh"}); !ok || user != "alice" {
		t.Errorf("alice-gh = %q %v", user, ok)
	}
}

func TestUsersNeverMatchesServiceAccounts(t *testing.T) {
	u := &Users{
		Gerrit:      fakeAccounts{users: []string{"alice", "github-bridge", "buildkite"}, groups: map[string][]string{"Service Users": {"buildkite"}}},
		Never:       []string{"github-bridge"},
		NeverGroups: []string{"Service Users"},
	}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "Alice"}); !ok || user != "alice" {
		t.Errorf("alice = %q %v", user, ok)
	}
	// Someone who registers the bridge's or CI's name on GitHub gets nothing.
	for _, login := range []string{"github-bridge", "BuildKite"} {
		if user, ok := u.GerritUserFor(ctx, github.User{Login: login}); ok {
			t.Errorf("%s matched %q", login, user)
		}
	}
}

type fakeIDs map[string]int64

// githubCreated is when every fakeIDs account was created.
var githubCreated = time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)

func (f fakeIDs) UserID(_ context.Context, login string) (int64, time.Time, error) {
	if id, ok := f[strings.ToLower(login)]; ok {
		return id, githubCreated, nil
	}
	return 0, time.Time{}, errors.New("404 Not Found")
}

type memPins struct{ ids *state.Identities }

func (m *memPins) LoadIdentities(context.Context) (*state.Identities, error) {
	cp := &state.Identities{Version: m.ids.Version, ByUser: map[string]state.Identity{}}
	for k, v := range m.ids.ByUser {
		cp.ByUser[k] = v
	}
	return cp, nil
}

func (m *memPins) SaveIdentities(_ context.Context, ids *state.Identities, _ string) error {
	m.ids = ids
	return nil
}

func TestUsersPinGitHubIDs(t *testing.T) {
	ids := fakeIDs{"alice": 1, "alice-renamed": 1}
	pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
	u := &Users{Gerrit: fakeAccounts{users: []string{"alice"}}, Overrides: map[string]string{"alice-renamed": "alice"},
		Pins: pins, GitHub: ids}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); !ok || user != "alice" {
		t.Fatalf("first sight = %q %v", user, ok)
	}
	if pins.ids.ByUser["alice"].GitHubID != 1 {
		t.Fatalf("not pinned: %+v", pins.ids.ByUser)
	}
	// Alice renames her GitHub account (an override maps the new login):
	// same id, still her.
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice-renamed"}); !ok || user != "alice" {
		t.Errorf("renamed = %q %v", user, ok)
	}
	// Someone else registers the freed login "alice".
	ids["alice"] = 2
	u.cache = nil
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); ok {
		t.Errorf("new owner of the login acts as %q", user)
	}
	// Unknown on GitHub (deleted): nobody.
	delete(ids, "alice")
	u.cache = nil
	if _, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); ok {
		t.Error("deleted GitHub account still maps")
	}
}

// A lookup by login is reused for a while, but GitHub says by numeric id
// who did something: whoever took a renamed user's login just now is not
// them, cached or not (sixth audit). Where the id is not known, the
// lookup is fresh (seventh audit: pushes, edits and resolutions were
// matched by login alone, from the cache).
func TestUsersCheckTheGitHubIDTheyAreGiven(t *testing.T) {
	ids := fakeIDs{"alice": 1}
	pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
	u := &Users{Gerrit: fakeAccounts{users: []string{"alice"}}, Pins: pins, GitHub: ids}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice", ID: 1}); !ok || user != "alice" {
		t.Fatalf("alice = %q %v", user, ok)
	}
	ids["alice"] = 2 // alice renamed; someone else took the login
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); ok {
		t.Errorf("by login alone, from the cache: the login's new holder acts as %q", user)
	}
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice", ID: 2}); ok {
		t.Errorf("the login's new holder acts as %q", user)
	}
	// Nor while GitHub's answer for the login is still the old holder's.
	ids["alice"] = 1
	u.cache = nil
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice", ID: 2}); ok {
		t.Errorf("an account GitHub's lookup does not name acts as %q", user)
	}
}

// An override maps login alice-gh to Gerrit's alice: whoever registers
// "alice" on GitHub (with an account old enough to pass the age rule)
// must not be matched to alice by username first, and so lock out the
// override's own account (fifth audit). The override's account renamed
// back to "alice" still is.
func TestUsernameMatchDoesNotTakeAnOverridesAccount(t *testing.T) {
	ctx := context.Background()
	registered := githubCreated.Add(24 * time.Hour) // the attacker's GitHub account is older
	for _, tc := range []struct {
		name     string
		aliceID  int64 // whoever has the login "alice" on GitHub
		matches  bool
		withPins bool
	}{{"someone else", 2, false, true}, {"the override's account, renamed back", 1, true, true}, {"no pins: nobody", 1, false, false}} {
		pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
		u := &Users{Gerrit: fakeAccounts{users: []string{"alice"}, registered: map[int]time.Time{1: registered}},
			Overrides: map[string]string{"alice-gh": "alice"}, GitHub: fakeIDs{"alice": tc.aliceID, "alice-gh": 1}}
		if tc.withPins {
			u.Pins = pins
		}
		if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); ok != tc.matches {
			t.Errorf("%s: login alice = %q %v", tc.name, user, ok)
		}
		if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice-gh"}); tc.withPins && (!ok || user != "alice") {
			t.Errorf("%s: the override's login = %q %v", tc.name, user, ok)
		}
		if p := pins.ids.ByUser["alice"]; tc.withPins && p.GitHubID != 1 {
			t.Errorf("%s: alice pinned to %+v", tc.name, p)
		}
	}
}

func TestUsersRefuseUsernamesGerritReadsAsOtherAccounts(t *testing.T) {
	// Gerrit takes "1000000" as account 1000000 and "me"/"self" as the
	// caller wherever it takes an account, so whoever registers such a
	// login on GitHub (and Gerrit) would act as someone else.
	u := &Users{
		Gerrit:    fakeAccounts{users: []string{"1000000", "Self", "me", "alice2"}},
		Overrides: map[string]string{"bob": "1000001"},
	}
	ctx := context.Background()
	for _, login := range []string{"1000000", "self", "me", "bob"} {
		if user, ok := u.GerritUserFor(ctx, github.User{Login: login}); ok {
			t.Errorf("%s acts as %q", login, user)
		}
	}
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice2"}); !ok || user != "alice2" {
		t.Errorf("alice2 = %q %v", user, ok)
	}
}

func TestUsersRefuseLoginsMatchingSeveralAccounts(t *testing.T) {
	u := &Users{Gerrit: fakeAccounts{users: []string{"Alice", "alice"}}}
	if user, ok := u.GerritUserFor(context.Background(), github.User{Login: "alice"}); ok {
		t.Errorf("alice acts as %q", user)
	}
}

func TestUsersNeverMatchTheBridgesOwnLogin(t *testing.T) {
	u := &Users{Overrides: map[string]string{"bridge-bot": "alice"}, NeverLogins: []string{"Bridge-Bot"}}
	if user, ok := u.GerritUserFor(context.Background(), github.User{Login: "bridge-bot"}); ok {
		t.Errorf("the bridge's login acts as %q", user)
	}
}

func TestUsersRefuseGitHubAccountsNewerThanTheGerritAccount(t *testing.T) {
	// "admin" was registered in Gerrit (2020) long before anyone took the
	// login on GitHub: that GitHub account cannot be admin's, however the
	// login came free.
	pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
	u := &Users{
		Gerrit: fakeAccounts{users: []string{"admin", "alice"}, registered: map[int]time.Time{
			1: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		Overrides: map[string]string{"admin-on-github": "admin"},
		Pins:      pins, GitHub: fakeIDs{"admin": 7, "alice": 8, "admin-on-github": 9},
	}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "admin"}); ok {
		t.Errorf("a newer GitHub account acts as %q", user)
	}
	// Pinned, if at all, to the override's account, never to this one.
	if p, ok := pins.ids.ByUser["admin"]; ok && p.GitHubID != 9 {
		t.Errorf("pinned: %+v", pins.ids.ByUser)
	}
	// Older GitHub accounts match and pin, and overrides are trusted as
	// written.
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); !ok || user != "alice" {
		t.Errorf("alice = %q %v", user, ok)
	}
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "admin-on-github"}); !ok || user != "admin" {
		t.Errorf("override = %q %v", user, ok)
	}
}

// Gerrit says 404 for a group the bridge may not see, as for one that
// does not exist: either way the exclusion cannot be checked, so the
// bridge will not start, and nobody is matched meanwhile.
func TestUsersNeverGroupsFailClosed(t *testing.T) {
	u := &Users{
		Gerrit:      fakeAccounts{users: []string{"alice", "admin"}, groups: map[string][]string{"Service Users": nil}},
		NeverGroups: []string{"Service Users", "Administrators"}, // Administrators: 404
	}
	ctx := context.Background()
	if err := u.CheckGroups(ctx); err == nil || !strings.Contains(err.Error(), "Administrators") {
		t.Errorf("CheckGroups = %v", err)
	}
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "admin"}); ok {
		t.Errorf("admin acts as %q", user)
	}
	u.NeverGroups = []string{"Service Users"}
	if err := u.CheckGroups(ctx); err != nil {
		t.Errorf("visible groups: %v", err)
	}
	// Members the bridge cannot list: an external group's, or a hidden
	// included group's.
	internal, hidden := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for inc, ok := range map[string]bool{internal: true, "github:org/admins": false, "ldap:cn=admins": false, hidden: false} {
		// (Even were Gerrit to answer for an external group: with nobody.)
		u.Gerrit = fakeAccounts{groups: map[string][]string{"Service Users": nil, internal: nil, "github:org/admins": nil, "ldap:cn=admins": nil},
			includes: map[string][]string{"Service Users": {inc}}}
		if err := u.CheckGroups(ctx); (err == nil) != ok {
			t.Errorf("including %s: %v", inc, err)
		}
	}
	// Checked at startup, then a hidden group included: once the check is
	// due again, nobody is matched until it passes (fourth audit).
	now := time.Now()
	u = &Users{Gerrit: fakeAccounts{users: []string{"alice"}, groups: map[string][]string{"Service Users": nil}},
		NeverGroups: []string{"Service Users"}, now: func() time.Time { return now }}
	if err := u.CheckGroups(ctx); err != nil {
		t.Fatal(err)
	}
	u.Gerrit = fakeAccounts{users: []string{"alice"}, groups: map[string][]string{"Service Users": nil}, includes: map[string][]string{"Service Users": {hidden}}}
	if _, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); !ok {
		t.Error("alice not matched while the last check holds")
	}
	now = now.Add(groupsRecheck + time.Second)
	u.cache = nil
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "alice"}); ok {
		t.Errorf("alice acts as %q though the excluded groups cannot all be listed", user)
	}
}

// One GitHub account, pinned to Gerrit bob, renamed onto the freed login
// of Gerrit maint (never pinned): not maint too.
func TestOneGitHubAccountIsNotPinnedToTwoGerritAccounts(t *testing.T) {
	ids := fakeIDs{"bob": 5}
	pins := &memPins{ids: &state.Identities{ByUser: map[string]state.Identity{}}}
	u := &Users{Gerrit: fakeAccounts{users: []string{"bob", "maint"}}, Pins: pins, GitHub: ids}
	ctx := context.Background()
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "bob", ID: 5}); !ok || user != "bob" {
		t.Fatalf("bob = %q %v", user, ok)
	}
	delete(ids, "bob")
	ids["maint"] = 5 // bob renamed himself to the freed login
	if user, ok := u.GerritUserFor(ctx, github.User{Login: "maint", ID: 5}); ok {
		t.Errorf("bob's GitHub account (pinned to Gerrit bob) now acts as Gerrit %q too; pins %+v", user, pins.ids.ByUser)
	}
}
