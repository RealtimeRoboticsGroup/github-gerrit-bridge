package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
		{ID: "m3", Author: alice, Message: "Patch Set 2: Code-Review+1\n\nCloser.", RevisionNumber: 2},
		{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
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
	// Asked once: the refused vote without words is not asked for again
	// on every pass, and the +2 she changed to -2 is not asked for at all
	// (seventeenth review: not her approval now).
	if e.h.voteTries != 1 {
		t.Errorf("asked GitHub for %d reviews, want 1", e.h.voteTries)
	}
}

// A vote on a patch set GitHub does not have (Gerrit is ahead of a fork's
// branch, or a stranger's patch set) is no review of another commit, which
// would be its voter's approval of code they never saw there (tenth
// audit): its words are a comment that says which patch set it is on.
func TestLinkedVoteOnPatchSetNotOnGitHub(t *testing.T) {
	e, _ := linkedEnv(t)
	e.h.prCommits = map[string]bool{"sha1": true, "sha2": true}
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Message: "Patch Set 3: Code-Review+2\n\nShip it.", RevisionNumber: 3},
		{ID: "m2", Author: alice, Message: "Patch Set 3: Code-Review-1", RevisionNumber: 3},
	}
	e.sync(t)
	if len(e.h.reviews) != 0 {
		t.Errorf("a vote on patch set 3 went on another commit as a review: %+v", e.h.reviews)
	}
	if len(e.h.ics) != 1 || e.h.ics[0].User != aliceGH || !strings.HasPrefix(e.h.ics[0].Body, "**Code-Review +2** on [patch set 3](") ||
		!strings.Contains(e.h.ics[0].Body, "not on GitHub") || !strings.Contains(e.h.ics[0].Body, "Ship it.") {
		t.Errorf("PR comments %+v", e.h.ics)
	}
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

// A Gerrit thread whose last comment is the bridge's copy of alice's
// GitHub reply, posted as her, is not resolved on GitHub with her token:
// she resolved nothing in Gerrit (tenth audit).
func TestBridgeCopyDoesNotResolveAsItsAuthor(t *testing.T) {
	e, _ := linkedEnv(t)
	e.g.maxVote = 2
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	e.c.GerritAs = func(user string) GerritAPI {
		return fakeAs{e.g, gerrit.AccountInfo{AccountID: 1, Username: user, Name: "Alice Trusted"}}
	}
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "c1", Path: "a.txt", PatchSet: 2, Line: 2, Message: "Please fix.", Updated: e.g.tick(),
		Unresolved: true, Author: gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}})
	e.sync(t)
	root := e.h.rcs[0].ID
	// On GitHub: dave resolves the thread, alice replies in it.
	e.h.resolved[root] = true
	e.h.resolvedBy = map[int64]github.User{root: {Login: "dave", ID: 5}}
	id := e.h.human("a.txt", 2, "sha2", "I agree.", root)
	for i := range e.h.rcs {
		if e.h.rcs[i].ID == id {
			e.h.rcs[i].User = aliceGH
		}
	}
	calls := 0
	e.g.onList = func() {
		calls++
		if calls == 2 { // erin unresolves between passes
			e.h.resolved[root] = false
			e.h.resolvedBy[root] = github.User{Login: "erin", ID: 6}
		}
	}
	e.sync(t)
	if e.h.resolvedBy[root] == aliceGH {
		t.Errorf("thread resolved on GitHub with alice's token; alice resolved nothing")
	}
}

