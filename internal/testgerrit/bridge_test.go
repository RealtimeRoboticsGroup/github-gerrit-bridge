package testgerrit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
)

// A GitHub-owned project's generated config is one the bridge accepts:
// GitHub owns its branches, it links accounts, and its Checks tab is
// under the front proxy's Gerrit path.
func TestWriteProjectConfigGitHubOwned(t *testing.T) {
	dir := t.TempDir()
	s := &Server{URL: "http://127.0.0.1:18080/gerrit", SSHAddr: "127.0.0.1:29418"}
	path, err := s.WriteProjectConfig(dir, ProjectConfig{
		File: "p.yaml", Project: "p", Owner: "o", Repo: "r", Listen: "127.0.0.1:18083", StatusListen: "127.0.0.1:18084",
		Account: GitHubOwnedAccount, Secrets: "p-",
		Identity: map[string]string{"gh": "user"}, TokenUser: "bot",
		GitHubOwned: true, GerritURL: "http://127.0.0.1:18079/gerrit", ChecksURL: "http://127.0.0.1:18079/gerrit/bridge-p",
		UserApp: &UserApp{ClientID: "Iv1", ClientSecretFile: "/s", PublicURL: "http://127.0.0.1:18083", TokensFile: "/t", KeyFile: "/k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("%v\n%s", err, b)
	}
	if a := c.GitHub.UserApp; !c.GitHubOwned() || a == nil || a.TokensFile != "/t" || a.KeyFile != "/k" || a.ClientID != "Iv1" ||
		a.ClientSecretFile != "/s" || a.PublicURL != "http://127.0.0.1:18083" || c.StatusListen != "127.0.0.1:18084" ||
		c.Gerrit.URL != "http://127.0.0.1:18079/gerrit" || c.CI.ChecksURL != "http://127.0.0.1:18079/gerrit/bridge-p" ||
		c.Gerrit.MirrorDir != filepath.Join(dir, "p-mirror.git") {
		t.Errorf("config: %+v", c)
	}
	// Its own account and secrets, not the first bridge's (thirteenth
	// review: they shared one account, Maintain Server and all).
	if g := c.Gerrit; g.HTTPUser != GitHubOwnedAccount || g.SSH.User != GitHubOwnedAccount ||
		g.HTTPPasswordFile != filepath.Join(dir, "p-gerrit-http-password") || g.SSH.KeyFile != filepath.Join(dir, "p-id_ed25519") ||
		g.SSH.PeerKeyFile != filepath.Join(dir, "p-peer_key") || c.GitHub.WebhookSecretFile != filepath.Join(dir, "p-webhook-secret") {
		t.Errorf("config: %+v", c)
	}
}
