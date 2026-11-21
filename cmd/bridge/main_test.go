package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"bytes"
	"context"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/reconcile"
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

// Who may have Gerrit patch sets pushed to GitHub, where GitHub owns the
// branches: linked, and allowed to push.
func TestPublishable(t *testing.T) {
	ctx := context.Background()
	linked := func(_ context.Context, account int) (github.User, bool) {
		if account == 0 {
			return github.User{}, false
		}
		return github.User{Login: fmt.Sprint("u", account), ID: int64(account)}, true
	}
	asked := 0
	canPush := func(_ context.Context, u github.User) (bool, error) {
		asked++
		switch u.ID {
		case 1:
			return true, nil
		case 2:
			return false, nil
		}
		return false, errors.New("GitHub is down")
	}
	pub := publishable(linked, canPush, slog.Default())
	for account, want := range map[int][2]bool{0: {false, true}, 1: {true, true}, 2: {false, true}, 3: {false, false}} {
		ok, known := pub(ctx, account)
		if ok != want[0] || known != want[1] {
			t.Errorf("account %d: %v %v, want %v", account, ok, known, want)
		}
	}
	if asked != 3 {
		t.Errorf("asked GitHub %d times, want 3 (not for the unlinked)", asked)
	}
}

// /submit's merge goes out with the commenter's own linked token, at the
// head they saw; never for someone whose link is to another GitHub
// account, nor with the bridge's token.
func TestGitHubMergerMergesAsTheCommenter(t *testing.T) {
	var auth []string
	commits := "[]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/7/merge":
			auth = append(auth, r.Header.Get("Authorization"))
			var in github.MergeInput
			json.NewDecoder(r.Body).Decode(&in)
			if in.SHA != "head1" {
				t.Errorf("merged at %q", in.SHA)
			}
			io.WriteString(w, `{"sha":"m1","merged":true}`)
		case r.URL.Path == "/repos/o/r/pulls/7/commits":
			io.WriteString(w, commits)
		default:
			t.Errorf("asked %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	bot := &github.Client{BaseURL: srv.URL, Owner: "o", Repo: "r", Tokens: github.StaticToken("bot-token")}
	m := githubMerger{
		gerritUser: func(_ context.Context, u github.User) (string, bool) {
			return map[string]string{"alice": "alice", "carol": "carol"}[u.Login], u.Login != "bob"
		},
		account: func(_ context.Context, user string) (int, error) {
			return map[string]int{"alice": 1, "carol": 3}[user], nil
		},
		tokenFor: func(_ context.Context, account int) (string, github.User, bool, error) {
			switch account {
			case 1:
				return "alice-token", github.User{Login: "alice", ID: 11}, true, nil
			case 3: // linked to another GitHub account than the commenter
				return "carol-token", github.User{Login: "carol-old", ID: 99}, true, nil
			}
			return "", github.User{}, false, nil
		},
		source: func(_ int64, tok string) github.TokenSource { return github.StaticToken(tok) },
		gh:     bot,
	}
	ctx := context.Background()
	sha, linked, err := m.Merge(ctx, github.User{Login: "alice", ID: 11}, 7, "head1", github.MergeInput{Method: "squash"})
	if err != nil || !linked || sha != "m1" || len(auth) != 1 || auth[0] != "Bearer alice-token" {
		t.Errorf("alice: %q %v %v, auth %v", sha, linked, err, auth)
	}
	for _, u := range []github.User{{Login: "bob", ID: 12}, {Login: "carol", ID: 13}, {Login: "alice", ID: 0}} {
		if _, linked, err := m.Merge(ctx, u, 7, "head1", github.MergeInput{}); linked || err != nil {
			t.Errorf("%s: linked %v %v", u.Login, linked, err)
		}
	}
	if len(auth) != 1 {
		t.Errorf("merged %d times, want only alice's", len(auth))
	}
	// Only git subtree's commits make it a subtree: an "Update branch"
	// merge does not, nor its trailer on a commit that is not a root, nor
	// a real one beside a commit carrying a Change-Id.
	for list, want := range map[string]bool{
		`[{"sha":"s","commit":{"message":"Fake\n\ngit-subtree-dir: x"},"parents":[{"sha":"p"}]}]`:                                                                                                                                                                                  false,
		`[{"sha":"s","commit":{"message":"Squashed\n\ngit-subtree-dir: x"},"parents":[]},{"sha":"c","commit":{"message":"Other\n\nChange-Id: I0000000000000000000000000000000000000002"},"parents":[{"sha":"s"}]}]`:                                                                false,
		`[{"sha":"s","commit":{"message":"Squashed\n\ngit-subtree-dir: x"},"parents":[]},{"sha":"b","commit":{"message":"Other\n\nchange-id: I0000000000000000000000000000000000000002"},"parents":[{"sha":"s"}]}]`:                                                                false, // any case, as Gerrit reads footers
		`[{"sha":"a","commit":{"message":"x"},"parents":[{"sha":"p"}]},{"sha":"b","commit":{"message":"Merge branch 'main' into topic"},"parents":[{"sha":"a"},{"sha":"m"}]}]`:                                                                                                     false,
		`[{"sha":"s","commit":{"message":"Squashed 'third_party/x/' content from commit abc\n\ngit-subtree-dir: third_party/x\ngit-subtree-split: abc"},"parents":[]},{"sha":"b","commit":{"message":"Merge commit 's' as 'third_party/x'"},"parents":[{"sha":"a"},{"sha":"s"}]}]`: true,
	} {
		commits = list
		head := "b" // the last listed, in the true case
		if has, err := m.HasSubtree(ctx, 7, head); has != want || err != nil {
			t.Errorf("subtree %v, want %v (%v): %s", has, want, err, list)
		}
		// Not at the head seen (pushed since): not vetted, squashed.
		if has, _ := m.HasSubtree(ctx, 7, "newer"); has {
			t.Errorf("subtree at another head: %s", list)
		}
	}
}

// A GitHub-owned bridge needs Maintain Server, and will not start
// without it; a Gerrit-owned one never asks.
func TestCheckCapabilities(t *testing.T) {
	for _, tc := range []struct {
		owner, caps string
		asked, ok   bool
	}{
		{config.OwnerGitHub, `{"maintainServer":true}`, true, true},
		{config.OwnerGitHub, `{}`, true, false},
		{config.OwnerGerrit, `{}`, false, true},
		{config.OwnerGitHub, "", true, false}, // Gerrit fails to answer: not started either
	} {
		asked := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/a/accounts/self/capabilities" || r.URL.Query().Get("q") != "maintainServer" {
				t.Errorf("asked %s", r.URL)
			}
			asked = true
			if tc.caps == "" {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			io.WriteString(w, ")]}'\n"+tc.caps)
		}))
		rest := &gerrit.Client{BaseURL: srv.URL, User: "bridge", Password: "p", HTTP: srv.Client()}
		err := checkCapabilities(context.Background(), &config.Config{BranchOwner: tc.owner}, rest)
		srv.Close()
		if asked != tc.asked || (err == nil) != tc.ok {
			t.Errorf("%s %s: asked %v, err %v", tc.owner, tc.caps, asked, err)
		}
	}
}