// A resolution the bridge made on GitHub with alice's token is not taken
// for hers, to undo Carol's unresolving in Gerrit as alice, when it was
// not recorded because a later one failed (tenth audit).
func TestBridgeResolutionAsSomeoneIsKeptWhenALaterOneFails(t *testing.T) {
	e, _ := linkedEnv(t)
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	e.c.GerritAs = func(user string) GerritAPI {
		return fakeAs{e.g, gerrit.AccountInfo{AccountID: 1, Username: user, Name: "Alice Trusted"}}
	}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	for _, id := range []string{"c1", "d1"} {
		e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: id, Path: "a.txt", PatchSet: 2, Line: 2, Message: "Please fix.", Updated: e.g.tick(),
			Unresolved: true, Author: carol})
	}
	e.sync(t)
	roots := map[string]int64{}
	for _, rc := range e.h.rcs {
		for _, id := range []string{"c1", "d1"} {
			if strings.Contains(rc.Body, "bridge:gerrit/5/"+id+" ") {
				roots[id] = rc.ID
			}
		}
	}
	if len(roots) != 2 {
		t.Fatalf("roots %v", roots)
	}
	e.g.user("c2", "a.txt", 2, 2, "Done", "c1", false)
	e.g.user("d2", "a.txt", 2, 2, "Done", "d1", false)
	e.h.resolveErr = map[int64]error{roots["d1"]: errors.New("GitHub is down")}
	if err := e.c.Sync(context.Background(), 5); err == nil {
		t.Fatal("the failed resolution was not reported")
	}
	if e.h.resolvedBy[roots["c1"]] != aliceGH {
		t.Fatalf("setup: %+v", e.h.resolvedBy)
	}
	e.h.resolveErr = nil
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "c3", Path: "a.txt", PatchSet: 2, Line: 2, InReplyTo: "c2", Message: "Not done.", Updated: e.g.tick(),
		Unresolved: true, Author: carol})
	e.sync(t)
	for _, gc := range e.g.comments {
		if gc.Author.Username == "alice" && strings.Contains(gc.Message, "on GitHub") {
			t.Errorf("the bridge's resolution came back as alice's: %+v", gc)
		}
	}
	if e.h.resolved[roots["c1"]] {
		t.Error("Carol's unresolving was not mirrored")
	}
}

// A linked user's +2, posted as their GitHub approval, lowered to +1: the
// approval is dismissed, not left standing for a vote they no longer give
// (tenth audit).
func TestLoweredVoteDismissesTheApprovalItPosted(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2},
	}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if len(e.h.reviews) != 1 || e.h.reviews[0].State != "APPROVED" || e.h.reviews[0].User != aliceGH {
		t.Fatalf("setup: %+v", e.h.reviews)
	}
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m2", Author: alice, Message: "Patch Set 2: Code-Review+1", RevisionNumber: 2})
	e.g.vote("alice", "Code-Review", 1)
	e.sync(t)
	if len(e.h.dismissed) == 0 {
		t.Errorf("alice's approval stands on GitHub though her Gerrit vote is +1")
	}
}

// A change message's GitHub copy, as alice or as the bot, edited by
// someone else is put back, like any other copy (tenth audit)...
func TestMessageCopyEditedByOtherIsRestored(t *testing.T) {
	for _, linked := range []bool{true, false} {
		e, a := linkedEnv(t)
		if !linked {
			delete(a.linked, 1)
		}
		e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"},
			Message: "Patch Set 2:\n\nLooks wrong to me.", RevisionNumber: 2}}
		e.sync(t)
		e.h.ics[0].Body = strings.Replace(e.h.ics[0].Body, "Looks wrong to me.", "Ship it, I approve.", 1)
		e.h.edited(e.h.ics[0].ID, "mallory")
		e.sync(t)
		if strings.Contains(e.h.ics[0].Body, "Ship it") {
			t.Errorf("linked %v: someone else's edit of alice's message copy stands (by %s): %q", linked, e.h.ics[0].User.Login, e.h.ics[0].Body)
		}
	}
}

