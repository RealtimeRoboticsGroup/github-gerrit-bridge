// Package integration runs the bridge's Gerrit-facing pieces against a real
// Gerrit (internal/testgerrit), checking the assumptions the design makes
// about Gerrit 3.14: stream-events access, refs/bridge/* ACLs and
// compare-and-swap pushes, and group membership over SSH.
package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"crypto/rand"
	"crypto/sha1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/config"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/events"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit/gerritssh"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/reconcile"
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
	msg := fmt.Sprintf("Add %s\n\nChange-Id: I%x", file, sha1.Sum([]byte(file)))
	if strings.HasPrefix(file, "signed") {
		msg += "\nSigned-off-by: " + u.Username + " <" + u.Username + "@example.com>"
	}
	run("commit", "-q", "-m", msg)
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

func TestWriteBridgeConfigParses(t *testing.T) {
	path, err := srv.WriteBridgeConfig(filepath.Join(t.TempDir(), "bridge"), env, "AustinSchuh", "bridge-scratch", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gerrit.SSH.PeerKeyFile == "" || cfg.Identity.GitHubToGerrit["AustinSchuh"] != "alice" {
		t.Errorf("config = %+v", cfg)
	}
}

// TestAOSSubmitRequirements checks the project is set up like AOS: a commit
// without Signed-off-by is held back by the Signed-off-by requirement.
func TestAOSSubmitRequirements(t *testing.T) {
	ctx := context.Background()
	pushChange(t, env.Alice, "unsigned.txt")
	pushChange(t, env.Alice, "signed.txt")
	status := func(file string) map[string]string {
		var cs []struct {
			SRs []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"submit_requirements"`
		}
		q := fmt.Sprintf("changes/?q=change:I%x&o=SUBMIT_REQUIREMENTS", sha1.Sum([]byte(file)))
		if err := srv.Admin.Do(ctx, "GET", q, nil, &cs); err != nil || len(cs) != 1 {
			t.Fatalf("query %s: %v (%d)", file, err, len(cs))
		}
		m := map[string]string{}
		for _, r := range cs[0].SRs {
			m[r.Name] = r.Status
		}
		return m
	}
	if got := status("unsigned.txt"); got["Signed-off-by"] != "UNSATISFIED" || got["Verified"] != "UNSATISFIED" {
		t.Errorf("unsigned change requirements = %v", got)
	}
	if got := status("signed.txt"); got["Signed-off-by"] != "SATISFIED" {
		t.Errorf("signed change requirements = %v", got)
	}
}

// TestRunAsAndUsernameMatching checks design §7 on real Gerrit: a GitHub
// login resolves to the Gerrit account of the same username (the github
// plugin's convention), and the bridge account can post as that user.
func TestRunAsAndUsernameMatching(t *testing.T) {
	ctx := context.Background()
	bridge := &gerrit.Client{BaseURL: srv.URL, User: env.Bridge.Username, Password: env.Bridge.HTTPPassword}
	users := &reconcile.Users{Gerrit: bridge}
	if u, ok := users.GerritUser(ctx, "ALICE"); !ok || u != "alice" {
		t.Errorf("GerritUser(ALICE) = %q, %v", u, ok)
	}
	if _, ok := users.GerritUser(ctx, "nobody-here"); ok {
		t.Error("matched a login with no account")
	}
	pushChange(t, env.Bob, "runas.txt")
	id := fmt.Sprintf("I%x", sha1.Sum([]byte("runas.txt")))
	var cs []gerrit.ChangeInfo
	if err := srv.Admin.Do(ctx, "GET", "changes/?q=change:"+id, nil, &cs); err != nil || len(cs) != 1 {
		t.Fatalf("find change: %v %d", err, len(cs))
	}
	n := cs[0].Number
	in := gerrit.ReviewInput{Tag: "github-bridge:mirror", Labels: map[string]int{"Code-Review": 1},
		Comments: map[string][]gerrit.CommentInput{gerrit.PatchSetLevel: {{Message: "**@alice** (GitHub):\n\nretest"}}}}
	if err := bridge.As("alice").SetReview(ctx, n, "current", in); err != nil {
		t.Fatalf("Run-As review: %v", err)
	}
	comments, err := srv.Admin.ListComments(ctx, n)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments: %v %d", err, len(comments))
	}
	if c := comments[0]; c.Author.Username != "alice" || c.Tag != "github-bridge:mirror" {
		t.Errorf("comment by %q tag %q, want alice / github-bridge:mirror", c.Author.Username, c.Tag)
	}
	ch, err := srv.Admin.GetChange(ctx, n, "DETAILED_LABELS", "DETAILED_ACCOUNTS")
	if err != nil {
		t.Fatal(err)
	}
	var voted bool
	for _, a := range ch.Labels["Code-Review"].All {
		voted = voted || (a.Username == "alice" && a.Value == 1 && a.Tag == "github-bridge:mirror")
	}
	if !voted {
		t.Errorf("Code-Review votes = %+v, want alice +1 tagged as a mirror", ch.Labels["Code-Review"].All)
	}
}

// The audit's premise (design §7.1), on a real Gerrit: X-Gerrit-RunAs
// reads a numeric name as an account id, so a user whose username is
// another account's id would be acted as that account. The bridge never
// matches or acts as such names.
func TestNumericUsernamesAreAccountIDs(t *testing.T) {
	ctx := context.Background()
	// Gerrit will not create a user named after an existing account's id,
	// but nothing stops one named after an account that comes later (or
	// a GitHub login, through the github plugin).
	first, err := srv.CreateUser(ctx, "numeric-probe", "Probe")
	if err != nil {
		t.Fatal(err)
	}
	var probe gerrit.AccountInfo
	if err := srv.Admin.Do(ctx, "GET", "accounts/"+first.Username, nil, &probe); err != nil {
		t.Fatal(err)
	}
	victimID := probe.AccountID + 2
	name := strconv.Itoa(victimID)
	if _, err := srv.CreateUser(ctx, name, "Numeric Name"); err != nil { // id probe+1
		t.Fatal(err)
	}
	if _, err := srv.CreateUser(ctx, "numeric-victim", "Victim"); err != nil { // id probe+2
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/a/accounts/self", nil)
	req.SetBasicAuth(env.Bridge.Username, env.Bridge.HTTPPassword)
	req.Header.Set("X-Gerrit-RunAs", name)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"username":"numeric-victim"`) {
		t.Errorf("Run-As %q gave %s; the premise (it means account %d) does not hold", name, body, victimID)
	}
	bridge := &gerrit.Client{BaseURL: srv.URL, User: env.Bridge.Username, Password: env.Bridge.HTTPPassword}
	users := &reconcile.Users{Gerrit: bridge}
	if u, ok := users.GerritUser(ctx, name); ok {
		t.Errorf("GitHub login %s acts as %q", name, u)
	}
	if err := bridge.As(name).Do(ctx, "GET", "accounts/self", nil, nil); err == nil {
		t.Errorf("the client ran as %s", name)
	}
	if _, err := bridge.Registered(ctx, probe.AccountID); err != nil {
		t.Errorf("registered_on: %v", err)
	}
}

// The bridge's grants are idempotent and least-privilege: it is not in
// Service Users (whose members vote Verified), yet still streams events
// (TestStreamEvents) and runs as users.
func TestBridgeGrants(t *testing.T) {
	ctx := context.Background()
	if err := srv.GrantRunAs(ctx, project); err != nil {
		t.Fatalf("second GrantRunAs: %v", err)
	}
	// The bridge, though no administrator, can check its never-groups.
	bridge := &gerrit.Client{BaseURL: srv.URL, User: env.Bridge.Username, Password: env.Bridge.HTTPPassword}
	users := &reconcile.Users{Gerrit: bridge, NeverGroups: []string{"Service Users", "Administrators"}}
	if err := users.CheckGroups(ctx); err != nil {
		t.Errorf("the bridge cannot check its never-groups: %v", err)
	}
	var members []gerrit.AccountInfo
	if err := srv.Admin.Do(ctx, "GET", "groups/Service%20Users/members/", nil, &members); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Username == env.Bridge.Username {
			t.Error("the bridge is in Service Users")
		}
	}
}

// Submit is the Submitters group's, as on AOS, not every registered
// user's: with it, anyone could rebase a trusted user's change onto their
// own on that user's behalf.
func TestOnlySubmittersMaySubmit(t *testing.T) {
	ctx := context.Background()
	pushChange(t, env.Alice, "submitters.txt")
	id := fmt.Sprintf("I%x", sha1.Sum([]byte("submitters.txt")))
	var cs []gerrit.ChangeInfo
	if err := srv.Admin.Do(ctx, "GET", "changes/?q=change:"+id, nil, &cs); err != nil || len(cs) != 1 {
		t.Fatalf("find change: %v %d", err, len(cs))
	}
	bob := &gerrit.Client{BaseURL: srv.URL, User: env.Bob.Username, Password: env.Bob.HTTPPassword}
	actions, err := bob.RevisionActions(ctx, cs[0].Number, "current")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actions["rebase"]; ok {
		t.Error("bob (no submitter) may rebase alice's change")
	}
	err = bob.Do(ctx, "POST", fmt.Sprintf("changes/%d/rebase", cs[0].Number), map[string]any{"on_behalf_of_uploader": true, "allow_conflicts": true}, nil)
	if err == nil {
		t.Error("bob rebased alice's change on her behalf")
	}
}

// configLines is project.config's settings, one "key=value" each, sorted.
func configLines(t *testing.T, config string) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "project.config")
	os.WriteFile(f, []byte(config), 0o644)
	out, err := exec.Command("git", "config", "-f", f, "--list").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(lines)
	return lines
}

