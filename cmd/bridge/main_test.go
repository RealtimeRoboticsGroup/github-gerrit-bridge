package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
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
