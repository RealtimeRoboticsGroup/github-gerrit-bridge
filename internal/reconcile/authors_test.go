package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

var aliceGH = github.User{Login: "alice-gh", ID: 77}

// linkedEnv is a comments env where alice (Gerrit account 1) linked
// GitHub account alice-gh.
func linkedEnv(t *testing.T) (*commentsEnv, *fakeAuthors) {
	e := newCommentsEnv(t)
	e.c.BridgeLogin = "bridge"
	a := &fakeAuthors{f: e.h, linked: map[int]github.User{1: aliceGH}}
	e.c.Authors = a
	return e, a
}

func mapped(t *testing.T, e *commentsEnv) map[string]state.Comment {
	st, err := e.st.Load(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]state.Comment{}
	for _, c := range st.Comments {
		out[c.GerritID] = c
	}
	return out
}

// A linked user's Gerrit comments, replies and messages are posted on
// GitHub as them, without the header saying whose they are; nobody
// else's are, and none comes back to Gerrit.
func TestLinkedAuthorsPostAsThemselves(t *testing.T) {
	e, _ := linkedEnv(t)
	e.h.human("a.txt", 3, "sha2", "Is three right?", 0)
	e.sync(t) // bob's comment reaches Gerrit
	var bobs string
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "Is three right?") {
			bobs = gc.ID
		}
	}
	if bobs == "" {
		t.Fatal("bob's comment not mirrored")
	}
	e.g.user("c1", "a.txt", 2, 2, "Why two?", "", true)                    // a line comment: a review
	e.g.user("c2", gerrit.PatchSetLevel, 2, 0, "Overall fine.", "", false) // patch set level: a PR comment
	e.g.user("c3", "a.txt", 2, 3, "Yes.", bobs, false)                     // a reply
	e.g.user("c4", "a.txt", 1, 2, "Old patch set.", "", true)
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "c5", Path: gerrit.PatchSetLevel, PatchSet: 2, Message: "Carol's.",
		Updated: e.g.tick(), Author: gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}})
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m1", Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted"},
		Message: "Patch Set 2: Code-Review+1\n\nSplit it, please.", RevisionNumber: 2})
	e.h.diffAt["sha1/a.txt"] = map[int]bool{2: true}
	e.sync(t)

	m := mapped(t, e)
	body := func(id string) (string, github.User) {
		mc := m[id]
		for _, rc := range e.h.rcs {
			if rc.ID == mc.GitHubID && mc.Kind == kindReviewComment {
				return rc.Body, rc.User
			}
		}
		for _, ic := range e.h.ics {
			if ic.ID == mc.GitHubID && mc.Kind == kindIssueComment {
				return ic.Body, ic.User
			}
		}
		t.Fatalf("%s not on GitHub: %+v", id, mc)
		return "", github.User{}
	}
	for _, id := range []string{"c1", "c2", "c3", "c4", "m1"} {
		b, u := body(id)
		if u != aliceGH || m[id].As != aliceGH.ID {
			t.Errorf("%s posted as %+v (recorded as %d), want alice-gh", id, u, m[id].As)
		}
		if strings.Contains(b, "Alice Trusted") || strings.Contains(b, "([Gerrit](") || !strings.Contains(b, "<!-- bridge:gerrit/5/"+id+" -->") {
			t.Errorf("%s body:\n%s", id, b)
		}
	}
	if b, _ := body("c1"); !strings.HasPrefix(b, "Why two?") {
		t.Errorf("c1 body:\n%s", b)
	}
	if b, _ := body("c4"); !strings.HasPrefix(b, "Old patch set.") {
		t.Errorf("c4 body:\n%s", b)
	}
	if b, _ := body("m1"); !strings.HasPrefix(b, "**Code-Review +1**\n\nSplit it, please.") {
		t.Errorf("m1 body:\n%s", b)
	}
	if b, u := body("c5"); u.Login != "bridge" || m["c5"].As != 0 || !strings.Contains(b, "**Carol** ([Gerrit](") {
		t.Errorf("carol's, unlinked, posted as %+v:\n%s", u, b)
	}
	e.settled(t)
	for _, gc := range e.g.comments {
		if strings.Contains(gc.Message, "Why two?") && gc.ID != "c1" {
			t.Errorf("alice's comment came back to Gerrit: %+v", gc)
		}
	}
}

