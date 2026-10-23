package gitmirror

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

func TestParsePorcelain(t *testing.T) {
	out := "To /x/origin.git\n" +
		"*\tabc:refs/bridge/changes/01/1\t[new reference]\n" +
		"!\tdef:refs/bridge/changes/02/2\t[rejected] (stale info)\n" +
		"Done\n"
	want := []PushResult{
		{Ref: "refs/bridge/changes/01/1"},
		{Ref: "refs/bridge/changes/02/2", Rejected: true, Reason: "[rejected] (stale info)"},
	}
	if got := parsePorcelain([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestInitIdempotentAndRevParse(t *testing.T) {
	ctx := context.Background()
	r := &Repo{Dir: filepath.Join(t.TempDir(), "m.git"), Remote: "/a"}
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	os.Chmod(r.Dir, 0o755) // made before mirrors were private
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	r.Remote = "/b"
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	out, _ := r.Git(ctx, nil, "remote", "get-url", "origin")
	if strings.TrimSpace(string(out)) != "/b" {
		t.Errorf("origin = %q", out)
	}
	if out, _ := r.Git(ctx, nil, "config", "pack.useBitmaps"); strings.TrimSpace(string(out)) != "false" {
		t.Errorf("pack.useBitmaps = %q", out)
	}
	if out, _ := r.Git(ctx, nil, "config", "http.followRedirects"); strings.TrimSpace(string(out)) != "false" {
		t.Errorf("http.followRedirects = %q", out)
	}
	if fi, err := os.Stat(r.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mirror mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	if out, _ := r.Git(ctx, nil, "config", "fetch.writeCommitGraph"); strings.TrimSpace(string(out)) != "true" {
		t.Errorf("fetch.writeCommitGraph = %q", out)
	}
	if v, err := r.RevParse(ctx, "refs/heads/nope"); v != "" || err != nil {
		t.Errorf("RevParse missing = %q, %v", v, err)
	}
}

// A git command in a run's context is a span of its trace: its
// arguments, not its config or environment.
func TestGitTraces(t *testing.T) {
	tr := trace.New()
	ctx := trace.With(context.Background(), tr)
	r := &Repo{Dir: filepath.Join(t.TempDir(), "m.git"), Remote: "/a"}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Git(ctx, nil, "rev-parse", "--is-bare-repository")
	r.Git(ctx, nil, "rev-parse", "nonesuch")
	spans, _ := tr.Spans()
	if len(spans) != 2 || spans[0].What != "git rev-parse --is-bare-repository" || spans[0].Err != "" || spans[1].Err == "" {
		t.Errorf("spans %+v", spans)
	}
}

func TestHTTPCredentialsScopedToURL(t *testing.T) {
	ctx := context.Background()
	gCfg, gEnv := HTTPCredentials("https://gerrit.example.com", "bridge", "pa ss")
	hCfg, hEnv := HTTPCredentials("https://github.com", "x-access-token", "ghp_tok")
	r := &Repo{
		Dir: filepath.Join(t.TempDir(), "m.git"), Remote: "/a",
		Config: append(gCfg, hCfg...), Env: append(gEnv, hEnv...),
	}
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	fill := func(host string) string {
		out, err := r.Git(ctx, []byte("protocol=https\nhost="+host+"\n\n"), "credential", "fill")
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)
	}
	if out := fill("gerrit.example.com"); !strings.Contains(out, "username=bridge\n") || !strings.Contains(out, "password=pa ss\n") {
		t.Errorf("gerrit credential fill = %q", out)
	}
	if out := fill("github.com"); !strings.Contains(out, "username=x-access-token\n") || strings.Contains(out, "pa ss") {
		t.Errorf("github credential fill = %q", out)
	}
	// Unknown hosts get nothing (git fails rather than prompting).
	if out := fill("evil.example.com"); strings.Contains(out, "password=") {
		t.Errorf("credentials offered to another host: %q", out)
	}
	// A per-command env overrides the token, for refreshed tokens.
	out, _ := r.GitEnv(ctx, CredentialEnv("https://github.com", "x-access-token", "new"),
		[]byte("protocol=https\nhost=github.com\n\n"), "credential", "fill")
	if !strings.Contains(string(out), "password=new\n") {
		t.Errorf("override = %q", out)
	}
	for _, c := range r.Config {
		if strings.Contains(c, "pa ss") || strings.Contains(c, "ghp_tok") {
			t.Error("secret leaked into -c config")
		}
	}
}

// A deadline stops git's children too, and a fetch killed by it leaves no
// .keep file to hold its pack for good (eighth audit: index-pack, fed a
// small pack that inflates to gigabytes, outlived the importer's fetch
// deadline by as long as it liked, with the branch locked meanwhile). The
// alias stands in for fetch-pack: it marks a pack as fetch-pack does, and
// its child, as index-pack does, holds git's output open.
func TestDeadlineStopsGitsChildren(t *testing.T) {
	r := &Repo{Dir: filepath.Join(t.TempDir(), "m.git"), Remote: "/none"}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	keep := filepath.Join(r.Dir, "objects", "pack", "pack-x.keep")
	other := filepath.Join(r.Dir, "objects", "pack", "pack-y.keep")
	if err := os.WriteFile(other, []byte("fetch-pack 1 on elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := r.GitEnv(ctx, nil, nil, "-c", `alias.slow=!echo "fetch-pack $PPID on here" > "$GIT_DIR/objects/pack/pack-x.keep"; sleep 30`, "slow")
	if took := time.Since(start); err == nil || took > 2*time.Second {
		t.Errorf("returned after %v (err %v): the deadline was 300ms", took, err)
	}
	if _, err := os.Stat(keep); err == nil {
		t.Error("the killed fetch's .keep file is still there")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another fetch's .keep file: %v", err)
	}
}