// editedLines is configLines of config after edits (git config arguments).
func editedLines(t *testing.T, config string, edits ...[][]string) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "project.config")
	os.WriteFile(f, []byte(config), 0o644)
	for _, es := range edits {
		for _, e := range es {
			if out, err := exec.Command("git", append([]string{"config", "-f", f}, e...)...).CombinedOutput(); err != nil {
				t.Fatalf("git config %v: %v: %s", e, err, out)
			}
		}
	}
	b, _ := os.ReadFile(f)
	return configLines(t, string(b))
}

// The test site's access rules are AOS's (testgerrit/aos, read from
// realtimeroboticsgroup.org) plus exactly the bridge's additions, the
// deployment checklist: nothing granted here that AOS would not have.
func TestAccessIsAOSsPlusTheBridges(t *testing.T) {
	ctx := context.Background()
	stored := func(project string) string {
		t.Helper()
		var content string
		path := "projects/" + url.PathEscape(project) + "/branches/" + url.PathEscape("refs/meta/config") + "/files/project.config/content"
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/a/"+path, nil)
		req.SetBasicAuth("admin", srv.Admin.Password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		dec, err := base64.StdEncoding.DecodeString(string(b))
		if err != nil {
			t.Fatalf("%s: %v (%s)", project, err, b)
		}
		content = string(dec)
		return content
	}
	for _, tc := range []struct {
		project, aos string
		edits         [][][]string
	}{
		{"All-Projects", testgerrit.AOSAllProjects, [][][]string{testgerrit.AllProjectsFor314, testgerrit.BridgeAllProjects}},
		{project, testgerrit.AOSProject, [][][]string{testgerrit.ProjectFor314, testgerrit.BridgeProject}},
	} {
		got, want := configLines(t, stored(tc.project)), editedLines(t, tc.aos, tc.edits...)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s's access differs from AOS's plus the bridge's:\n got: %q\nwant: %q", tc.project, got, want)
		}
	}
}

