package config

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const full = `
listen: ":9000"
gerrit:
  url: https://gerrit.example.com/
  project: frc971/971-Robot-Code
  http_user: github-bridge
  http_password_file: /etc/bridge/gerrit-password
  mirror_dir: /var/cache/bridge/mirror.git
  ssh:
    address: gerrit.example.com:29418
    user: github-bridge
    key_file: /etc/bridge/id_ed25519
    known_hosts_file: /etc/bridge/known_hosts
    keepalive_interval: 30s
    watchdog: 1h
github:
  owner: frc971
  repo: 971-Robot-Code
  webhook_secret_file: /etc/bridge/webhook-secret
  token_file: /etc/bridge/github-token
  import_created_after: 2026-10-04T07:50:00Z
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.Gerrit.URL != "https://gerrit.example.com" {
		t.Errorf("URL not trimmed: %q", c.Gerrit.URL)
	}
	if c.Gerrit.SSH.KeepaliveInterval != 30*time.Second {
		t.Errorf("KeepaliveInterval = %v", c.Gerrit.SSH.KeepaliveInterval)
	}
	if c.Gerrit.SSH.Watchdog != time.Hour {
		t.Errorf("Watchdog = %v", c.Gerrit.SSH.Watchdog)
	}
	if want := time.Date(2026, 10, 4, 7, 50, 0, 0, time.UTC); !c.GitHub.ImportCreatedAfter.Equal(want) {
		t.Errorf("ImportCreatedAfter = %v", c.GitHub.ImportCreatedAfter)
	}
	// Defaults survive for keys that were not set.
	if c.Gerrit.SSH.KeepaliveCountMax != 3 || c.Workers != 4 || c.CI.TrustedGroup != "Verified Users" {
		t.Errorf("defaults lost: %+v", c)
	}
}

//go:embed example.yaml
var example []byte

func TestExample(t *testing.T) {
	if _, err := Parse(example); err != nil {
		t.Fatalf("example.yaml: %v", err)
	}
}

func TestUnknownKey(t *testing.T) {
	_, err := Parse([]byte(full + "  webhok_secret: oops\n"))
	if err == nil || !strings.Contains(err.Error(), "webhok_secret") {
		t.Fatalf("err = %v, want unknown-field error", err)
	}
}

func TestMissingRequired(t *testing.T) {
	_, err := Parse([]byte("gerrit:\n  url: https://g\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"gerrit.project", "gerrit.ssh.key_file", "github.owner", "github.webhook_secret_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "gerrit.url") {
		t.Errorf("error mentions gerrit.url, which was set: %v", err)
	}
}

func TestExactlyOneToken(t *testing.T) {
	both := strings.Replace(full, "  token_file:", "  token_command: [gh, auth, token]\n  token_file:", 1)
	none := strings.Replace(full, "  token_file: /etc/bridge/github-token\n", "", 1)
	for name, in := range map[string]string{"both": both, "none": none} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s")
	os.WriteFile(p, []byte("  hunter2\n"), 0o600)
	if s, err := ReadSecret(p); err != nil || s != "hunter2" {
		t.Errorf("ReadSecret = %q, %v", s, err)
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := ReadSecret(p); err == nil {
		t.Error("empty secret accepted")
	}
}
