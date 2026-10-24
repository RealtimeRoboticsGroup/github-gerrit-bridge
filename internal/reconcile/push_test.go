package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

func TestSuexecRefusesNamesGerritReadsAsOtherAccounts(t *testing.T) {
	// Nothing is pushed: the name is refused before git runs (Mirror is
	// nil and would panic).
	u := SuexecUploader{Address: "gerrit:29418", Project: "p"}
	for _, user := range []string{"1000000", "self", "me", "a b", "-x", "a;b"} {
		if err := u.Upload(context.Background(), user, "abc", "main", nil); err == nil {
			t.Errorf("uploaded as %q", user)
		}
	}
}

// Git runs GIT_SSH_COMMAND through the shell.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"/a b/key": `'/a b/key'`, "it's": `'it'\''s'`, "$(x)": `'$(x)'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// A branch name with a % in it would bring its own push options to an
// upload (submit, base=...), which Gerrit takes from any branch so named
// (eighth audit); one git would read as something else is refused too.
func TestRefsForRefusesOptionsInTheBranch(t *testing.T) {
	if r, err := refsFor("main", []string{"wip"}); err != nil || r != "refs/for/main%wip" {
		t.Errorf("refsFor(main) = %q, %v", r, err)
	}
	if r, err := refsFor("release/1.0", nil); err != nil || r != "refs/for/release/1.0" {
		t.Errorf("refsFor(release/1.0) = %q, %v", r, err)
	}
	for _, target := range []string{"main%submit", "x:y", "-main", "", "a b", "a^", "a~1", "a*"} {
		if r, err := refsFor(target, nil); err == nil {
			t.Errorf("refsFor(%q) = %q, want refused", target, r)
		}
	}
}

// An upload Gerrit takes is a write in the run's trace; one it refuses,
// or one with no new changes, is not.
func TestUploadsAreTraced(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	m := s.mirror(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	if _, err := m.Git(ctx, nil, "fetch", "-q", "origin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	c1 := s.commit(m0, "f.txt", "f\n", "One")
	s.push("gerrit.git", c1, "refs/heads/staging") // so the mirror can fetch it
	if _, err := m.Git(ctx, nil, "fetch", "-q", "origin", "refs/heads/staging"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(s.dir, "gerrit.git", "hooks", "pre-receive")
	tr := trace.New()
	tctx := trace.With(ctx, tr)
	os.WriteFile(hook, []byte("#!/bin/sh\necho 'no new changes' >&2; exit 1\n"), 0o755)
	if err := (HTTPUploader{Mirror: m}).Upload(tctx, "", c1, "main", nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(hook, []byte("#!/bin/sh\necho 'prohibited by Gerrit' >&2; exit 1\n"), 0o755)
	if err := (HTTPUploader{Mirror: m}).Upload(tctx, "", c1, "main", nil); err == nil {
		t.Fatal("refused upload: no error")
	}
	if w, n := tr.Writes(); n != 0 {
		t.Fatalf("an upload with no new changes, and a refused one, traced as writes: %q", w)
	}
	os.Remove(hook)
	if err := (HTTPUploader{Mirror: m}).Upload(tctx, "", c1, "main", nil); err != nil {
		t.Fatal(err)
	}
	if w, n := tr.Writes(); n != 1 || w[0] != "gerrit upload "+c1+" to refs/for/main" {
		t.Errorf("writes %q", w)
	}
}