// Groups are as on AOS (hidden, Verified Users including Service Users),
// plus only what the bridge needs: its never_groups and CI's trusted group
// visible to all, so it can check them; the others it cannot see.
func TestGroupsAreAOSsPlusTheBridges(t *testing.T) {
	ctx := context.Background()
	for _, g := range testgerrit.AOSGroups {
		var info struct {
			Options struct {
				VisibleToAll bool `json:"visible_to_all"`
			} `json:"options"`
		}
		if err := srv.Admin.Do(ctx, http.MethodGet, "groups/"+url.PathEscape(g), nil, &info); err != nil {
			t.Fatal(err)
		}
		if want := slices.Contains(testgerrit.BridgeVisibleGroups, g); info.Options.VisibleToAll != want {
			t.Errorf("%s visible to all = %v, want %v", g, info.Options.VisibleToAll, want)
		}
	}
	var inc []gerrit.GroupInfo
	if err := srv.Admin.Do(ctx, http.MethodGet, "groups/"+url.PathEscape(testgerrit.TrustedGroup)+"/groups/", nil, &inc); err != nil || len(inc) != 1 || inc[0].Name != "Service Users" {
		t.Errorf("%s includes %+v (%v), want Service Users", testgerrit.TrustedGroup, inc, err)
	}
	bridge := &gerrit.Client{BaseURL: srv.URL, User: env.Bridge.Username, Password: env.Bridge.HTTPPassword}
	if _, err := bridge.GroupMembers(ctx, testgerrit.TrustedGroup); err != nil {
		t.Errorf("the bridge cannot read %s: %v", testgerrit.TrustedGroup, err)
	}
	if _, err := bridge.GroupMembers(ctx, "Sudo"); err == nil {
		t.Error("the bridge can read Sudo, which AOS hides")
	}
}
