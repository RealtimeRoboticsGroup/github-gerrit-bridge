// Package testgerrit runs a throwaway local Gerrit for integration tests
// and manual testing, so the bridge never has to be pointed at a real
// server to be exercised.
//
// The site uses DEVELOPMENT_BECOME_ANY_ACCOUNT auth, so anyone who can
// reach it is an admin. It only ever listens on 127.0.0.1.
package testgerrit

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/crypto/ssh"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
)

// Options configure a site.
type Options struct {
	// Java and War are paths to the java binary and gerrit.war. Empty
	// means the Bazel runfiles named by $TESTGERRIT_JAVA and
	// $TESTGERRIT_WAR.
	Java, War string
	// Site is the site directory; it is initialized if empty.
	Site string
	// HTTPPort and SSHPort default to free ports.
	HTTPPort, SSHPort int
	// Log receives Gerrit's console output; nil discards it.
	Log *os.File
	// Replicate, if set, installs the bundled replication plugin and
	// pushes refs/heads/* of the given projects to a remote.
	Replicate *Replication
	// BasePath serves Gerrit under a path, like a site behind a proxy
	// (e.g. "/gerrit").
	BasePath string
	// GitHubLogin, if set, makes people log in with GitHub through the
	// github plugin, configured like AOS. A new site first boots in dev
	// mode once, to bootstrap the admin account.
	GitHubLogin *GitHubLogin
}

// GitHubLogin configures the github plugin for login.
type GitHubLogin struct {
	// JarsDir holds github-plugin.jar and github-oauth.jar.
	JarsDir                string
	ClientID, ClientSecret string
	// WebhookUser is github.webhookUser (set on AOS).
	WebhookUser string
}

// Replication configures the replication plugin for one remote.
type Replication struct {
	Name     string   // remote name, e.g. "github"
	URL      string   // e.g. https://github.com/owner/${name}.git
	Projects []string // projects to replicate
	User     string   // HTTP user, e.g. x-access-token
	Password string   // HTTP password or token; stored in etc/secure.config
	// SSHKey, if set, is a private key file to push over SSH with instead
	// (a GitHub deploy key, which a ruleset can let update protected
	// branches), and KnownHosts the remote's host keys. URL must then be
	// an SSH URL.
	SSHKey, KnownHosts string
}

// Server is a running Gerrit.
type Server struct {
	URL     string // http://127.0.0.1:<port>, no trailing slash
	SSHAddr string // 127.0.0.1:<port>
	Site    string
	// Admin is a REST client authenticated as the dev-mode admin.
	Admin *gerrit.Client

	cmd     *exec.Cmd
	done    chan struct{} // closed when Gerrit exits
	exitErr error
}

