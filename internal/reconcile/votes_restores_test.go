package reconcile

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Round 20 (diff): a vote-only message GitHub refuses once with its
// voter's token is recorded as refused for good (51eab1b), whatever the
// refusal: a 401 is an expired or revoked token (the client invalidates
// it for a fresh one), a 403 can be a secondary rate limit. Where GitHub
// owns the branches no statuses show Gerrit votes (Checks.GitHubOwned),
// so alice's Code-Review -2 never reaches GitHub at all once her token
// works again: her veto is lost, and the PR can merge.
func TestVetoNotLostToOneTransientRefusal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		e, a := linkedEnv(t)
		e.c.GitHubOwned = true
		a.refuse = &github.APIError{Status: status, Message: "transient"}
		alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
		e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review-2", RevisionNumber: 2}}
		e.g.vote("alice", "Code-Review", -2)
		e.sync(t)
		a.refuse = nil // her token works again
		e.sync(t)
		e.sync(t)
		found := false
		for _, rv := range e.h.reviews {
			found = found || (rv.State == "CHANGES_REQUESTED" && rv.User == aliceGH)
		}
		if !found {
			t.Errorf("after one %d: alice's -2 never became her change request on GitHub (reviews %+v)", status, e.h.reviews)
		}
	}
}

func ownDismissed(e *commentsEnv) bool {
	return slices.ContainsFunc(e.h.dismissed, func(d string) bool { return strings.Contains(d, "owns this change") })
}

// Where GitHub owns the branches only bob's own later Gerrit vote lifts
// his GitHub change request (7db5300). One cast in his name by someone
// else (on_behalf_of: Gerrit's real_author) is not his: the bridge never
// makes such a vote his GitHub review (sixteenth review), and it must not
// dismiss his review either; on GitHub that takes "dismiss reviews".
func TestChangeRequestNotLiftedOnHisBehalf(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e := changeRequested(t, true, alice)
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.uploaders[3] = alice
	e.g.vote("bobg", "Code-Review", 0)
	e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "mb", Author: bob, RealAuthor: &carol, Date: e.g.tick(),
		Message: "Patch Set 3: -Code-Review", RevisionNumber: 3})
	e.sync(t)
	if len(e.h.dismissed) != 0 {
		t.Errorf("carol, as bob, dismissed his change request: %v", e.h.dismissed)
	}
}

// A vote-only message GitHub refuses with its voter's token posts
// nothing, so it spends nothing (eighteenth review's refund rule): 51eab1b's
// record-and-stop path returns "done" and keeps alice's PostBudget spent
// (kills V2, which refunds).
func TestRefusedVoteRefundsItsBudget(t *testing.T) {
	e, a := linkedEnv(t)
	e.c.PostBudget = &Budget{N: 2, Per: time.Hour}
	a.refuse = &github.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if left := e.c.PostBudget.Left(context.Background(), 1); left != 2 {
		t.Errorf("alice has %d posts left, want 2: a vote GitHub refused posted nothing", left)
	}
}

// threadWithBobsRoot mirrors bob's GitHub comment to Gerrit and returns
// its GitHub id and Gerrit copy's id.
func threadWithBobsRoot(t *testing.T, e *commentsEnv) (int64, string) {
	t.Helper()
	id := e.h.human("a.txt", 3, "sha2", "Is three right?", 0)
	e.sync(t)
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "Is three right?") {
			return id, gc.ID
		}
	}
	t.Fatal("bob's comment not mirrored")
	return 0, ""
}

func editBobs(e *commentsEnv, id int64) {
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == id {
			e.h.rcs[i].Body += " (typo fixed)"
		}
	}
	e.h.edited(id, "bob")
}

// Gerrit-owned (AOS), as on main: a resolution whose last Gerrit comment
// is the bridge's own post resolves as the bridge, never with the token
// of whoever wrote before it (kills S2: 01bf4b2's walk applied in both
// modes).
func TestGerritOwnedResolutionThroughBridgePostIsTheBridges(t *testing.T) {
	e, _ := linkedEnv(t)
	id, bobs := threadWithBobsRoot(t, e)
	e.g.user("c2", "a.txt", 2, 3, "Yes, done.", bobs, false)
	editBobs(e, id)
	e.sync(t)
	e.sync(t)
	if !e.h.resolved[id] {
		t.Fatalf("not resolved")
	}
	if e.h.resolvedBy[id] == aliceGH {
		t.Errorf("resolved with alice's token through the bridge's note; main resolves as the bridge")
	}
}

