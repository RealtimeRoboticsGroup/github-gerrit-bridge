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
	if v, err := r.RevParse(ctx, "refs/heads/nope"); v != "" || err != nil {
		t.Errorf("RevParse missing = %q, %v", v, err)
	}
}

func TestHTTPCredentialsHelper(t *testing.T) {
	ctx := context.Background()
	cfg, env := HTTPCredentials("bridge", "pa ss")
	r := &Repo{Dir: filepath.Join(t.TempDir(), "m.git"), Remote: "/a", Config: cfg, Env: env}
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := r.Git(ctx, []byte("protocol=https\nhost=gerrit.example.com\n\n"), "credential", "fill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "username=bridge\n") || !strings.Contains(string(out), "password=pa ss\n") {
		t.Errorf("credential fill = %q", out)
	}
	// The password must not be in the arguments (visible in ps).
	for _, c := range cfg {
		if strings.Contains(c, "pa ss") {
			t.Error("password leaked into -c config")
		}
	}
}