// ...and so is the text of an approval posted as her.
func TestVoteReviewEditedByOtherIsRestored(t *testing.T) {
	e, _ := linkedEnv(t)
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"},
		Message: "Patch Set 2: Code-Review+2\n\nOnly the docs need work.", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if len(e.h.reviews) != 1 || e.h.reviews[0].State != "APPROVED" {
		t.Fatalf("setup: %+v", e.h.reviews)
	}
	id := e.h.reviews[0].ID
	e.h.reviews[0].Body = strings.Replace(e.h.reviews[0].Body, "Only the docs need work.", "No need to test this.", 1)
	e.h.reviewEdits = map[int64]string{id: "mallory"}
	e.sync(t)
	if b := e.h.reviews[0].Body; strings.Contains(b, "No need") || !strings.Contains(b, "Only the docs need work.") {
		t.Errorf("someone else's edit of alice's approval stands: %q", b)
	}
	// GitHub refusing to restore it does not stop the sync.
	e.h.refuseReviewEdit = true
	e.h.reviewEdits = map[int64]string{id: "mallory"}
	e.sync(t)
	e.h.refuseReviewEdit = false
	// Her own edit is hers.
	e.h.reviews[0].Body = strings.Replace(e.h.reviews[0].Body, "Only the docs", "Only the docs and tests", 1)
	e.h.reviewEdits = map[int64]string{id: aliceGH.Login}
	e.sync(t)
	if !strings.Contains(e.h.reviews[0].Body, "Only the docs and tests") {
		t.Errorf("alice's own edit was undone: %q", e.h.reviews[0].Body)
	}
}

// Sixteenth review (impersonation, bad code submitted): a vote cast in Gerrit
// on alice's behalf by someone else (on_behalf_of with Label-As, or
// another account's Run-As: Gerrit sets real_author) is not alice's: it
// must not become her GitHub approval, posted with her own token, which
// is what GitHub's required reviews count where GitHub owns the branches.
func TestVoteOnBehalfIsNoApprovalAsThem(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, RealAuthor: &carol, Message: "Patch Set 2: Code-Review+2\n\nLGTM.", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	for _, rv := range e.h.reviews {
		if rv.User == aliceGH {
			t.Errorf("carol's vote on alice's behalf became alice's own GitHub review: %+v", rv)
		}
	}
	for _, ic := range e.h.ics {
		if ic.User == aliceGH {
			t.Errorf("carol's message on alice's behalf posted with alice's token: %q", ic.Body)
		}
	}
	if !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool {
		return strings.Contains(ic.Body, "**Carol**, as **Alice Trusted (alice)**,") && strings.Contains(ic.Body, "LGTM.")
	}) {
		t.Errorf("no copy as the bridge naming both: %+v", e.h.ics)
	}
}

// Sixteenth review (bad code submitted): a linked user's Gerrit +2 on patch set
// 2, mirrored after the PR's branch moved on to a commit Gerrit does not
// have (sha3: a push the importer refuses or has not got to), is not
// posted as their GitHub approval of patch set 2's commit. GitHub's
// "dismiss stale approvals" acts on a push, which is past: an approval
// made afterwards stands on the PR whose head is sha3, code its author
// never saw, and Gerrit's current patch set still carries the +2, so
// dismissStale never takes it back.
func TestLateVoteIsNoApprovalOfAMovedHead(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.h.head = "sha3" // pushed on GitHub after her vote; not a patch set
	e.sync(t)
	e.sync(t)
	for _, rv := range e.h.reviews {
		if rv.State == "APPROVED" {
			t.Errorf("approval posted on %s while the PR's head is sha3, and left standing (dismissed: %v)", rv.CommitID, e.h.dismissed)
		}
	}
}

// Seventeenth review (impersonation), the comments' half of
// TestVoteOnBehalfIsNoApprovalAsThem: carol posts a review on alice's
// behalf (another account's Run-As, or on_behalf_of), a reply "Done"
// that resolves a thread. Gerrit sets real_author only on the review's
// change message; its comments, author alice, belong to it (their
// change_message_id, and the same timestamp). They are carol's words
// and carol's resolution: not to be posted, nor the thread resolved,
// with alice's own token as alice.
func TestCommentsOnBehalfAreNotPostedAsThem(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(fmt.Sprint("by message id ", byID), func(t *testing.T) { commentsOnBehalf(t, byID) })
	}
}