// The identity mapping is what the config says: username matching only if
// on, the LDAP directory only if set (AOS's config: none), trusting its CA
// file, and the bridge's own account and login never matched.
func TestNewUsers(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	rest := &gerrit.Client{BaseURL: "http://gerrit.invalid"}
	off := false
	for _, tc := range []struct {
		name string
		id   config.Identity
		dir  bool
		err  bool
	}{
		{"AOS", config.Identity{}, false, false},
		{"LDAP", config.Identity{MatchUsernames: &off, LDAP: &config.LDAP{URL: "ldaps://ldap.spacecookies.dev", CAFile: ca, BaseDN: "ou=People", ObjectClass: "githubAccount", Refresh: time.Minute}}, true, false},
		{"no CA file", config.Identity{LDAP: &config.LDAP{URL: "ldaps://ldap.spacecookies.dev", CAFile: ca + ".missing", BaseDN: "ou=People"}}, false, true},
	} {
		cfg := &config.Config{Identity: tc.id}
		users, dir, err := newUsers(cfg, rest, nil, nil, "github-bridge", "bridge-bot", false, slog.Default())
		if (err != nil) != tc.err {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if err != nil {
			continue
		}
		if (dir != nil) != tc.dir || (users.Directory != nil) != tc.dir || (users.Gerrit != nil) != tc.id.MatchesUsernames() || users.Active == nil {
			t.Errorf("%s: directory %v, users %+v", tc.name, dir, users)
		}
		if dir != nil && (dir.BaseDN != "ou=People" || dir.Refresh != time.Minute || dir.Searcher.(*ldap.Server).RootCAs == nil) {
			t.Errorf("%s: directory %+v", tc.name, dir)
		}
		if len(users.Never) != 1 || users.Never[0] != "github-bridge" || len(users.NeverLogins) != 1 || users.NeverLogins[0] != "bridge-bot" {
			t.Errorf("%s: never %v %v", tc.name, users.Never, users.NeverLogins)
		}
	}
}

// Everything that depends on who owns the branches follows the mode,
// and the budgets exempt whoever may have code pushed in it: CI's trusted
// group where Gerrit owns them, linked writers where GitHub does.
func TestSetMode(t *testing.T) {
	ctx := context.Background()
	for _, owned := range []bool{false, true} {
		code := &reconcile.Code{Importer: &reconcile.Importer{},
			Trusted:     func(_ context.Context, a int) (bool, bool) { return a == 1, true },
			Publishable: func(_ context.Context, a int) (bool, bool) { return a == 2, true }}
		comments, commands, checks := &reconcile.Comments{}, &reconcile.Commands{}, &reconcile.Checks{}
		setMode(owned, code, comments, commands, checks)
		if code.GitHubOwned != owned || code.Importer.GitHubOwned != owned || commands.GitHubOwned != owned || checks.GitHubOwned != owned {
			t.Errorf("GitHub owns %v: code %v importer %v commands %v checks %v", owned, code.GitHubOwned, code.Importer.GitHubOwned, commands.GitHubOwned, checks.GitHubOwned)
		}
		exempt := map[bool]int{false: 1, true: 2}[owned]
		for name, b := range map[string]*reconcile.Budget{"pr": code.PRBudget, "text": code.TextBudget, "post": comments.PostBudget, "status": checks.StatusBudget} {
			if b == nil || b.Trusted == nil {
				t.Errorf("GitHub owns %v: %s budget %+v", owned, name, b)
				continue
			}
			if ok, _ := b.Trusted(ctx, exempt); !ok {
				t.Errorf("GitHub owns %v: %s budget does not exempt account %d", owned, name, exempt)
			}
			if ok, _ := b.Trusted(ctx, 3-exempt); ok {
				t.Errorf("GitHub owns %v: %s budget exempts account %d", owned, name, 3-exempt)
			}
		}
	}
}
