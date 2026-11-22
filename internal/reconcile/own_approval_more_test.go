package reconcile

import (
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

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
		found = found || strings.Contains(gc.Message, notCounted+" it approves its author's own change.")
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