func commentsOnBehalf(t *testing.T, byID bool) {
	e, _ := linkedEnv(t)
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	carol := gerrit.AccountInfo{AccountID: 2, Name: "Carol", Username: "carol"}
	e.g.comments = append(e.g.comments, gerrit.CommentInfo{ID: "c1", Path: "a.txt", PatchSet: 2, Line: 2, Message: "Please fix.", Updated: e.g.tick(),
		Unresolved: true, Author: carol})
	e.sync(t)
	root := e.h.rcs[0].ID
	// alice's own reply first: a comment is matched to its own message,
	// not to any of its author's.
	own := e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m0", Author: alice, Date: own, Message: "Patch Set 2:\n\nLooking.", RevisionNumber: 2})
	at := e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m1", Author: alice, RealAuthor: &carol, Date: at,
		Message: "Patch Set 2:\n\n(3 comments)", RevisionNumber: 2})
	e.g.comments = append(e.g.comments,
		gerrit.CommentInfo{ID: "c2", Path: "a.txt", PatchSet: 2, Line: 2, Message: "Done", InReplyTo: "c1", Updated: at, Author: alice},
		gerrit.CommentInfo{ID: "c3", Path: gerrit.PatchSetLevel, PatchSet: 2, Message: "Ship it.", Updated: at, Author: alice},
		gerrit.CommentInfo{ID: "c4", Path: "a.txt", PatchSet: 2, Line: 3, Message: "And this.", Updated: at, Author: alice})
	if byID {
		// Gerrit names the reply's change message; the times need not match.
		for i := range e.g.comments[1:] {
			e.g.comments[1+i].ChangeMessageID, e.g.comments[1+i].Updated = "m1", e.g.tick()
		}
	}
	e.sync(t)
	for _, rc := range e.h.rcs {
		if rc.User == aliceGH {
			t.Errorf("carol's comment on alice's behalf posted with alice's token: %q", rc.Body)
		}
	}
	for _, ic := range e.h.ics {
		if ic.User == aliceGH && !strings.Contains(ic.Body, "gerrit/5/m0") { // alice's own reply is hers
			t.Errorf("carol's comment on alice's behalf posted with alice's token: %q", ic.Body)
		}
	}
	if e.h.resolvedBy[root] == aliceGH {
		t.Errorf("carol's resolution on alice's behalf made with alice's token")
	}
	if !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool {
		return strings.Contains(ic.Body, "**Carol**, as **Alice Trusted (alice)**,") && strings.Contains(ic.Body, "Ship it.")
	}) {
		t.Errorf("no copy as the bridge naming both: %+v", e.h.ics)
	}
}

