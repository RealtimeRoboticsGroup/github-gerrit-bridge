package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Where GitHub owns the branches, a Gerrit-born change's PR opened as the
// bridge lets its owner approve it on GitHub, and GitHub counts that
// approval of their own change (eighteenth review): it casts no Gerrit
// vote and is dismissed. Who uploaded its patch sets since is ignored, as
// Gerrit's rule against self-approval reads it (the owner's call): bob's
// fix to alice's change, then his approval of it, stands.
func TestGitHubApprovalOfOwnChangeIsDismissed(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	for _, tc := range []struct {
		name      string
		owner     gerrit.AccountInfo
		uploaders map[int]gerrit.AccountInfo
		own       bool
	}{
		{"his own change", bob, map[int]gerrit.AccountInfo{1: bob, 2: bob}, true},
		{"his patch set on alice's change", alice, map[int]gerrit.AccountInfo{1: alice, 2: bob}, false},
		{"his change, alice's upload", bob, map[int]gerrit.AccountInfo{1: alice, 2: alice}, true},
		{"alice's code", alice, map[int]gerrit.AccountInfo{1: alice, 2: alice}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ownApproval(t, true, tc.owner, tc.uploaders)
			if got := len(e.h.dismissed) > 0 && strings.Contains(e.h.dismissed[0], "owns this change"); got != tc.own {
				t.Errorf("dismissed %v, want own %v", e.h.dismissed, tc.own)
			}
			if got := len(e.g.votes) == 0; got != tc.own {
				t.Errorf("Gerrit votes %v, want none %v", e.g.votes, tc.own)
			}
		})
	}
}

// Nor does an approval of alice's change become bob's own when he
// uploads a patch set to it later.
func TestGitHubApprovalStandsAfterItsAuthorsUpload(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	e := ownApproval(t, true, alice, map[int]gerrit.AccountInfo{1: alice, 2: alice})
	if len(e.h.dismissed) != 0 || len(e.g.votes) != 1 {
		t.Fatalf("bob's approval of alice's code: dismissed %v, votes %v", e.h.dismissed, e.g.votes)
	}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.uploaders[3] = bob
	e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
	e.sync(t)
	for _, d := range e.h.dismissed {
		if strings.Contains(d, "owns this change") {
			t.Errorf("after bob's upload, his approval of alice's change dismissed as his own: %v", e.h.dismissed)
		}
	}
}

func ownApproval(t *testing.T, githubOwned bool, owner gerrit.AccountInfo, uploaders map[int]gerrit.AccountInfo) *commentsEnv {
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = githubOwned
	e.g.owner = owner
	e.g.uploaders = uploaders
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	return e
}

// GitHub refusing to dismiss one (who may dismiss reviews is the
// repository's rule) is logged, not a failure that stops the change's
// sync on every pass (nineteenth review).
func TestRefusedOwnApprovalDismissalDoesNotStall(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.g.owner = bob
	e.g.uploaders = map[int]gerrit.AccountInfo{1: bob, 2: bob}
	e.h.refuseDismiss = &github.APIError{Status: 403}
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t) // fails the test on an error
	e.sync(t)
	said := 0
	for _, ic := range e.h.ics {
		if strings.Contains(ic.Body, "GitHub would not let the bridge dismiss it") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("said %d times on the PR, want once: %+v", said, e.h.ics)
	}
}
