package gitmirror

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	r.Remote = "/b"
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	out, _ := r.Git(ctx, nil, "remote", "get-url", "origin")
	if strings.TrimSpace(string(out)) != "/b" {
		t.Errorf("origin = %q", out)
	}
	if out, _ := r.Git(ctx, nil, "config", "fetch.writeCommitGraph"); strings.TrimSpace(string(out)) != "true" {
		t.Errorf("fetch.writeCommitGraph = %q", out)
	}
	if v, err := r.RevParse(ctx, "refs/heads/nope"); v != "" || err != nil {
		t.Errorf("RevParse missing = %q, %v", v, err)
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
