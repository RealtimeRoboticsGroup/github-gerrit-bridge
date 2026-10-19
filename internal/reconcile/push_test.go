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
