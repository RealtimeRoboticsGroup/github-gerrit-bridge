package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

type fakeAccounts struct {
	users  []string
	groups map[string][]string
}

func (f fakeAccounts) QueryAccounts(_ context.Context, q string) ([]gerrit.AccountInfo, error) {
	var out []gerrit.AccountInfo
	for _, u := range f.users {
		if strings.Contains(strings.ToLower(q), strings.ToLower(u)) {
			out = append(out, gerrit.AccountInfo{Username: u})
		}
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

func TestUsersNeverMatchesServiceAccounts(t *testing.T) {
	u := &Users{
		Gerrit:      fakeAccounts{users: []string{"alice", "github-bridge", "buildkite"}, groups: map[string][]string{"Service Users": {"buildkite"}}},
		Never:       []string{"github-bridge"},
		NeverGroups: []string{"Service Users", "Non-Interactive Users"},
	}
	ctx := context.Background()
	if user, ok := u.GerritUser(ctx, "Alice"); !ok || user != "alice" {
		t.Errorf("alice = %q %v", user, ok)
	}
	// Someone who registers the bridge's or CI's name on GitHub gets nothing.
	for _, login := range []string{"github-bridge", "BuildKite"} {
		if user, ok := u.GerritUser(ctx, login); ok {
			t.Errorf("%s matched %q", login, user)
		}
	}
}

type fakeIDs map[string]int64

func (f fakeIDs) UserID(_ context.Context, login string) (int64, error) {
	if id, ok := f[strings.ToLower(login)]; ok {
		return id, nil
	}
	return 0, errors.New("404 Not Found")
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
	if user, ok := u.GerritUser(ctx, "alice"); !ok || user != "alice" {
		t.Fatalf("first sight = %q %v", user, ok)
	}
	if pins.ids.ByUser["alice"].GitHubID != 1 {
		t.Fatalf("not pinned: %+v", pins.ids.ByUser)
	}
	// Alice renames her GitHub account (an override maps the new login):
	// same id, still her.
	if user, ok := u.GerritUser(ctx, "alice-renamed"); !ok || user != "alice" {
		t.Errorf("renamed = %q %v", user, ok)
	}
	// Someone else registers the freed login "alice".
	ids["alice"] = 2
	u.cache = nil
	if user, ok := u.GerritUser(ctx, "alice"); ok {
		t.Errorf("new owner of the login acts as %q", user)
	}
	// Unknown on GitHub (deleted): nobody.
	delete(ids, "alice")
	u.cache = nil
	if _, ok := u.GerritUser(ctx, "alice"); ok {
		t.Error("deleted GitHub account still maps")
	}
}