// GitHub-owned: alice reopens in Gerrit, bob edits his GitHub comment
// (the bridge's note, unresolved, is then last). Unresolving only holds
// the PR back: the bridge reopens the GitHub thread (kills S1: the walk
// applied to unresolving, which then finds nobody who "resolved" it).
func TestGitHubOwnedReopenThroughBridgePost(t *testing.T) {
	e, _ := linkedEnv(t)
	e.c.GitHubOwned = true
	id, bobs := threadWithBobsRoot(t, e)
	e.g.user("c2", "a.txt", 2, 3, "Yes, done.", bobs, false)
	e.sync(t)
	e.sync(t)
	if !e.h.resolved[id] {
		t.Fatalf("alice's resolution not mirrored")
	}
	e.g.user("c3", "a.txt", 2, 3, "No, not done.", "c2", true)
	editBobs(e, id)
	// The edit's note goes in first: the snapshot syncResolution reads
	// already has it last.
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "n1", Path: "a.txt", PatchSet: 2, Line: 3, InReplyTo: "c3", Tag: commentTag,
		Message: "**@bob**'s comment was edited on GitHub", Updated: e.g.tick(), Unresolved: true, Author: gerrit.AccountInfo{AccountID: bridgeID}})
	e.sync(t)
	e.sync(t)
	if e.h.resolved[id] {
		t.Errorf("alice reopened it in Gerrit; GitHub's thread stays resolved")
	}
}

// GitHub-owned: a bridge post that says "resolved" while the last person
// who wrote left it unresolved resolves nothing as them (kills S3).
func TestGitHubOwnedBridgePostResolvesNotAsAnUnresolvingWriter(t *testing.T) {
	e, _ := linkedEnv(t)
	e.c.GitHubOwned = true
	id, bobs := threadWithBobsRoot(t, e)
	e.g.user("c2", "a.txt", 2, 3, "Not yet.", bobs, true)
	e.sync(t)
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "n1", Path: "a.txt", PatchSet: 2, Line: 3, InReplyTo: "c2", Tag: commentTag,
		Message: "a bridge note", Updated: e.g.tick(), Unresolved: false, Author: gerrit.AccountInfo{AccountID: bridgeID}})
	e.sync(t)
	e.sync(t)
	if e.h.resolved[id] && e.h.resolvedBy[id] == aliceGH {
		t.Errorf("resolved with alice's token, though her last word left it unresolved")
	}
}

// GitHub-owned: bob's +1 in Gerrit BEFORE his GitHub change request does
// not lift it once a new patch set drops his -1 (kills Q6: votedSince
// from any time).
func TestChangeRequestNotLiftedByAnEarlierVote(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	e := newCommentsEnv(t)
	e.runAs()
	e.c.GitHubOwned = true
	e.g.owner = alice
	e.g.uploaders = map[int]gerrit.AccountInfo{1: alice, 2: alice, 3: alice}
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "mb", Author: bob, Date: e.g.tick(), Message: "Patch Set 2: Code-Review+1", RevisionNumber: 2})
	e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "CHANGES_REQUESTED", CommitID: "sha2", User: github.User{Login: "bob"},
		Body: "No.", SubmittedAt: e.g.now().Add(10 * time.Second), HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
	e.g.clock += 20
	e.sync(t)
	e.sync(t)
	if e.g.labels["Code-Review"]["bobg"] != -1 || len(e.h.dismissed) != 0 {
		t.Fatalf("votes %v, dismissed %v", e.g.labels, e.h.dismissed)
	}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.vote("bobg", "Code-Review", 0)
	e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
	e.sync(t)
	if len(e.h.dismissed) != 0 {
		t.Errorf("bob's change request dismissed by his own older +1: %v", e.h.dismissed)
	}
}