// Start initializes the site if needed, starts Gerrit, waits until it
// answers, and makes sure the admin has an HTTP password.
func Start(ctx context.Context, o Options) (*Server, error) {
	var err error
	if o.Java == "" {
		if o.Java, err = runfiles.Rlocation(os.Getenv("TESTGERRIT_JAVA")); err != nil {
			return nil, fmt.Errorf("java: %w", err)
		}
	}
	if o.War == "" {
		if o.War, err = runfiles.Rlocation(os.Getenv("TESTGERRIT_WAR")); err != nil {
			return nil, fmt.Errorf("gerrit.war: %w", err)
		}
	}
	if o.Site, err = filepath.Abs(o.Site); err != nil {
		return nil, err
	}
	for _, p := range []*int{&o.HTTPPort, &o.SSHPort} {
		if *p == 0 {
			if *p, err = freePort(); err != nil {
				return nil, err
			}
		} else if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(*p)); err != nil {
			// Otherwise a Gerrit already on the port would answer our
			// readiness check.
			return nil, fmt.Errorf("port %d: %w (is another Gerrit running?)", *p, err)
		} else {
			l.Close()
		}
	}
	s := &Server{
		URL:     "http://127.0.0.1:" + strconv.Itoa(o.HTTPPort) + strings.TrimRight(o.BasePath, "/"),
		SSHAddr: "127.0.0.1:" + strconv.Itoa(o.SSHPort),
		Site:    o.Site,
	}

	// The launcher unpacks its jars under $HOME/.gerritcodereview or
	// java.io.tmpdir; keep both next to the site, which is writable even
	// in a test sandbox.
	home := o.Site + ".home"
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0o755); err != nil {
		return nil, err
	}
	java := func(ctx context.Context, args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, o.Java, append([]string{
			"-Duser.home=" + home, "-Djava.io.tmpdir=" + filepath.Join(home, "tmp"), "-jar", o.War}, args...)...)
		c.Env = append(os.Environ(), "HOME="+home)
		return c
	}

	if _, err := os.Stat(filepath.Join(o.Site, "etc", "gerrit.config")); errors.Is(err, os.ErrNotExist) {
		init := java(ctx, "init", "--batch", "--dev", "--no-auto-start", "-d", o.Site)
		if out, err := init.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("gerrit init: %w\n%s", err, out)
		}
	}
	// (Re)apply the settings that keep the site local and quiet.
	for _, kv := range [][2]string{
		{"gerrit.canonicalWebUrl", s.URL + "/"},
		{"httpd.listenUrl", s.URL + "/"},
		{"sshd.listenAddress", s.SSHAddr},
		{"sendemail.enable", "false"},
		{"container.javaHome", filepath.Dir(filepath.Dir(o.Java))},
		{"index.batchThreads", "1"},
		// What anyone's PR may bring (the bridge also refuses PRs over
		// its own bounds, design §7.1).
		{"receive.maxObjectSizeLimit", "25m"},
		// Let people attach "Apply fix" suggestions to comments, which
		// the bridge maps to GitHub suggestions.
		{"experiments.enabled", "GerritBackendFeature__allow_fix_suggestions_in_comments"},
	} {
		c := exec.CommandContext(ctx, "git", "config", "-f", filepath.Join(o.Site, "etc", "gerrit.config"), kv[0], kv[1])
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git config %s: %w: %s", kv[0], err, out)
		}
	}

	if o.Replicate != nil {
		if err := installReplication(o.War, o.Site, home, o.Replicate); err != nil {
			return nil, fmt.Errorf("replication: %w", err)
		}
	}

	start := func() error {
		s.cmd = java(context.Background(), "daemon", "-d", o.Site, "--console-log")
		s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if o.Log != nil {
			s.cmd.Stdout, s.cmd.Stderr = o.Log, o.Log
		}
		if err := s.cmd.Start(); err != nil {
			return err
		}
		s.done = make(chan struct{})
		go func() {
			s.exitErr = s.cmd.Wait()
			close(s.done)
		}()
		if err := s.waitReady(ctx); err != nil {
			s.Stop()
			return err
		}
		return nil
	}
	setAuthDev := func() error {
		return gitConfig(filepath.Join(o.Site, "etc", "gerrit.config"),
			[]string{"auth.type", "DEVELOPMENT_BECOME_ANY_ACCOUNT"}, []string{"--unset", "httpd.filterClass"})
	}

	if o.GitHubLogin != nil {
		if _, err := os.Stat(filepath.Join(o.Site, "etc", "testgerrit-admin-password")); err != nil {
			// Bootstrap the admin's HTTP password in dev mode, once, the
			// way an operator sets up a site before turning on SSO.
			if err := setAuthDev(); err != nil {
				return nil, err
			}
			if err := start(); err != nil {
				return nil, err
			}
			_, err := s.adminPassword(ctx)
			s.Stop()
			if err != nil {
				return nil, fmt.Errorf("admin password: %w", err)
			}
		}
		// AOS's plugins/ (all bundled in the war); the github plugin binds
		// to replication-api.
		if err := installWarPlugins(o.War, o.Site, aosPlugins...); err != nil {
			return nil, err
		}
		if err := installGitHubLogin(o.Site, o.GitHubLogin); err != nil {
			return nil, fmt.Errorf("github plugin: %w", err)
		}
	} else if err := setAuthDev(); err != nil {
		return nil, err
	}
	if err := start(); err != nil {
		return nil, err
	}
	pw, err := s.adminPassword(ctx)
	if err != nil {
		s.Stop()
		return nil, fmt.Errorf("admin password: %w", err)
	}
	s.Admin = &gerrit.Client{BaseURL: s.URL, User: "admin", Password: pw, HTTP: &http.Client{Timeout: time.Minute}}
	return s, nil
}

// gitConfig applies `git config -f file` edits; "--unset" of a missing
// key is fine.
func gitConfig(file string, edits ...[]string) error {
	for _, e := range edits {
		out, err := exec.Command("git", append([]string{"config", "-f", file}, e...)...).CombinedOutput()
		if err != nil && !(e[0] == "--unset" || e[0] == "--unset-all") {
			return fmt.Errorf("git config %v: %w: %s", e, err, out)
		}
	}
	return nil
}

