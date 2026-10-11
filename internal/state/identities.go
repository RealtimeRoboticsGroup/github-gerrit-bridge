package state

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// IdentitiesRef holds which GitHub account each Gerrit account was first
// seen acting as (design §7.1): identities.json, a commit per change.
const IdentitiesRef = "refs/bridge/identities"

// Identity pins a Gerrit account to a GitHub account by its numeric id,
// which, unlike the login, never passes to someone else.
type Identity struct {
	GitHubID    int64     `json:"github_id"`
	GitHubLogin string    `json:"github_login"` // when pinned; for people
	Pinned      time.Time `json:"pinned"`
}

// Identities are the pins, by lowercased Gerrit username.
type Identities struct {
	// Version is the commit they were loaded from ("" if none yet).
	Version string `json:"-"`
	ByUser  map[string]Identity
}

// LoadIdentities reads the pins from the mirror.
func (s *Store) LoadIdentities(ctx context.Context) (*Identities, error) {
	ids := &Identities{ByUser: map[string]Identity{}}
	ver, err := s.Repo.RevParse(ctx, IdentitiesRef+"^{commit}")
	if err != nil || ver == "" {
		return ids, err
	}
	ids.Version = ver
	b, err := s.Repo.Git(ctx, nil, "cat-file", "blob", ver+":identities.json")
	if err != nil {
		return nil, err
	}
	return ids, json.Unmarshal(b, &ids.ByUser)
}

// SaveIdentities writes ids as a new commit, if the ref is still at
// ids.Version; otherwise it returns ErrConflict (reload and retry).
func (s *Store) SaveIdentities(ctx context.Context, ids *Identities, msg string) error {
	b, err := json.MarshalIndent(ids.ByUser, "", "  ")
	if err != nil {
		return err
	}
	out, err := s.Repo.Git(ctx, append(b, '\n'), "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	out, err = s.Repo.Git(ctx, []byte("100644 blob "+strings.TrimSpace(string(out))+"\tidentities.json\n"), "mktree")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", strings.TrimSpace(string(out)), "-m", msg}
	if ids.Version != "" {
		args = append(args, "-p", ids.Version)
	}
	out, err = s.Repo.Git(ctx, nil, args...)
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(out))
	if err := s.pushRef(ctx, IdentitiesRef, ids.Version, commit); err != nil {
		return err
	}
	ids.Version = commit
	return nil
}
