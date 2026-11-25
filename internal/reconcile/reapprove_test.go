package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// reapproveEnv: alice's Gerrit +2 on patch set 2 mirrored as her GitHub
// approval, where GitHub owns the branches; then patch set 3 (a rebase,
// Gerrit copying her vote) pushed, and GitHub dismissing the approval
// on that push as dismissal (by, commit).
func reapproveEnv(t *testing.T, by, commit string, vote int) (*commentsEnv, int64) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if len(e.h.reviews) != 1 || e.h.reviews[0].State != "APPROVED" {
		t.Fatalf("setup: %+v", e.h.reviews)
	}
	first := e.h.reviews[0].ID
	early := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := updateStateFn(context.Background(), e.st, 5, "ps3", func(s *state.Change) {
		s.Revisions = append(s.Revisions, state.Revision{PatchSet: 3, GerritSHA: "sha3", GitHubSHA: "sha3", At: early})
		s.Heads = append(s.Heads, state.Head{SHA: "sha3", At: early})
	}); err != nil {
		t.Fatal(err)
	}
	e.g.revs["sha3"], e.g.current, e.h.head = 3, "sha3", "sha3"
	e.g.vote("alice", "Code-Review", vote)
	e.h.dismiss(first, by, e.g.now())
	d := e.h.dismissals[first]
	d.Commit = commit
	e.h.dismissals[first] = d
	return e, first
}

func approvals(e *commentsEnv, commit string) int {
	n := 0
	for _, rv := range e.h.reviews {
		if rv.State == "APPROVED" && rv.User == aliceGH && rv.CommitID == commit {
			n++
		}
	}
	return n
}

// GitHub dismissed alice's approval on the push of a patch set Gerrit
// copied her +2 to: approved again, as her, on its commit, once.
func TestGitHubOwnedKeptVoteIsApprovedAgain(t *testing.T) {
	e, _ := reapproveEnv(t, "pusher", "sha3", 2)
	e.sync(t)
	if got := approvals(e, "sha3"); got != 1 {
		t.Fatalf("approvals of sha3 = %d; reviews %+v", got, e.h.reviews)
	}
	e.settled(t)
	if got := approvals(e, "sha3"); got != 1 {
		t.Errorf("approved again twice: %+v", e.h.reviews)
	}
}

// Not when a person dismissed it, nor when Gerrit did not keep the vote.
func TestGitHubOwnedReapprovalOnlyForAKeptVoteAndAPush(t *testing.T) {
	for _, tc := range []struct {
		name, by, commit string
		vote             int
	}{
		{"a person's dismissal", "mallory", "", 2},
		{"a vote not kept", "pusher", "sha3", 0},
		{"a vote lowered", "pusher", "sha3", 1},
	} {
		e, _ := reapproveEnv(t, tc.by, tc.commit, tc.vote)
		e.sync(t)
		if got := approvals(e, "sha3"); got != 0 {
			t.Errorf("%s: approved again (%+v)", tc.name, e.h.reviews)
		}
	}
}

// Asked to review again since, alice is not approved again: a review as
// her would end that request.
func TestGitHubOwnedReapprovalLeavesAPendingRequest(t *testing.T) {
	e, _ := reapproveEnv(t, "pusher", "sha3", 2)
	e.h.requested = append(e.h.requested, aliceGH)
	e.sync(t)
	if got := approvals(e, "sha3"); got != 0 {
		t.Errorf("approved again over a pending request: %+v", e.h.reviews)
	}
}

