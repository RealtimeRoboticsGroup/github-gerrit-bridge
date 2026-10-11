package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
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
