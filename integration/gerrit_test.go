// Package integration runs the bridge's Gerrit-facing pieces against a real
// Gerrit (internal/testgerrit), checking the assumptions the design makes
// about Gerrit 3.14: stream-events access, refs/bridge/* ACLs and
// compare-and-swap pushes, and group membership over SSH.
package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/testgerrit"
)

const project = "bridge-test"

var (
	srv *testgerrit.Server
	env *testgerrit.Env
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		ctx := context.Background()
		tmp := os.Getenv("TEST_TMPDIR")
		if tmp == "" {
			tmp, _ = os.MkdirTemp("", "integration")
		}
		log, _ := os.Create(filepath.Join(tmp, "gerrit.log"))
		var err error
		srv, err = testgerrit.Start(ctx, testgerrit.Options{Site: filepath.Join(tmp, "site"), Log: log})
		if err != nil {
			fmt.Fprintln(os.Stderr, "starting gerrit:", err)
			return 1
		}
		defer srv.Stop()
		if env, err = srv.SetupBridge(ctx, project); err != nil {
			fmt.Fprintln(os.Stderr, "setting up:", err)
			return 1
		}
		return m.Run()
	}())
}

func sshConfig(t *testing.T, u *testgerrit.User) gerritssh.Config {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	line, err := srv.KnownHostsLine()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(kh, []byte(line+"\n"), 0o600)
	cb, algos, err := gerritssh.KnownHosts(kh, srv.SSHAddr)
	if err != nil {
		t.Fatal(err)
	}
	return gerritssh.Config{
		Address: srv.SSHAddr, User: u.Username, Signer: u.Signer,
		HostKeyCallback: cb, HostKeyAlgorithms: algos,
		KeepaliveInterval: 5 * time.Second, KeepaliveCountMax: 3,
	}
}

func mirror(t *testing.T, u *testgerrit.User) *gitmirror.Repo {
	t.Helper()
	cfg, envv := gitmirror.HTTPCredentials(srv.URL, u.Username, u.HTTPPassword)
	r := &gitmirror.Repo{
		Dir:    filepath.Join(t.TempDir(), "mirror.git"),
		Remote: srv.URL + "/a/" + project,
		Config: cfg, Env: envv,
		Name: "GitHub Bridge", Email: u.Username + "@example.com",
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// pushChange uploads a new change adding file, as u.
func pushChange(t *testing.T, u *testgerrit.User, file string) {
	t.Helper()
	dir := t.TempDir()
	cfg, envv := gitmirror.HTTPCredentials(srv.URL, u.Username, u.HTTPPassword)
	run := func(args ...string) string {
		var full []string
		for _, c := range cfg {
			full = append(full, "-c", c)
		}
		cmd := exec.Command("git", append(full, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), envv...)
		cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME="+u.Username, "GIT_AUTHOR_EMAIL="+u.Username+"@example.com",
			"GIT_COMMITTER_NAME="+u.Username, "GIT_COMMITTER_EMAIL="+u.Username+"@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("fetch", "-q", srv.URL+"/a/"+project, "refs/heads/main")
	run("checkout", "-q", "FETCH_HEAD")
	os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644)
	run("add", file)
	// A fixed Change-Id per file keeps the test independent of the hook.
	run("commit", "-q", "-m", fmt.Sprintf("Add %s\n\nChange-Id: I%x", file, sha1.Sum([]byte(file))))
	run("push", "-q", srv.URL+"/a/"+project, "HEAD:refs/for/main")
}

type handler struct {
	mu     sync.Mutex
	events []*events.Event
	resync int
	got    chan struct{}
}

func (h *handler) Event(e *events.Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
	select {
	case h.got <- struct{}{}:
	default:
	}
}

func (h *handler) Resync(string) {
	h.mu.Lock()
	h.resync++
	h.mu.Unlock()
	select {
	case h.got <- struct{}{}:
	default:
	}
}

func TestStreamEvents(t *testing.T) {
	h := &handler{got: make(chan struct{}, 1)}
	s := &events.Stream{Source: events.SSHSource(sshConfig(t, env.Bridge), events.Types), Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, h, func() bool { return h.resync > 0 })
	pushChange(t, env.Alice, "stream.txt")
	var pc *events.Event
	waitFor(t, h, func() bool {
		for _, e := range h.events {
			if e.Type == "patchset-created" {
				pc = e
				return true
			}
		}
		return false
	})
	if pc.Project() != project || pc.Change.Number == 0 || pc.PatchSet.Number != 1 {
		t.Errorf("patchset-created = %+v / %+v", pc.Change, pc.PatchSet)
	}
	if a := pc.Actor(); a == nil || a.Username != "alice" {
		t.Errorf("uploader = %+v, want alice", a)
	}
}

func waitFor(t *testing.T, h *handler, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		h.mu.Lock()
		ok := cond()
		h.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-h.got:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out")
		}
	}
}

func TestStateStoreOnGerrit(t *testing.T) {
	ctx := context.Background()
	a := &state.Store{Repo: mirror(t, env.Bridge)}
	b := &state.Store{Repo: mirror(t, env.Bridge)}

	c := &state.Change{State: state.ChangeState{ChangeID: "Iabc", Branch: "main", Origin: "gerrit", PR: 4}}
	if err := a.Save(ctx, 42, c, "Link PR #4"); err != nil {
		t.Fatalf("bridge cannot create its state ref: %v", err)
	}
	c.State.Stack = 6
	if err := a.Save(ctx, 42, c, "Stack #6"); err != nil {
		t.Fatalf("bridge cannot update its state ref: %v", err)
	}

	// A second writer with a stale view is refused by Gerrit (the lease).
	stale := &state.Change{State: state.ChangeState{ChangeID: "Iabc", PR: 9}}
	if err := b.Save(ctx, 42, stale, "stale"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale create: err = %v, want ErrConflict", err)
	}
	if err := b.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := b.Load(ctx, 42)
	if err != nil || got.State.Stack != 6 || b.ChangeForPR(4) != 42 {
		t.Fatalf("after fetch: %+v, %v, index %d", got, err, b.ChangeForPR(4))
	}
}

