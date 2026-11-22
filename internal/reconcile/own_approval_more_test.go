package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Nineteenth review: where GitHub owns the branches, a Gerrit patch set
// whose uploader may not push is never pushed, so the PR's head stays an
// older patch set's commit. Bob uploaded that one (patch set 2) on
// alice's change; alice's rework (patch set 3) is only in Gerrit. Bob's
// +2 of patch set 3 is his to give, but his GitHub approval of the PR's
// head approves his own code, and GitHub counts it: it is dismissed,
// whatever Gerrit's current patch set is.
func TestGitHubApprovalOfOwnHeadIsDismissedBehindGerrit(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.g.owner = alice
	e.g.uploaders = map[int]gerrit.AccountInfo{1: alice, 2: bob, 3: alice}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.vote("bobg", "Code-Review", 2) // his +2 of alice's patch set 3
	// The PR's head is still sha2, bob's upload.
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	if len(e.h.dismissed) == 0 {
		t.Errorf("bob's GitHub approval of the PR's head, his own upload, stands (dismissed %v)", e.h.dismissed)
	}
}

// The directory's username and Gerrit's may differ in case (LDAP uids
// are case-insensitive): bob's approval of his own change is his own
// either way, dismissed and no vote (nineteenth review: sameAccount's
// EqualFold was untested).
func TestGitHubApprovalOfOwnCodeAnyCase(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"bob": "BobG"}}
	e.c.GitHubOwned = true
	e.g.owner = bob
	e.g.uploaders = map[int]gerrit.AccountInfo{1: bob, 2: bob}
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	if len(e.h.dismissed) == 0 || len(e.g.votes) != 0 {
		t.Errorf("BobG's approval of bobg's change: dismissed %v, votes %v", e.h.dismissed, e.g.votes)
	}
}

// Its Gerrit copy says why it cast no vote (nineteenth review: the note
// was untested).
func TestGitHubApprovalOfOwnCodeSaysNotCounted(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	e := ownApproval(t, true, bob, map[int]gerrit.AccountInfo{1: bob, 2: bob})
	found := false
	for _, gc := range e.g.comments {
		found = found || strings.Contains(gc.Message, notCounted+" it approves code its author owns or uploaded.")
	}
	if !found {
		t.Errorf("no note in Gerrit: %+v", e.g.comments)
	}
}

// A comment posted on its author's behalf goes as the bridge, account 0:
// never with a link that happens to be recorded for account 0
// (nineteenth review: posterFor's account != 0 was untested).
func TestOnBehalfNeverPostsWithAccountZerosLink(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e, a := linkedEnv(t)
	a.linked[0] = aliceGH
	at := e.g.tick()
	e.g.messages = append(e.g.messages,
		gerrit.ChangeMessage{ID: "m1", Author: alice, RealAuthor: &carol, Date: at, Message: "Patch Set 2:\n\n(1 comment)", RevisionNumber: 2})
	e.g.comments = append(e.g.comments,
		gerrit.CommentInfo{ID: "cB", Path: gerrit.PatchSetLevel, PatchSet: 2, Message: "The words.", Updated: at, Author: alice, ChangeMessageID: "m1"})
	e.sync(t)
	for _, ic := range e.h.ics {
		if strings.Contains(ic.Body, "The words.") && ic.User == aliceGH {
			t.Errorf("carol's words on alice's behalf posted with a link: %+v", ic)
		}
	}
}

// One given on a commit of his own, the PR since moved on to alice's
// rework, still counts on GitHub (without "dismiss stale approvals"): it
// is dismissed too.
func TestGitHubApprovalOfOwnEarlierCommitIsDismissed(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.g.owner = alice
	e.g.uploaders = map[int]gerrit.AccountInfo{1: alice, 2: bob, 3: alice}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	if len(e.h.dismissed) == 0 || !strings.Contains(e.h.dismissed[0], "owns or uploaded this code") {
		t.Errorf("bob's approval of his own sha2 stands: dismissed %v", e.h.dismissed)
	}
}

// One given on alice's earlier commit counts for the head GitHub has
// since (without "dismiss stale approvals"): bob's own upload there,
// behind Gerrit's current patch set, makes it his own code's.
func TestGitHubApprovalCountingForOwnHeadIsDismissed(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.g.owner = alice
	e.g.uploaders = map[int]gerrit.AccountInfo{1: alice, 2: bob, 3: alice}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.vote("bobg", "Code-Review", 2)
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha1", User: github.User{Login: "bob"},
		HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	if len(e.h.dismissed) == 0 || !strings.Contains(e.h.dismissed[0], "owns or uploaded this code") {
		t.Errorf("bob's approval counting for his own head stands: dismissed %v", e.h.dismissed)
	}
}
