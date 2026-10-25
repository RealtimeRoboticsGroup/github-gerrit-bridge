// Package usertokens keeps the GitHub tokens of users who linked their
// GitHub account (design §7.2), so the bridge can post their Gerrit
// comments and votes on GitHub as them.
package usertokens

import (
	"bytes"
	"cmp"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Record is one linked user: their GitHub account, the Gerrit account it
// was bound to when they linked, and their token.
type Record struct {
	GitHub github.User `json:"github"`
	// GerritAccount and GerritUser are the Gerrit account the bridge's
	// identity mapping gave for GitHub when they linked. Their token is
	// used only for that account's posts.
	GerritAccount int              `json:"gerrit_account"`
	GerritUser    string           `json:"gerrit_user"`
	Token         github.UserToken `json:"token"`
	Linked        time.Time        `json:"linked"`
}

// Store keeps Records in a file, encrypted (AES-256-GCM) with a key kept
// elsewhere: a copy of the file alone gives nobody's token away.
type Store struct {
	Path string
	Key  []byte // 32 bytes

	mu   sync.Mutex
	recs map[int64]Record
}

// magic starts the file, and is authenticated with it.
var magic = []byte("bridge-user-tokens-v1\n")

// ReadKey reads a 32-byte key file: 64 hex digits (and whitespace), or
// the 32 bytes themselves.
func ReadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if k, err := hex.DecodeString(string(bytes.TrimSpace(raw))); err == nil && len(k) == 32 {
		return k, nil
	}
	if len(raw) == 32 {
		return raw, nil
	}
	return nil, fmt.Errorf("%s: want 32 bytes, or 64 hex digits", path)
}

func (s *Store) aead() (cipher.AEAD, error) {
	if len(s.Key) != 32 {
		return nil, errors.New("user token key must be 32 bytes")
	}
	b, err := aes.NewCipher(s.Key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Load reads the file; none is an empty store.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = map[int64]Record{}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	g, err := s.aead()
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(raw, magic) || len(raw) < len(magic)+g.NonceSize() {
		return fmt.Errorf("%s: not a user token file", s.Path)
	}
	rest := raw[len(magic):]
	plain, err := g.Open(nil, rest[:g.NonceSize()], rest[g.NonceSize():], magic)
	if err != nil {
		return fmt.Errorf("%s: cannot decrypt (wrong key?)", s.Path)
	}
	var recs []Record
	if err := json.Unmarshal(plain, &recs); err != nil {
		return fmt.Errorf("%s: %w", s.Path, err)
	}
	for _, r := range recs {
		s.recs[r.GitHub.ID] = r
	}
	return nil
}

// save writes the records, atomically. Called with mu held.
func (s *Store) save() error {
	recs := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		recs = append(recs, r)
	}
	slices.SortFunc(recs, func(a, b Record) int { return cmp.Compare(a.GitHub.ID, b.GitHub.ID) })
	plain, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	g, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append(append(slices.Clone(magic), nonce...), g.Seal(nil, nonce, plain, magic)...)
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".user-tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Get returns GitHub account id's record.
func (s *Store) Get(id int64) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	return r, ok
}

// All returns every record.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Record) int { return cmp.Compare(a.GitHub.ID, b.GitHub.ID) })
	return out
}

// Put adds or replaces r, and drops any other record bound to its Gerrit
// account: one GitHub account posts for a Gerrit account, the one linked
// last. The record is kept in memory even if the file cannot be written.
func (s *Store) Put(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = map[int64]Record{}
	}
	for id, o := range s.recs {
		if id != r.GitHub.ID && o.GerritAccount == r.GerritAccount {
			delete(s.recs, id)
		}
	}
	s.recs[r.GitHub.ID] = r
	return s.save()
}

// Delete drops GitHub account id's record.
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[id]; !ok {
		return nil
	}
	delete(s.recs, id)
	return s.save()
}
