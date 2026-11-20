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
		File: "p.yaml", Project: "p", Owner: "o", Repo: "r", Listen: "127.0.0.1:18083",
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
	if !c.GitHubOwned() || c.GitHub.UserApp == nil || c.GitHub.UserApp.TokensFile != "/t" ||
		c.Gerrit.URL != "http://127.0.0.1:18079/gerrit" || c.CI.ChecksURL != "http://127.0.0.1:18079/gerrit/bridge-p" ||
		c.Gerrit.MirrorDir != filepath.Join(dir, "p-mirror.git") {
		t.Errorf("config: %+v", c)
	}
}
