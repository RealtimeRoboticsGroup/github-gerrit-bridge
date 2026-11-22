package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Where GitHub owns the branches, a Gerrit-born change's PR is the
// bridge's, so its owner may approve it on GitHub, and GitHub counts that
// approval of their own code (eighteenth review): it casts no Gerrit vote
// and is dismissed. So is one by whoever uploaded the head's code, on
// someone else's change; an approval of code that is not theirs stands.
func TestGitHubApprovalOfOwnCodeIsDismissed(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	for _, tc := range []struct {
		name      string
		owner     gerrit.AccountInfo
		uploaders map[int]gerrit.AccountInfo
		own       bool
	}{
		{"his own change", bob, map[int]gerrit.AccountInfo{1: bob, 2: bob}, true},
		{"his patch set on alice's change", alice, map[int]gerrit.AccountInfo{1: alice, 2: bob}, true},
		{"his change, alice's upload", bob, map[int]gerrit.AccountInfo{1: alice, 2: alice}, true},
		{"alice's code", alice, map[int]gerrit.AccountInfo{1: alice, 2: alice}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ownApproval(t, true, tc.owner, tc.uploaders)
			if got := len(e.h.dismissed) > 0 && strings.Contains(e.h.dismissed[0], "owns or uploaded this code"); got != tc.own {
				t.Errorf("dismissed %v, want own %v", e.h.dismissed, tc.own)
			}
			if got := len(e.g.votes) == 0; got != tc.own {
				t.Errorf("Gerrit votes %v, want none %v", e.g.votes, tc.own)
			}
		})
	}
}

// An approval made before its author uploaded the head's code becomes
// one of their own code, and is dismissed then.
func TestGitHubApprovalBecomesOwnCodeIsDismissed(t *testing.T) {
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
	if len(e.h.dismissed) != 1 || !strings.Contains(e.h.dismissed[0], "@bob owns or uploaded this code") {
		t.Errorf("after bob's upload: dismissed %v", e.h.dismissed)
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

// The bridge's restore of Gerrit's patch set over a GitHub push
// (restoreGerrit) is that patch set's code: an approval of it by whoever
// uploaded the restored one is of their own code (nineteenth review).
func TestGitHubApprovalOfRestoredOwnCodeIsDismissed(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	bridge := gerrit.AccountInfo{AccountID: bridgeID, Name: "GitHub Bridge"}
	for _, noted := range []bool{false, true} {
		e := newCommentsEnv(t)
		e.runAs()
		e.c.GitHubOwned = true
		e.g.owner = alice
		e.g.uploaders = map[int]gerrit.AccountInfo{1: alice, 2: bob, 3: bridge}
		e.g.revs["sha3"] = 3
		e.g.current = "sha3"
		e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
		if noted {
			e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "mc", Author: bridge, Tag: conflictTag, RevisionNumber: 3, Date: e.g.tick(),
				Message: "Both GitHub (PR #1) and Gerrit (patch set 2) changed this at the same time. The GitHub version was uploaded as a patch set, then Gerrit's version was restored on top"})
		}
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha3", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
		e.sync(t)
		e.sync(t)
		own := len(e.h.dismissed) > 0 && strings.Contains(e.h.dismissed[0], "owns or uploaded this code")
		if own != noted {
			t.Errorf("restore noted %v: dismissed %v", noted, e.h.dismissed)
		}
	}
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
}