// installGitHubLogin installs the github plugin (github-plugin.jar in
// plugins/, github-oauth.jar in lib/) and configures GitHub login as on
// AOS (RealtimeRoboticsGroup's gerrit.config, 2026-10-10), plus what the
// plugin's init step writes: the client secret and the token cipher key.
func installGitHubLogin(site string, g *GitHubLogin) error {
	for jar, dir := range map[string]string{"github-plugin.jar": "plugins", "github-oauth.jar": "lib"} {
		b, err := os.ReadFile(filepath.Join(g.JarsDir, jar))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(site, dir), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(site, dir, jar), b, 0o644); err != nil {
			return err
		}
	}
	key := filepath.Join(site, "data", "github-plugin", "default.key")
	if _, err := os.Stat(key); err != nil {
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			return err
		}
		k := make([]byte, 16)
		rand.Read(k)
		if err := os.WriteFile(key, k, 0o600); err != nil {
			return err
		}
	}
	err := gitConfig(filepath.Join(site, "etc", "gerrit.config"),
		[]string{"auth.type", "HTTP"},
		[]string{"auth.userNameCaseInsensitive", "true"},
		[]string{"auth.httpHeader", "GITHUB_USER"},
		[]string{"auth.httpExternalIdHeader", "GITHUB_OAUTH_TOKEN"},
		[]string{"auth.loginUrl", "/login"},
		[]string{"auth.loginText", "Sign-in with GitHub"},
		[]string{"auth.registerPageUrl", "/#/register"},
		[]string{"httpd.filterClass", "com.gerritforge.gerrit.plugins.github.oauth.OAuthFilter"},
		[]string{"github.url", "https://github.com"},
		[]string{"github.apiUrl", "https://api.github.com"},
		[]string{"github.clientId", g.ClientID},
		[]string{"github.webhookUser", g.WebhookUser},
		[]string{"github-key.current.passwordDevice", key},
		[]string{"github-key.current.passwordLength", "16"},
		[]string{"github-key.current.current", "true"},
	)
	if err != nil {
		return err
	}
	secure := filepath.Join(site, "etc", "secure.config")
	if err := gitConfig(secure, []string{"github.clientSecret", g.ClientSecret}); err != nil {
		return err
	}
	return os.Chmod(secure, 0o600)
}

// Stop shuts Gerrit down and waits for it to exit.
func (s *Server) Stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// Wait blocks until Gerrit exits.
func (s *Server) Wait() error {
	<-s.done
	return s.exitErr
}

func (s *Server) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return fmt.Errorf("gerrit exited during startup: %v (see %s/logs)", s.exitErr, s.Site)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		resp, err := http.Get(s.URL + "/config/server/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return errors.New("gerrit did not become ready in 3 minutes")
}

