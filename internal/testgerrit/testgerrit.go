// Package testgerrit runs a throwaway local Gerrit for integration tests
// and manual testing, so the bridge never has to be pointed at a real
// server to be exercised.
//
// The site uses DEVELOPMENT_BECOME_ANY_ACCOUNT auth, so anyone who can
// reach it is an admin. It only ever listens on 127.0.0.1.
package testgerrit

import (
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
		URL:     "http://127.0.0.1:" + strconv.Itoa(o.HTTPPort),
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
		// Let people attach "Apply fix" suggestions to comments, which
		// the bridge maps to GitHub suggestions.
		{"experiments.enabled", "GerritBackendFeature__allow_fix_suggestions_in_comments"},
	} {
		c := exec.CommandContext(ctx, "git", "config", "-f", filepath.Join(o.Site, "etc", "gerrit.config"), kv[0], kv[1])
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git config %s: %w: %s", kv[0], err, out)
		}
	}

	s.cmd = java(context.Background(), "daemon", "-d", o.Site, "--console-log")
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if o.Log != nil {
		s.cmd.Stdout, s.cmd.Stderr = o.Log, o.Log
	}
	if err := s.cmd.Start(); err != nil {
		return nil, err
	}
	s.done = make(chan struct{})
	go func() {
		s.exitErr = s.cmd.Wait()
		close(s.done)
	}()

	if err := s.waitReady(ctx); err != nil {
		s.Stop()
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