// GitHub refusing a linked user's token (the App not installed, say)
// posts as the bridge, with the header, and stops trying their token
// for the rest of the sync. Not being able to renew it waits.
func TestLinkedAuthorRefusedPostsAsTheBridge(t *testing.T) {
	e, a := linkedEnv(t)
	a.refuse = &github.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.g.user("c2", gerrit.PatchSetLevel, 2, 0, "Two.", "", false)
	e.sync(t)
	m := mapped(t, e)
	if len(e.h.ics) != 2 || a.calls != 1 {
		t.Fatalf("posted %d, user token tried %d times", len(e.h.ics), a.calls)
	}
	for _, ic := range e.h.ics {
		if ic.User.Login != "bridge" || !strings.Contains(ic.Body, "**Alice Trusted (alice)**") {
			t.Errorf("fallback: %+v", ic)
		}
	}
	if m["c1"].As != 0 || m["c2"].As != 0 {
		t.Errorf("recorded as alice's: %+v", m)
	}
	// Her vote, after that, is not tried as her either.
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m1", Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted"},
		Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2})
	e.g.user("c3", gerrit.PatchSetLevel, 2, 0, "Three.", "", false)
	e.sync(t)
	if len(e.h.reviews) != 0 || a.calls != 2 {
		t.Errorf("reviews %+v, user token tried %d times", e.h.reviews, a.calls)
	}

	e, a = linkedEnv(t)
	a.err = errors.New("GitHub is down")
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	if err := e.c.Sync(context.Background(), 5); err == nil || len(e.h.ics) != 0 {
		t.Errorf("err %v, posted %+v; want it to wait", err, e.h.ics)
	}
}

// What a linked user's token posted is known for theirs even when the
// sync that posted it did not save: by its marker, in their comment, as
// they wrote it, naming their own Gerrit comment. Someone else's comment
// naming it is not its copy.
func TestLinkedPostsRecoveredWithoutState(t *testing.T) {
	e, _ := linkedEnv(t)
	e.g.user("c1", "a.txt", 2, 2, "One.", "", true)
	e.g.user("c2", gerrit.PatchSetLevel, 2, 0, "Two.", "", false)
	e.sync(t)
	ctx := context.Background()
	updateStateFn(ctx, e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.settled(t)
	m := mapped(t, e)
	if m["c1"].As != aliceGH.ID || m["c2"].As != aliceGH.ID || len(e.h.rcs) != 1 || len(e.h.ics) != 1 {
		t.Errorf("not rebuilt: %+v, %d review comments, %d PR comments", m, len(e.h.rcs), len(e.h.ics))
	}

	e, _ = linkedEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 1, Body: "Mine now <!-- bridge:gerrit/5/c1 -->", User: github.User{Login: "mallory", ID: 66}})
	e.sync(t)
	if m := mapped(t, e); m["c1"].GitHubID == 1 || m["c1"].As != aliceGH.ID {
		t.Errorf("mallory's comment taken for alice's copy: %+v", m["c1"])
	}
}

// A copy posted as its author is theirs to edit on GitHub; anyone else's
// edit is put back, as for the bridge's own copies.
func TestLinkedCopiesEditedOnGitHub(t *testing.T) {
	e, _ := linkedEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.sync(t)
	id := e.h.ics[0].ID
	e.h.ics[0].Body = "One, said better. <!-- bridge:gerrit/5/c1 -->"
	e.h.edited(id, "alice-gh")
	e.settled(t)
	if e.h.ics[0].Body != "One, said better. <!-- bridge:gerrit/5/c1 -->" {
		t.Errorf("author's own edit undone: %q", e.h.ics[0].Body)
	}
	e.h.ics[0].Body = "Defaced."
	e.h.edited(id, "mallory")
	e.sync(t)
	if b := e.h.ics[0].Body; !strings.HasPrefix(b, "<sub>(Restored by the bridge") || !strings.Contains(b, "One.") || strings.Contains(b, "Alice Trusted") {
		t.Errorf("not restored as alice's:\n%s", b)
	}
}

