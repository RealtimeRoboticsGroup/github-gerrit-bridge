package reconcile

import (
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Where GitHub owns the branches, a change request is the gate, and only
// its reviewer (or whoever may dismiss reviews) lifts it. Gerrit does not
// copy a -1 to a new patch set, the author's or a stranger's withheld
// one: the bridge reading "no vote" then must not dismiss it (nineteenth
// review). The reviewer's own later vote of 0 or more in Gerrit does.
func TestGitHubOwnedChangeRequestSurvivesANewPatchSet(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	mallory := gerrit.AccountInfo{AccountID: 13, Name: "Mallory", Username: "mallory"}
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	for _, tc := range []struct {
		name      string
		uploader  gerrit.AccountInfo
		onGitHub  bool
		lift      string // bob's own later Gerrit message
		dismissed bool
	}{
		{"alice's push", alice, true, "", false},
		{"a stranger's withheld upload", mallory, false, "", false},
		{"bob's +1 since", alice, true, "Patch Set 3: Code-Review+1", true},
		{"bob's vote removed by himself", alice, true, "Patch Set 3: -Code-Review", true},
		{"bob's -2 since", alice, true, "Patch Set 3: Code-Review-2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := changeRequested(t, true, alice)
			e.g.revs["sha3"] = 3
			e.g.current = "sha3"
			e.g.uploaders[3] = tc.uploader
			e.g.vote("bobg", "Code-Review", 0)
			if tc.onGitHub {
				e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
			}
			if tc.lift != "" {
				e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "mb", Author: bob, Date: e.g.tick(), Message: tc.lift, RevisionNumber: 3})
			}
			e.sync(t)
			if got := len(e.h.dismissed) != 0; got != tc.dismissed {
				t.Errorf("dismissed %v, want %v", e.h.dismissed, tc.dismissed)
			}
		})
	}
}

func changeRequested(t *testing.T, githubOwned bool, owner gerrit.AccountInfo) *commentsEnv {
	t.Helper()
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = githubOwned
	e.g.owner = owner
	e.g.uploaders = map[int]gerrit.AccountInfo{1: owner, 2: owner}
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "No.", HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.sync(t)
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != -1 || len(e.h.dismissed) != 0 {
		t.Fatalf("votes %v, dismissed %v; want bob's -1 cast and his change request standing", e.g.labels, e.h.dismissed)
	}
	return e
}
