// Command testgerrit runs a local Gerrit, set up for the bridge, for manual
// testing:
//
//	bazel run //cmd/testgerrit
//	bazel run //cmd/bridge -- --config ~/tmp/gerrit-test/bridge/config.yaml
//
// The first run initializes the site and creates the bridge, alice and bob
// accounts, the project and its ACLs, and writes a bridge config with all
// the credentials under <site>/bridge/. Later runs reuse the site.
//
// With --replicate_project and --github_token_file, Gerrit's bundled
// replication plugin pushes that project's branches to the GitHub repo,
// as in production. With --ci_user, a CI account like gerrit-buildkite's
// is created, and its key, known_hosts and an ssh wrapper are written to
// <site>/ci/ for running gerrit-buildkite against this site (it always
// uses port 29418, the default here).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
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
	sshPort := flag.Int("ssh_port", 29418, "SSH port on 127.0.0.1.")
	project := flag.String("project", "bridge-test", "Project to create on first run.")
	github := flag.String("github", "AustinSchuh/bridge-scratch", "GitHub owner/repo for the generated bridge config.")
	listen := flag.String("bridge_listen", "127.0.0.1:18081", "Listen address for the generated bridge config.")
	replicate := flag.String("replicate_project", "", "Replicate this project's branches to the --github repo.")
	tokenFile := flag.String("github_token_file", "", "GitHub token for replication (stored in etc/secure.config).")
	ciUser := flag.String("ci_user", "", "Create this CI account (once), voting Verified on --replicate_project.")
	aliceEmail := flag.String("alice_email", "", "Also give alice this email (yours), so your commits can be pushed as alice.")
	identity := flag.String("identity", "AustinSchuh=alice,AustinSchuhBRT=bob", "GitHub login=Gerrit user pairs (comma separated) for the replicated project's bridge config.")
	bot := flag.String("github_bot", "", "gh account the bridge acts as on GitHub (its token comes from `gh auth token --user`).")
	bobEmail := flag.String("bob_email", "", "Also give bob this email, so a second GitHub account's commits can be pushed as bob.")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := testgerrit.Options{Site: *site, HTTPPort: *httpPort, SSHPort: *sshPort, Log: os.Stderr}
	if *replicate != "" {
		if *tokenFile == "" {
			return errors.New("--replicate_project needs --github_token_file")
		}
		tok, err := config.ReadSecret(*tokenFile)
		if err != nil {
			return err
		}
		opts.Replicate = &testgerrit.Replication{
			Name: "github", URL: "https://github.com/" + *github + ".git",
			Projects: []string{*replicate}, User: "x-access-token", Password: tok,
		}
	}
	s, err := testgerrit.Start(ctx, opts)
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
	if *replicate != "" {
		pcfg, err := mirrorProject(ctx, s, bridgeDir, *replicate, *github, *listen, *tokenFile, *identity, *bot)
		if err != nil {
			return err
		}
		log.Printf("Bridge config for %s: %s", *replicate, pcfg)
	}
	for user, email := range map[string]string{"alice": *aliceEmail, "bob": *bobEmail} {
		if email == "" {
			continue
		}
		if err := s.AddEmail(ctx, user, email); err != nil {
			return fmt.Errorf("%s's email: %w", user, err)
		}
	}
	if *ciUser != "" {
		if err := setupCI(ctx, s, filepath.Join(*site, "ci"), *replicate, *ciUser); err != nil {
			return err
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

// mirrorProject makes sure the replicated project exists, seeded from the
// GitHub repo's main, and that a bridge config for it exists.
func mirrorProject(ctx context.Context, s *testgerrit.Server, bridgeDir, project, repo, listen, tokenFile, identity, bot string) (string, error) {
	tok, err := config.ReadSecret(tokenFile)
	if err != nil {
		return "", err
	}
	exists, err := s.ProjectExists(ctx, project)
	if err != nil {
		return "", err
	}
	if !exists {
		log.Printf("Creating %s from github.com/%s main", project, repo)
		if err := s.MirrorProject(ctx, project, "https://github.com/"+repo+".git", tok, "main"); err != nil {
			return "", err
		}
	} else if err := s.ConfigureProject(ctx, project); err != nil {
		// Keep an existing site's config current (labels, requirements).
		return "", err
	} else if err := s.ReindexOpenChanges(ctx, project); err != nil {
		// Submit requirement results are stored in the index.
		return "", err
	}
	if err := s.GrantRunAs(ctx); err != nil {
		return "", err
	}
	path := filepath.Join(bridgeDir, project+".yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	ids := map[string]string{}
	for _, pair := range strings.Split(identity, ",") {
		if gh, user, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			ids[gh] = user
		}
	}
	owner, name, _ := strings.Cut(repo, "/")
	return s.WriteProjectConfig(bridgeDir, testgerrit.ProjectConfig{
		File: project + ".yaml", Project: project, Owner: owner, Repo: name, Listen: listen,
		Identity: ids, ImportCreatedAfter: time.Now(), ResyncInterval: 5 * time.Minute, TokenUser: bot,
	})
}

// setupCI creates the CI account once and writes what gerrit-buildkite
// needs into dir: the private key, known_hosts for localhost and
// 127.0.0.1, and bin/ssh, a wrapper that uses that known_hosts file
// (gerrit-buildkite runs plain `ssh`).
func setupCI(ctx context.Context, s *testgerrit.Server, dir, project, user string) error {
	key := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(key); err == nil {
		return nil
	}
	if project == "" {
		return errors.New("--ci_user needs --replicate_project (the project CI votes on)")
	}
	u, err := s.SetupCI(ctx, project, user)
	if err != nil {
		return fmt.Errorf("CI account: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	line, err := s.KnownHostsLine()
	if err != nil {
		return err
	}
	_, port, _ := strings.Cut(s.SSHAddr, ":")
	keyPart := line[strings.Index(line, " ")+1:]
	kh := fmt.Sprintf("[localhost]:%s %s\n[127.0.0.1]:%s %s\n", port, keyPart, port, keyPart)
	files := map[string]string{
		"id_ed25519":  string(u.PrivateKeyPEM),
		"known_hosts": kh,
		"bin/ssh": fmt.Sprintf("#!/bin/sh\n# ssh for gerrit-buildkite against the test Gerrit.\n"+
			"exec /usr/bin/ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -o IdentitiesOnly=yes \"$@\"\n",
			filepath.Join(dir, "known_hosts")),
	}
	for name, content := range files {
		mode := os.FileMode(0o600)
		if name == "bin/ssh" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			return err
		}
	}
	log.Printf("CI account %s ready; key and ssh wrapper in %s", user, dir)
	return nil
}

func waitErr(s *testgerrit.Server) <-chan error {
	c := make(chan error, 1)
	go func() { c <- s.Wait() }()
	return c
}