// A link to the bridge's own GitHub account posts nothing as it: its
// posts would read as nobody's.
func TestLinkToTheBridgesAccountIsIgnored(t *testing.T) {
	e, a := linkedEnv(t)
	a.linked[1] = github.User{Login: "Bridge", ID: 1}
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.sync(t)
	if len(e.h.ics) != 1 || a.calls != 0 || !strings.Contains(e.h.ics[0].Body, "**Alice Trusted (alice)**") {
		t.Errorf("calls %d, posted %+v", a.calls, e.h.ics)
	}
}

// A copy someone else edited is no proof: it is not taken back for
// alice's copy, and hers is posted again.
func TestLinkedCopyEditedByOthersIsNotRecovered(t *testing.T) {
	e, _ := linkedEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.sync(t)
	e.h.edited(e.h.ics[0].ID, "mallory")
	updateStateFn(context.Background(), e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.sync(t)
	if m := mapped(t, e); len(e.h.ics) != 2 || m["c1"].GitHubID != e.h.ics[1].ID {
		t.Errorf("mapped %+v; PR comments %+v", m["c1"], e.h.ics)
	}
}

// A linked user's Code-Review +2 or negative vote becomes their GitHub
// approval or change request, with or without words; a +1 stays a PR
// comment if it has words, and nobody else's vote becomes a review. The
// review is the bridge's post: never taken for a GitHub review to vote
// with in Gerrit.
func TestLinkedVotesBecomeReviews(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m2", Author: alice, Message: "Patch Set 1: Code-Review-1\n\nNot like this.", RevisionNumber: 1},
		{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
		{ID: "m3", Author: alice, Message: "Patch Set 2: Code-Review+1\n\nCloser.", RevisionNumber: 2},
		{ID: "m4", Author: carol, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
	}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if len(e.h.reviews) != 2 {
		t.Fatalf("reviews %+v", e.h.reviews)
	}
	byState := map[string]github.Review{}
	for _, rv := range e.h.reviews {
		byState[rv.State] = rv
		if rv.User != aliceGH {
			t.Errorf("review by %+v", rv.User)
		}
	}
	if rv := byState["APPROVED"]; rv.CommitID != "sha2" || !strings.Contains(rv.Body, "<!-- bridge:gerrit/5/m1 -->") {
		t.Errorf("approval %+v", rv)
	}
	if rv := byState["CHANGES_REQUESTED"]; rv.CommitID != "sha1" || !strings.Contains(rv.Body, "Not like this.") || !strings.Contains(rv.Body, "**Code-Review -1**") {
		t.Errorf("change request %+v", rv)
	}
	if len(e.h.ics) != 1 || !strings.Contains(e.h.ics[0].Body, "Closer.") || e.h.ics[0].User != aliceGH {
		t.Errorf("PR comments %+v", e.h.ics)
	}
	m := mapped(t, e)
	if m["m1"].Kind != kindReviewBody || m["m1"].As != aliceGH.ID || m["m4"].GitHubID != 0 {
		t.Errorf("mapped %+v", m)
	}
	votes := e.g.attempts
	e.settled(t)
	if e.g.attempts != votes {
		t.Errorf("alice's own review was mirrored back to Gerrit (%d SetReview calls)", e.g.attempts-votes)
	}
	// Found again by marker when the state is lost.
	updateStateFn(context.Background(), e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	e.settled(t)
	if m := mapped(t, e); m["m1"].As != aliceGH.ID || m["m2"].As != aliceGH.ID {
		t.Errorf("not rebuilt: %+v", m)
	}

	// Her vote withdrawn in Gerrit: the bridge dismisses the approval,
	// which withdraws nothing.
	e.g.vote("alice", "Code-Review", 0)
	e.sync(t)
	if len(e.h.dismissed) != 1 || !strings.HasPrefix(e.h.dismissed[0], fmt.Sprintf("%d: ", byState["APPROVED"].ID)) {
		t.Errorf("dismissed %v", e.h.dismissed)
	}
	// A person dismissing it on GitHub withdraws nothing either: the
	// vote was given in Gerrit.
	e.g.vote("alice", "Code-Review", 2)
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m5", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2, Date: "2026-10-04 00:30:00.000000000"})
	e.sync(t)
	var again int64
	for _, rv := range e.h.reviews {
		if rv.State == "APPROVED" {
			again = rv.ID
		}
	}
	e.h.dismiss(again, "mallory", time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC))
	e.sync(t)
	if e.g.labels["Code-Review"]["alice"] != 2 {
		t.Errorf("a person's dismissal of the bridge's approval withdrew alice's vote: %v", e.g.labels)
	}
}

// GitHub refusing the review (alice's own PR, say) leaves her words as a
// PR comment, as her, and a vote without words to the statuses.
func TestLinkedVoteGitHubRefuses(t *testing.T) {
	e, _ := linkedEnv(t)
	e.h.refuseVote = &github.APIError{Status: 422, Message: "Can not approve your own pull request"}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
		{ID: "m2", Author: alice, Message: "Patch Set 2: Code-Review-2\n\nNo.", RevisionNumber: 2},
	}
	e.sync(t)
	if len(e.h.reviews) != 0 || len(e.h.ics) != 1 || !strings.Contains(e.h.ics[0].Body, "No.") || e.h.ics[0].User != aliceGH {
		t.Errorf("reviews %+v, PR comments %+v", e.h.reviews, e.h.ics)
	}
	e.settled(t)
}

// Not every vote is one to post: the bridge's own (a GitHub approval cast
// in Gerrit as its reviewer, which would echo), CI's, other labels'. And
// an unlinked reviewer's vote costs none of their budget.
func TestLinkedVotesOnlyTheirOwnCodeReview(t *testing.T) {
	e, _ := linkedEnv(t)
	e.c.CIMessage = DefaultCIMessage
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e.c.PostBudget = &Budget{N: 1, Per: time.Hour}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Tag: mirrorTag, Message: "Patch Set 2: Code-Review+2\n\nApproved on GitHub.", RevisionNumber: 2},
		{ID: "m2", Author: alice, Message: "Patch Set 2: Verified-1", RevisionNumber: 2},
		{ID: "m3", Author: alice, Tag: "autogenerated:gerrit:newPatchSet", Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
		{ID: "m4", Author: carol, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
		{ID: "m5", Author: carol, Message: "Patch Set 2:\n\nCarol's words.", RevisionNumber: 2},
	}
	e.sync(t)
	if len(e.h.reviews) != 0 {
		t.Errorf("reviews %+v", e.h.reviews)
	}
	if len(e.h.ics) != 1 || !strings.Contains(e.h.ics[0].Body, "Carol's words.") {
		t.Errorf("PR comments %+v", e.h.ics)
	}
}

// A thread resolved in Gerrit by someone linked is resolved on GitHub as
// them; by the bridge if GitHub will not let them.
func TestLinkedResolution(t *testing.T) {
	for _, refused := range []bool{false, true} {
		e, _ := linkedEnv(t)
		if refused {
			e.h.refuseUserResolve = &github.GraphQLError{Messages: []string{"Resource not accessible by integration"}}
		}
		e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "c1", Path: "a.txt", PatchSet: 2, Line: 2, Message: "Please fix.", Updated: e.g.tick(),
			Unresolved: true, Author: gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}})
		e.sync(t)
		root := e.h.rcs[0].ID
		e.g.user("c2", "a.txt", 2, 2, "Done", "c1", false)
		e.sync(t)
		want := aliceGH
		if refused {
			want = github.User{Login: "bridge"}
		}
		if !e.h.resolved[root] || e.h.resolvedBy[root] != want {
			t.Errorf("refused %v: resolved %v by %+v", refused, e.h.resolved[root], e.h.resolvedBy[root])
		}
		e.settled(t)
	}
}
