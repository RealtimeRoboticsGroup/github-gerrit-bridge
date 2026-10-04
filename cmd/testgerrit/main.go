// Command testgerrit runs a local Gerrit, set up for the bridge, for manual
// testing:
//
//	bazel run //cmd/testgerrit
//	bazel run //cmd/bridge -- --config ~/tmp/gerrit-test/bridge/config.yaml
//
// The first run initializes the site and creates the bridge, alice and bob
// accounts, the project and its ACLs, and writes a bridge config with all
// the credentials under <site>/bridge/. Later runs reuse the site.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/testgerrit"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	home, _ := os.UserHomeDir()
	site := flag.String("site", filepath.Join(home, "tmp", "gerrit-test"), "Gerrit site directory.")
	httpPort := flag.Int("http_port", 18080, "HTTP port on 127.0.0.1.")
	sshPort := flag.Int("ssh_port", 29419, "SSH port on 127.0.0.1.")
	project := flag.String("project", "bridge-test", "Project to create on first run.")
	github := flag.String("github", "AustinSchuh/bridge-scratch", "GitHub owner/repo for the generated bridge config.")
	listen := flag.String("bridge_listen", "127.0.0.1:18081", "Listen address for the generated bridge config.")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := testgerrit.Start(ctx, testgerrit.Options{
		Site: *site, HTTPPort: *httpPort, SSHPort: *sshPort, Log: os.Stderr,
	})
	if err != nil {
		return err
	}
	defer s.Stop()

	bridgeDir := filepath.Join(*site, "bridge")
	cfg := filepath.Join(bridgeDir, "config.yaml")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		env, err := s.SetupBridge(ctx, *project)
		if err != nil {
			return fmt.Errorf("setting up bridge (delete %s to start over): %w", *site, err)
		}
		owner, repo, _ := strings.Cut(*github, "/")
		if cfg, err = s.WriteBridgeConfig(bridgeDir, env, owner, repo, *listen); err != nil {
			return err
		}
		for _, u := range []*testgerrit.User{env.Alice, env.Bob} {
			p := filepath.Join(bridgeDir, u.Username+".http-password")
			os.WriteFile(p, []byte(u.HTTPPassword+"\n"), 0o600)
			os.WriteFile(filepath.Join(bridgeDir, u.Username+"_id_ed25519"), u.PrivateKeyPEM, 0o600)
		}
	}
	log.Printf("Gerrit %s is ready (ssh %s). Admin: log in at %s/login/?user_name=admin", s.URL, s.SSHAddr, s.URL)
	log.Printf("Bridge config: %s", cfg)
	select {
	case <-ctx.Done():
		return nil
	case err := <-waitErr(s):
		return fmt.Errorf("gerrit exited: %v", err)
	}
}

func waitErr(s *testgerrit.Server) <-chan error {
	c := make(chan error, 1)
	go func() { c <- s.Wait() }()
	return c
}
