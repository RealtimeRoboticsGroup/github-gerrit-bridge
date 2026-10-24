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
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerritchecks"
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
	statusListen := flag.String("bridge_status_listen", "127.0.0.1:18082", "Status page address for the generated --replicate_project bridge config (empty: none).")
	replicate := flag.String("replicate_project", "", "Replicate this project's branches to the --github repo.")
	tokenFile := flag.String("github_token_file", "", "GitHub token for replication (stored in etc/secure.config).")
	deployKey := flag.String("github_deploy_key_file", "", "Replicate over SSH with this private key (a GitHub deploy key with write access) instead of the token, "+
		"so a ruleset can let only it update main.")
	knownHosts := flag.String("github_known_hosts_file", "", "github.com host keys for --github_deploy_key_file (known_hosts format).")
	buildkiteURL := flag.String("buildkite_url", "", "CI's (fake) Buildkite API: the replicated project's bridge reads builds from it and "+
		"serves Gerrit's Checks tab, through plugins/bridge-checks.js.")
	ciUser := flag.String("ci_user", "", "Create this CI account (once), voting Verified on --replicate_project.")
	aliceEmail := flag.String("alice_email", "", "Also give alice this email (yours), so your commits can be pushed as alice.")
	identity := flag.String("identity", "", "GitHub login=Gerrit user overrides (comma separated) for the replicated project's bridge config; normally the bridge matches usernames.")
	basePath := flag.String("base_path", "", "Serve Gerrit under this path, like a site behind a proxy (e.g. /gerrit).")
	loginJars := flag.String("github_login_jars", "", "Directory with github-plugin.jar and github-oauth.jar: log in with GitHub, configured like AOS.")
	oauthFile := flag.String("github_oauth_file", "", "client_id=... and client_secret=... of the GitHub OAuth App.")
	webhookUser := flag.String("github_webhook_user", "", "github.webhookUser for the github plugin.")
	loginUsers := flag.String("github_login_users", "", "Accounts the github plugin creates at GitHub login, to set up once they exist: comma-separated login[|trusted][|admin][|email@...].")
	githubUsers := flag.String("github_users", "", "Accounts named after GitHub logins, like Gerrit's github plugin makes: comma-separated login|email[|trusted].")
	bot := flag.String("github_bot", "", "gh account the bridge acts as on GitHub (its token comes from `gh auth token --user`).")
	bobEmail := flag.String("bob_email", "", "Also give bob this email, so a second GitHub account's commits can be pushed as bob.")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := testgerrit.Options{Site: *site, HTTPPort: *httpPort, SSHPort: *sshPort, Log: os.Stderr, BasePath: *basePath}
	if *loginJars != "" {
		id, secret, err := readOAuth(*oauthFile)
		if err != nil {
			return err
		}
		opts.GitHubLogin = &testgerrit.GitHubLogin{JarsDir: *loginJars, ClientID: id, ClientSecret: secret, WebhookUser: *webhookUser}
	}
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
		if *deployKey != "" {
			hosts, err := os.ReadFile(*knownHosts)
			if err != nil {
				return fmt.Errorf("--github_known_hosts_file: %w", err)
			}
			opts.Replicate.URL = "git@github.com:" + *github + ".git"
			opts.Replicate.SSHKey, opts.Replicate.KnownHosts = *deployKey, string(hosts)
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
		pcfg, err := mirrorProject(ctx, s, bridgeDir, *replicate, *github, *listen, *statusListen, *tokenFile, *identity, *bot, *ciUser, *buildkiteURL)
		if err != nil {
			return err
		}
		if *buildkiteURL != "" {
			// Gerrit loads new plugins within a minute.
			js := gerritchecks.Plugin("http://"+*listen, *replicate)
			if err := os.WriteFile(filepath.Join(*site, "plugins", "bridge-checks.js"), js, 0o644); err != nil {
				return err
			}
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
	for _, spec := range strings.Split(*githubUsers, ",") {
		f := strings.Split(strings.TrimSpace(spec), "|")
		if f[0] == "" {
			continue
		}
		gu := testgerrit.GitHubUser{Login: f[0]}
		if len(f) > 1 {
			gu.Email = f[1]
		}
		gu.Trusted = len(f) > 2 && f[2] == "trusted"
		u, err := s.EnsureGitHubUser(ctx, gu)
		if err != nil {
			return fmt.Errorf("account %s: %w", gu.Login, err)
		}
		if u != nil {
			os.WriteFile(filepath.Join(bridgeDir, u.Username+".http-password"), []byte(u.HTTPPassword+"\n"), 0o600)
			os.WriteFile(filepath.Join(bridgeDir, u.Username+"_id_ed25519"), u.PrivateKeyPEM, 0o600)
			log.Printf("Created Gerrit account %s (credentials in %s)", u.Username, bridgeDir)
		}
	}
	if *ciUser != "" {
		if err := setupCI(ctx, s, filepath.Join(*site, "ci"), *replicate, *ciUser); err != nil {
			return err
		}
		// An HTTP password, so gerrit-buildkite can read a patch set's
		// real uploader over REST (-http_password_file).
		if _, err := s.SetUpLoginUser(ctx, *ciUser, nil, filepath.Join(*site, "ci", "http-password")); err != nil {
			return fmt.Errorf("CI HTTP password: %w", err)
		}
	}
	if *loginUsers != "" {
		go watchLoginUsers(ctx, s, bridgeDir, *loginUsers)
	}
	if opts.GitHubLogin != nil {
		log.Printf("Gerrit %s is ready (ssh %s). Log in with GitHub at %s/login", s.URL, s.SSHAddr, s.URL)
	} else {
		log.Printf("Gerrit %s is ready (ssh %s). Admin: log in at %s/login/?user_name=admin", s.URL, s.SSHAddr, s.URL)
	}
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
func mirrorProject(ctx context.Context, s *testgerrit.Server, bridgeDir, project, repo, listen, statusListen, tokenFile, identity, bot, ciUser, buildkiteURL string) (string, error) {
	tok, err := config.ReadSecret(tokenFile)
	if err != nil {
		return "", err
	}
	// Keep All-Projects' access AOS's (an older site's too).
	if err := s.ConfigureAllProjects(ctx); err != nil {
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
	if err := s.GrantRunAs(ctx, project); err != nil {
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
		File: project + ".yaml", Project: project, Owner: owner, Repo: name, Listen: listen, StatusListen: statusListen,
		Identity: ids, ImportCreatedAfter: time.Now(), ResyncInterval: 15 * time.Minute, TokenUser: bot, CIAccount: ciUser,
		Buildkite: buildkiteURL,
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

func readOAuth(path string) (id, secret string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "client_id":
			id = v
		case "client_secret":
			secret = v
		}
	}
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("%s needs client_id= and client_secret= lines", path)
	}
	return id, secret, nil
}

// watchLoginUsers waits for the github plugin to create each account (at
// that person's first GitHub login), then puts it in its groups and writes
// an HTTP password for pushing as it to <bridgeDir>/<login>.http-password.
func watchLoginUsers(ctx context.Context, s *testgerrit.Server, bridgeDir, spec string) {
	type want struct{ groups, emails []string }
	pending := map[string]want{}
	for _, e := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(e), "|")
		if f[0] == "" {
			continue
		}
		var w want
		for _, r := range f[1:] {
			switch {
			case r == "trusted":
				w.groups = append(w.groups, testgerrit.TrustedGroup, testgerrit.SubmitGroup)
			case r == "admin":
				// As AustinSchuh on AOS.
				w.groups = append(w.groups, "Administrators", "Sudo")
			case strings.Contains(r, "@"):
				// Like a user adding their address under Settings ->
				// Email Addresses (the plugin does not import it).
				w.emails = append(w.emails, r)
			}
		}
		pending[f[0]] = w
	}
	for len(pending) > 0 {
		for login, w := range pending {
			groups := w.groups
			ok, err := s.SetUpLoginUser(ctx, login, groups, filepath.Join(bridgeDir, login+".http-password"))
			for _, email := range w.emails {
				if ok && err == nil {
					err = s.SetPreferredEmail(ctx, login, email)
				}
			}
			if err != nil {
				log.Printf("setting up %s: %v", login, err)
			}
			if ok {
				log.Printf("%s logged in: in %v, HTTP password in %s", login, groups, bridgeDir)
				delete(pending, login)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}