func TestBridgeRefsHiddenFromUsers(t *testing.T) {
	ctx := context.Background()
	a := &state.Store{Repo: mirror(t, env.Bridge)}
	if err := a.Save(ctx, 7, &state.Change{State: state.ChangeState{ChangeID: "I7"}}, "x"); err != nil {
		t.Fatal(err)
	}
	alice := mirror(t, env.Alice)
	out, err := alice.Git(ctx, nil, "ls-remote", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "refs/bridge/") {
		t.Errorf("alice can see refs/bridge/*:\n%s", out)
	}
	if !strings.Contains(string(out), "refs/heads/main") {
		t.Errorf("ls-remote looks wrong:\n%s", out)
	}
	// Nor can she write there.
	sa := &state.Store{Repo: alice}
	if err := sa.Save(ctx, 8, &state.Change{State: state.ChangeState{ChangeID: "I8"}}, "x"); err == nil {
		t.Error("alice wrote refs/bridge/changes/08/8")
	}
}

func TestTrustedGroupMembers(t *testing.T) {
	c, err := gerritssh.Dial(context.Background(), sshConfig(t, env.Bridge))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Run(context.Background(), `gerrit ls-members --recursive "`+testgerrit.TrustedGroup+`"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "alice") || strings.Contains(string(out), "github-bridge") {
		t.Errorf("ls-members:\n%s", out)
	}
}

// TestSuexecPushOwnership checks design §5.2: the bridge pushes a GitHub
// import through `suexec --as <user>` with a peer key, and the change is
// owned and uploaded by that user, not the bridge.
func TestSuexecPushOwnership(t *testing.T) {
	ctx := context.Background()
	peer := &testgerrit.User{Username: "peer"}
	var err error
	peer.Signer, peer.PrivateKeyPEM, err = newKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.AddPeerKey(peer.Signer.PublicKey(), "github-bridge peer"); err != nil {
		t.Fatal(err)
	}
	cfg := sshConfig(t, peer)
	cfg.User = "Gerrit Code Review"
	// Gerrit notices the new peer_keys file by its modification time.
	var c *gerritssh.Client
	for deadline := time.Now().Add(10 * time.Second); ; {
		if c, err = gerritssh.Dial(ctx, cfg); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("peer login: %v", err)
	}
	out, err := c.Run(ctx, "suexec --as bob -- gerrit version")
	c.Close()
	if err != nil {
		t.Fatalf("suexec: %v", err)
	}
	t.Logf("suexec gerrit version: %s", out)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "peer_key")
	os.WriteFile(keyFile, peer.PrivateKeyPEM, 0o600)
	kh := filepath.Join(dir, "known_hosts")
	line, _ := srv.KnownHostsLine()
	os.WriteFile(kh, []byte(line+"\n"), 0o600)

	repo := filepath.Join(dir, "repo")
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			// Gerrit requires the committer to be the uploader, i.e. the
			// suexec target, so the bridge commits as the user too.
			"GIT_AUTHOR_NAME=Bob Newcomer", "GIT_AUTHOR_EMAIL=bob@example.com",
			"GIT_COMMITTER_NAME=Bob Newcomer", "GIT_COMMITTER_EMAIL=bob@example.com",
			"GIT_SSH_COMMAND=ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=10 -o IdentitiesOnly=yes -o UserKnownHostsFile="+kh+" -i "+keyFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	os.MkdirAll(repo, 0o755)
	git("init", "-q")
	url := "ssh://Gerrit%20Code%20Review@" + srv.SSHAddr + "/" + project
	// The peer user itself can do nothing but suexec: a plain fetch as the
	// peer hangs rather than failing, so fetch as the user too.
	git("fetch", "-q", "--upload-pack=suexec --as bob -- git-upload-pack", url, "refs/heads/main")
	git("checkout", "-q", "FETCH_HEAD")
	os.WriteFile(filepath.Join(repo, "imported.txt"), []byte("from github\n"), 0o644)
	git("add", "imported.txt")
	git("commit", "-q", "-m", "Imported from GitHub\n\nChange-Id: I1111111111111111111111111111111111111111")
	git("push", "-q", "--receive-pack=suexec --as bob -- git-receive-pack", url, "HEAD:refs/for/main")

	var changes []struct {
		Owner struct {
			Username string `json:"username"`
		} `json:"owner"`
		Revisions map[string]struct {
			Uploader struct {
				Username string `json:"username"`
			} `json:"uploader"`
		} `json:"revisions"`
	}
	if err := srv.Admin.Do(ctx, "GET", "changes/?q=change:I1111111111111111111111111111111111111111&o=CURRENT_REVISION&o=DETAILED_ACCOUNTS", nil, &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("found %d changes", len(changes))
	}
	if changes[0].Owner.Username != "bob" {
		t.Errorf("owner = %q, want bob", changes[0].Owner.Username)
	}
	for _, r := range changes[0].Revisions {
		if r.Uploader.Username != "bob" {
			t.Errorf("uploader = %q, want bob", r.Uploader.Username)
		}
	}
}

func newKey() (ssh.Signer, []byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "peer")
	if err != nil {
		return nil, nil, err
	}
	return signer, pem.EncodeToMemory(block), nil
}
