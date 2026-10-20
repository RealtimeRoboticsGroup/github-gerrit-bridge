package reconcile

import (
	"context"
	"testing"
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