// Seventeenth review (bad code submitted): where GitHub owns the
// branches, GitHub's required reviews are the gate, and GitHub never
// counts a PR author's approval of their own code; but a Gerrit-born
// change's PR is the bridge's, and so is every push of a Gerrit patch
// set. A linked user's +2 on a change they own, or on code they
// uploaded, is no GitHub approval (its words are a comment); on someone
// else's code it is. Where Gerrit owns the branches, as on main.
func TestLinkedVoteOnOwnCodeIsNoApproval(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob", Username: "bobg"}
	for _, tc := range []struct {
		name         string
		githubOwned  bool
		owner        gerrit.AccountInfo
		ps1, ps2     gerrit.AccountInfo
		wantApproval bool
	}{
		{"her own change", true, alice, bob, bob, false},
		{"her upload", true, bob, bob, alice, false},
		{"bob's rework of hers", true, bob, alice, bob, false}, // she chose its parents (codeFrom)
		{"bob's", true, bob, bob, bob, true},
		{"her own, Gerrit-owned", false, alice, alice, alice, true},
		{"her upload as bob", true, bob, bob, bob, false},           // ps2 uploaded by alice on bob's behalf
		{"her message edit of bob's", true, bob, bob, alice, false}, // ps2 message-only: hers to have pushed
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := linkedEnv(t)
			e.runAs()
			e.c.GitHubOwned = tc.githubOwned
			e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
			e.g.owner = tc.owner
			e.g.uploaders = map[int]gerrit.AccountInfo{1: tc.ps1, 2: tc.ps2}
			switch tc.name {
			case "her upload as bob":
				e.g.realUploaders = map[int]*gerrit.AccountInfo{2: &alice}
			case "her message edit of bob's":
				e.g.kinds = map[int]string{2: "NO_CODE_CHANGE"}
			}
			e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2\n\nLGTM.", RevisionNumber: 2}}
			e.g.vote("alice", "Code-Review", 2)
			e.sync(t)
			approved := slices.ContainsFunc(e.h.reviews, func(rv github.Review) bool { return rv.State == "APPROVED" && rv.User == aliceGH })
			if approved != tc.wantApproval {
				t.Errorf("approved %v, want %v (reviews %+v)", approved, tc.wantApproval, e.h.reviews)
			}
			if !tc.wantApproval && !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool { return strings.Contains(ic.Body, "LGTM.") }) {
				t.Errorf("her words not posted: %+v", e.h.ics)
			}
		})
	}
}

// A +2 not posted while its commit was not the PR's head, then changed
// to -2, is not posted as an approval when the branch goes back to that
// commit (seventeenth review).
func TestWithdrawnVoteIsNoLaterApproval(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.h.head = "sha3" // the author pushed right after her vote
	e.sync(t)
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m2", Author: alice, Message: "Patch Set 2: Code-Review-2\n\nNo, this is wrong.", RevisionNumber: 2})
	e.g.vote("alice", "Code-Review", -2)
	e.sync(t)
	e.h.head = "sha2" // force-pushed back
	e.sync(t)
	last := ""
	for _, rv := range e.h.reviews {
		if rv.User == aliceGH {
			last = rv.State
		}
	}
	if last == "APPROVED" {
		t.Errorf("alice's withdrawn +2 is her latest GitHub review: %+v", e.h.reviews)
	}
}

// A +2 is an approval only while it is still its author's: not once
// Gerrit no longer has it (removed by the owner, say, with no message of
// theirs), nor, on an older patch set GitHub's PR is at, once they
// removed it there (seventeenth review).
func TestApprovalOnlyWhileTheVoteStands(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	approved := func(e *commentsEnv) bool {
		return slices.ContainsFunc(e.h.reviews, func(rv github.Review) bool { return rv.State == "APPROVED" && rv.User == aliceGH })
	}
	t.Run("removed in Gerrit", func(t *testing.T) {
		e, _ := linkedEnv(t)
		e.runAs()
		e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
		e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
		e.g.vote("alice", "Code-Review", 0)
		e.sync(t)
		if approved(e) {
			t.Errorf("approved with no +2 in Gerrit: %+v", e.h.reviews)
		}
		e.g.vote("alice", "Code-Review", 2)
		e.sync(t)
		if !approved(e) {
			t.Errorf("not approved once Gerrit has it: %+v", e.h.reviews)
		}
	})
	t.Run("removed on an older patch set", func(t *testing.T) {
		e, _ := linkedEnv(t)
		e.runAs()
		e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
		e.h.head = "sha1"
		e.g.messages = []gerrit.ChangeMessage{
			{ID: "m1", Author: alice, Message: "Patch Set 1: Code-Review+2", RevisionNumber: 1},
			{ID: "m2", Author: alice, Message: "Patch Set 1: -Code-Review", RevisionNumber: 1},
		}
		e.sync(t)
		if approved(e) {
			t.Errorf("approved after she removed it: %+v", e.h.reviews)
		}
	})
}