// adminPassword returns the admin's HTTP password, generating one through
// a dev-mode browser login the first time and caching it in the site.
func (s *Server) adminPassword(ctx context.Context) (string, error) {
	cache := filepath.Join(s.Site, "etc", "testgerrit-admin-password")
	if b, err := os.ReadFile(cache); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: time.Minute}
	// init --dev creates "admin" in Administrators; become it.
	for _, p := range []string{"/login/?user_name=admin", "/"} {
		resp, err := hc.Get(s.URL + p)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
	}
	u, _ := url.Parse(s.URL)
	var xsrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "XSRF_TOKEN" {
			xsrf = c.Value
		}
	}
	if xsrf == "" {
		return "", errors.New("no XSRF_TOKEN cookie after dev login")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, s.URL+"/accounts/self/password.http",
		strings.NewReader(`{"generate":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gerrit-Auth", xsrf)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("PUT password.http: %d %s", resp.StatusCode, b)
	}
	var pw string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(b), ")]}'")), &pw); err != nil {
		return "", err
	}
	if err := os.WriteFile(cache, []byte(pw+"\n"), 0o600); err != nil {
		return "", err
	}
	return pw, nil
}

// User is an account created by CreateUser.
type User struct {
	ID           int
	Username     string
	HTTPPassword string
	// Signer and PrivateKeyPEM are the account's SSH key.
	Signer        ssh.Signer
	PrivateKeyPEM []byte
}

// CreateUser creates an account with an SSH key and HTTP password, in the
// given groups (which must exist).
func (s *Server) CreateUser(ctx context.Context, username, fullName string, groups ...string) (*User, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, username)
	if err != nil {
		return nil, err
	}
	pw := randomPassword()
	in := map[string]any{
		"name":          fullName,
		"email":         username + "@example.com",
		"ssh_key":       strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		"http_password": pw,
		"groups":        groups,
	}
	var out struct {
		ID int `json:"_account_id"`
	}
	if err := s.Admin.Do(ctx, http.MethodPut, "accounts/"+url.PathEscape(username), in, &out); err != nil {
		return nil, err
	}
	return &User{ID: out.ID, Username: username, HTTPPassword: pw, Signer: signer, PrivateKeyPEM: pem.EncodeToMemory(block)}, nil
}

// CreateGroup creates a group owned by Administrators and visible to all
// users.
func (s *Server) CreateGroup(ctx context.Context, name string) error {
	return s.Admin.Do(ctx, http.MethodPut, "groups/"+url.PathEscape(name),
		map[string]any{"owner": "Administrators", "visible_to_all": true}, nil)
}

// CreateProject creates a project with an initial empty commit on main.
func (s *Server) CreateProject(ctx context.Context, name string) error {
	return s.Admin.Do(ctx, http.MethodPut, "projects/"+url.PathEscape(name),
		map[string]any{"create_empty_commit": true, "branches": []string{"main"}}, nil)
}

// KnownHostsLine returns a known_hosts line for the server's SSH host key.
func (s *Server) KnownHostsLine() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Site, "etc", "ssh_host_ed25519_key.pub"))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return "", errors.New("malformed host key")
	}
	host, port, _ := net.SplitHostPort(s.SSHAddr)
	return fmt.Sprintf("[%s]:%s %s %s", host, port, f[0], f[1]), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomPassword() string {
	b := make([]byte, 18)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// installReplication copies the replication plugin out of the war and
// writes its configuration. Branches are force-pushed: the Gerrit site owns
// them, as in production (design §3).
// aosPlugins are the war-bundled plugins AOS runs (besides the github
// plugin and replication, which are installed separately).
var aosPlugins = []string{
	"replication-api.jar", "codemirror-editor.jar", "commit-message-length-validator.jar",
	"delete-project.jar", "download-commands.jar", "gitiles.jar", "hooks.jar",
	"plugin-manager.jar", "reviewnotes.jar", "singleusergroup.jar", "webhooks.jar",
}

// installWarPlugins copies plugins bundled in the war into site/plugins.
func installWarPlugins(war, site string, names ...string) error {
	zr, err := zip.OpenReader(war)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(filepath.Join(site, "plugins"), 0o755); err != nil {
		return err
	}
	for _, name := range names {
		var jar *zip.File
		for _, f := range zr.File {
			if f.Name == "WEB-INF/plugins/"+name {
				jar = f
			}
		}
		if jar == nil {
			return fmt.Errorf("no %s in the war", name)
		}
		in, err := jar.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(site, "plugins", name))
		if err == nil {
			_, err = io.Copy(out, in)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func installReplication(war, site, home string, r *Replication) error {
	// replication-api is the API plugin (Gerrit 3.9+) other plugins, such
	// as the github plugin, bind to.
	if err := installWarPlugins(war, site, "replication.jar", "replication-api.jar"); err != nil {
		return err
	}

	cfg := filepath.Join(site, "etc", "replication.config")
	os.Remove(cfg)
	set := func(file string, kv ...string) error {
		for i := 0; i < len(kv); i += 2 {
			args := []string{"config", "-f", file}
			if strings.HasSuffix(kv[i], "+") {
				args = append(args, "--add", strings.TrimSuffix(kv[i], "+"), kv[i+1])
			} else {
				args = append(args, kv[i], kv[i+1])
			}
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
		return nil
	}
	// As on AOS: the default replicationDelay (15s), which is how long
	// the bridge has to retarget PRs after a submit in Gerrit (design §6.7).
	rem := "remote." + r.Name + "."
	kv := []string{
		"gerrit.replicateOnStartup", "true",
		"gerrit.autoReload", "true",
		"replication.lockErrorMaxRetries", "5",
		rem + "url", r.URL,
		rem + "push", "refs/heads/*:refs/heads/*",
		rem + "push+", "refs/tags/*:refs/tags/*",
		rem + "force", "true",
		rem + "rescheduleDelay", "15",
		rem + "threads", "1",
		rem + "createMissingRepositories", "false",
	}
	for _, p := range r.Projects {
		kv = append(kv, rem+"projects+", p)
	}
	if err := set(cfg, kv...); err != nil {
		return err
	}
	secure := filepath.Join(site, "etc", "secure.config")
	if r.SSHKey != "" {
		// Gerrit's SSH client (JGit) reads the key and host keys from
		// $HOME/.ssh, and the remote needs no password.
		exec.Command("git", "config", "-f", secure, "--remove-section", "remote."+r.Name).Run()
		key, err := os.ReadFile(r.SSHKey)
		if err != nil {
			return err
		}
		dir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), key, 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(r.KnownHosts), 0o644)
	}
	if err := set(secure, rem+"username", r.User, rem+"password", r.Password); err != nil {
		return err
	}
	return os.Chmod(secure, 0o600)
}

// SetupCI creates a CI account like gerrit-buildkite's: in Service Users
// (stream-events) and a "CI" group that may vote Verified on project.
func (s *Server) SetupCI(ctx context.Context, project, username string) (*User, error) {
	if err := s.CreateGroup(ctx, "CI"); err != nil {
		return nil, err
	}
	u, err := s.CreateUser(ctx, username, "Buildkite", "CI", "Service Users")
	if err != nil {
		return nil, err
	}
	// Voting Verified comes from Service Users (ConfigureProject), as on
	// AOS; the CI group just names the account's role.
	return u, nil
}
