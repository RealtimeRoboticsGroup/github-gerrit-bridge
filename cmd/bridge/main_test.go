package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"bytes"
	"context"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/usertokens"
	"os"
	"path/filepath"
)

// The bridge's clients never follow a redirect with its credentials.
func TestNoRedirects(t *testing.T) {
	followed := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := &http.Client{CheckRedirect: noRedirects}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if followed || resp.StatusCode != http.StatusFound {
		t.Errorf("followed=%v status=%d", followed, resp.StatusCode)
	}
}

// Every secret file is checked for loose permissions, Buildkite's too.
func TestSecretFiles(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gerrit.SSH.PeerKeyFile = "peer"
	cfg.CI.Buildkite = &config.Buildkite{TokenFile: "bk"}
	if got := secretFiles(cfg); !slices.Contains(got, "peer") || !slices.Contains(got, "bk") {
		t.Errorf("secret files = %v", got)
	}
}

// A key readable by others stops the bridge; another secret is warned
// about (tenth audit).
func TestLooseKeysAreRefused(t *testing.T) {
	dir := t.TempDir()
	file := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	for _, set := range []func(cfg *config.Config, p string){
		func(cfg *config.Config, p string) { cfg.Gerrit.SSH.PeerKeyFile = p },
		func(cfg *config.Config, p string) { cfg.Gerrit.SSH.KeyFile = p },
		func(cfg *config.Config, p string) { cfg.GitHub.UserApp = &config.UserApp{KeyFile: p} },
	} {
		cfg := &config.Config{}
		set(cfg, file("key", 0o640))
		if err := checkSecrets(cfg, slog.Default()); err == nil {
			t.Errorf("a loose key passed: %+v", cfg)
		}
		set(cfg, file("tight", 0o600))
		if err := checkSecrets(cfg, slog.Default()); err != nil {
			t.Errorf("a 0600 key: %v", err)
		}
	}
	cfg := &config.Config{}
	cfg.Gerrit.HTTPPasswordFile = file("pw", 0o644)
	if err := checkSecrets(cfg, slog.Default()); err != nil {
		t.Errorf("a loose password stops the bridge: %v", err)
	}
}

// The Gerrit events line counts each type, most first, and says how long
// ago the last came.
func TestGerritEvents(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if got := gerritEvents(nil, time.Time{}, now); got != "none since start" {
		t.Errorf("no events: %q", got)
	}
	got := gerritEvents(map[string]int{"patchset-created": 2, "comment-added": 5, "change-merged": 2}, now.Add(-3*time.Second), now)
	if want := "9 since start (comment-added 5, change-merged 2, patchset-created 2), the last 3s ago"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A linked user's posts go to the bridged repository with their own
// token; anyone else's are the bridge's.
func TestLinkedAuthors(t *testing.T) {
	ts := &usertokens.Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: bytes.Repeat([]byte{1}, 32)}
	ts.Put(usertokens.Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_alice"}})
	a := linkedAuthors{tokens: &usertokens.Tokens{Store: ts}, gh: &github.Client{BaseURL: "https://api.example", Owner: "o", Repo: "r"}}
	api, gh, ok, err := a.Client(context.Background(), 1000)
	c, _ := api.(*github.Client)
	if err != nil || !ok || gh.ID != 42 || c == nil || c.Owner != "o" || c.Repo != "r" || c.BaseURL != "https://api.example" {
		t.Fatalf("client %+v %+v %v %v", c, gh, ok, err)
	}
	if tok, _ := c.Tokens.Token(context.Background()); tok != "ghu_alice" {
		t.Errorf("token %q", tok)
	}
	if _, _, ok, _ := a.Client(context.Background(), 1001); ok {
		t.Error("an unlinked account has a client")
	}
	if u, ok := a.Linked(context.Background(), 1000); !ok || u.Login != "alice" {
		t.Errorf("Linked = %+v %v", u, ok)
	}
}

// Where GitHub owns the branches, the merge settings that keep merged
// changes from merging in Gerrit are said at startup.
func TestMergeSettingProblems(t *testing.T) {
	yes, no := true, false
	good := &github.Repository{AllowSquashMerge: &yes, AllowMergeCommit: &yes, AllowRebaseMerge: &no,
		SquashMergeCommitMessage: "PR_BODY", MergeCommitMessage: "PR_BODY"}
	if p := mergeSettingProblems(good); len(p) != 0 {
		t.Errorf("good settings: %v", p)
	}
	for name, r := range map[string]github.Repository{
		"unreadable":            {},
		"squash from commits":   {AllowSquashMerge: &yes, AllowMergeCommit: &no, AllowRebaseMerge: &no, SquashMergeCommitMessage: "COMMIT_MESSAGES", MergeCommitMessage: "PR_BODY"},
		"merge from PR title":   {AllowSquashMerge: &no, AllowMergeCommit: &yes, AllowRebaseMerge: &no, SquashMergeCommitMessage: "PR_BODY", MergeCommitMessage: "PR_TITLE"},
		"rebase merges allowed": {AllowSquashMerge: &yes, AllowMergeCommit: &no, AllowRebaseMerge: &yes, SquashMergeCommitMessage: "PR_BODY", MergeCommitMessage: "PR_BODY"},
	} {
		if p := mergeSettingProblems(&r); len(p) != 1 {
			t.Errorf("%s: %v, want one problem", name, p)
		}
	}
	// What the button does not offer does not matter.
	off := &github.Repository{AllowSquashMerge: &no, AllowMergeCommit: &yes, AllowRebaseMerge: &no, SquashMergeCommitMessage: "BLANK", MergeCommitMessage: "PR_BODY"}
	if p := mergeSettingProblems(off); len(p) != 0 {
		t.Errorf("squash off: %v", p)
	}
}
