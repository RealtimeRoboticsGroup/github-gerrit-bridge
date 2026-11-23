package reconcile

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// fakeGitHubAccounts maps Gerrit usernames to GitHub accounts; unknown,
// if set, is its error.
type fakeGitHubAccounts struct {
	m       map[string]github.User
	unknown error
	asked   int
}

func (f *fakeGitHubAccounts) GitHubFor(_ context.Context, a gerrit.AccountInfo) (github.User, bool, error) {
	f.asked++
	if f.unknown != nil {
		return github.User{}, false, f.unknown
	}
	u, ok := f.m[a.Username]
	return u, ok, nil
}

var (
	mike     = gerrit.AccountInfo{AccountID: 20, Username: "mikebauer"}
	mikeGH   = github.User{Login: "mike-gh", ID: 77, Type: "User"}
	jane     = gerrit.AccountInfo{AccountID: 21, Username: "jane"}
	janeGH   = github.User{Login: "jane-gh", ID: 78, Type: "User"}
	ownerGA  = gerrit.AccountInfo{AccountID: 22, Username: "owen"}
	ownerGH  = github.User{Login: "owen-gh", ID: 79, Type: "User"}
	austin   = gerrit.AccountInfo{AccountID: 25, Username: "austin"} // trusted
	stranger = gerrit.AccountInfo{AccountID: 26, Username: "stranger"}
	bridgeGA = gerrit.AccountInfo{AccountID: bridgeID, Username: "bridge"}
)

// reviewersEnv is a comment sync where GitHub owns the branches: change 5
// is owen's, PR 1 opened as him; austin and owen are trusted.
func reviewersEnv(t *testing.T) (*commentsEnv, *fakeGitHubAccounts) {
	e := newCommentsEnv(t)
	e.c.GitHubOwned = true
	a := &fakeGitHubAccounts{m: map[string]github.User{"mikebauer": mikeGH, "jane": janeGH, "owen": ownerGH}}
	e.c.Reviewers = a
	e.c.Trusted = func(_ context.Context, id int) bool { return id == austin.AccountID || id == ownerGA.AccountID }
	e.c.Users = &Users{Overrides: map[string]string{"mike-gh": "mikebauer", "jane-gh": "jane", "owen-gh": "owen"}}
	e.g.owner = ownerGA
	e.g.accounts = map[string]gerrit.AccountInfo{"mikebauer": mike, "jane": jane, "owen": ownerGA}
	e.h.author = ownerGH
	e.h.ids = map[string]int64{"mike-gh": 77, "jane-gh": 78, "owen-gh": 79}
	return e, a
}

func (e *commentsEnv) seen(t *testing.T) *state.Reviewers {
	t.Helper()
	st, err := e.st.Load(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	return st.State.Reviewers
}

// reviewed is u's review on GitHub, which ends their request.
func (e *commentsEnv) reviewed(u github.User, state string) {
	e.h.requested = slices.DeleteFunc(e.h.requested, func(r github.User) bool { return r.ID == u.ID })
	e.h.reviews = append(e.h.reviews, github.Review{ID: e.h.id(), State: state, User: u, CommitID: "sha2", SubmittedAt: time.Now()})
}

// exported is ps, uploaded to Gerrit by by, which the bridge records once
// it has pushed it to the PR.
func (e *commentsEnv) exported(t *testing.T, by gerrit.AccountInfo, ps int, sha string) string {
	t.Helper()
	ts := e.g.upload(by, ps, sha)
	if err := updateStateFn(context.Background(), e.st, 5, "export", func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: sha, GitHubSHA: sha})
	}); err != nil {
		t.Fatal(err)
	}
	return ts
}

// imported is the bridge importing patch set ps as sha from a GitHub push
// by by: Gerrit says by uploaded it, with no real uploader.
func (e *commentsEnv) imported(t *testing.T, by gerrit.AccountInfo, ps int, sha string) string {
	t.Helper()
	ts := e.g.upload(by, ps, sha)
	if err := updateStateFn(context.Background(), e.st, 5, "import", func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: sha, GitHubSHA: sha, Imported: true})
	}); err != nil {
		t.Fatal(err)
	}
	return ts
}

func (e *commentsEnv) wantAsks(t *testing.T, want ...string) {
	t.Helper()
	if !slices.Equal(e.h.asks, want) {
		t.Errorf("asks on GitHub = %v, want %v", e.h.asks, want)
	}
}

// git push origin HEAD:refs/for/main%r=mikebauer: austin makes mikebauer
// a reviewer, whose turn it is, and mikebauer is asked on GitHub, once.
// Their review ends the request; it is not made again.
func TestGerritReviewerIsAskedOnGitHub(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	e.g.vote("mikebauer", "Code-Review", 0) // Gerrit lists reviewers at 0
	e.sync(t)
	e.wantAsks(t, "+mike-gh")
	if s := e.seen(t); !slices.Equal(s.Requested, []state.GitHubAccount{{ID: 77, Login: "mike-gh"}}) {
		t.Errorf("seen = %+v", s)
	}
	e.sync(t) // GitHub's review_requested comes back: nothing
	e.reviewed(mikeGH, "COMMENTED")
	e.sync(t)
	e.sync(t)
	e.wantAsks(t, "+mike-gh")
	if _, in := e.g.attention[mike.AccountID]; in || len(e.g.reviewers["REVIEWER"]) != 1 {
		t.Errorf("after mike's review: attention %+v, reviewers %+v", e.g.attention, e.g.reviewers)
	}
}

// Gerrit's times are to the second: a trusted person and a stranger in
// the same second cannot be told apart, so neither is taken; and a
// reviewer update is about whom it names.
func TestOneSecondTwoPeople(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike, jane}}
	ts := e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "a", Author: austin, Date: ts, Message: "PTAL"})
	e.g.updates = append(e.g.updates, gerrit.ReviewerUpdate{Updated: ts, UpdatedBy: stranger, Reviewer: mike, State: "REVIEWER"})
	e.g.setAttention(mike, ts, "Reviewer was added", true)
	e.sync(t)
	e.wantAsks(t)
	// austin's reply in the same second as a stranger adding someone
	// else: jane's turn is austin's doing.
	ts = e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "b", Author: austin, Date: ts, Message: "PTAL"})
	e.g.updates = append(e.g.updates, gerrit.ReviewerUpdate{Updated: ts, UpdatedBy: stranger, Reviewer: gerrit.AccountInfo{AccountID: 31, Username: "y"}, State: "REVIEWER"})
	e.g.setAttention(jane, ts, "Someone replied", true)
	e.sync(t)
	e.wantAsks(t, "+jane-gh")
}

// Only a trusted person, in Gerrit, asks: not a stranger, not the bridge,
// not whatever Gerrit cannot say who did. Nor is the owner, a CC or the
// PR's author asked.
func TestOnlyTrustedPeopleAsk(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(stranger, mike)
	e.g.addReviewer(bridgeGA, jane)
	e.sync(t)
	e.wantAsks(t)

	e, _ = reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
	e.g.setAttention(mike, e.g.tick(), "Someone replied", true) // nobody Gerrit names
	e.g.addReviewer(austin, ownerGA)
	e.g.reviewers["CC"] = []gerrit.AccountInfo{jane}
	e.g.setAttention(jane, e.g.addReviewer(austin, gerrit.AccountInfo{AccountID: 30, Username: "x"}), "Someone replied", true)
	e.sync(t)
	e.wantAsks(t)

	e, _ = reviewersEnv(t)
	e.h.author = janeGH
	e.g.addReviewer(austin, jane)
	e.sync(t)
	e.wantAsks(t)

	// The owner, though the bot opened the PR.
	e, _ = reviewersEnv(t)
	e.h.author = github.User{Login: "bot", ID: 1}
	e.g.addReviewer(austin, ownerGA)
	e.sync(t)
	e.wantAsks(t)
}

// A reviewer whose turn it is again in Gerrit (austin replies, handing it
// to them) is asked again on GitHub; one whose turn ends there (they
// reply, or austin removes them) is no longer asked. A stranger taking
// them out of the attention set does nothing.
func TestTurnsFollowTheAttentionSet(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	e.sync(t)
	e.reviewed(mikeGH, "CHANGES_REQUESTED") // ends their turn in Gerrit too
	e.sync(t)
	if _, in := e.g.attention[mike.AccountID]; in {
		t.Errorf("still mike's turn after his review: %+v", e.g.attention)
	}
	e.g.reply(austin, "Done", []gerrit.AccountInfo{mike}, nil)
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "+mike-gh")

	e.g.reply(stranger, "meh", nil, []gerrit.AccountInfo{mike})
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "+mike-gh")
	e.g.setAttention(mike, e.g.tick(), "Someone replied", true) // back, by nobody Gerrit names
	e.sync(t)
	e.g.reply(mike, "LGTM", nil, []gerrit.AccountInfo{mike})
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "+mike-gh", "-mike-gh")

	e.g.reply(austin, "PTAL", []gerrit.AccountInfo{mike}, nil)
	e.sync(t)
	e.g.removeReviewer(austin, mike)
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "+mike-gh", "-mike-gh", "+mike-gh", "-mike-gh")
	if s := e.seen(t); len(e.h.requested) != 0 || len(s.Requested) != 0 {
		t.Errorf("requested = %v, seen %+v", e.h.requested, s)
	}
	// Who is who unknown for now when their turn ends: tried again.
	e.g.addReviewer(austin, mike)
	e.sync(t)
	e.g.removeReviewer(austin, mike)
	accounts := e.c.Reviewers
	e.c.Reviewers = &fakeGitHubAccounts{unknown: &IdentityUnknown{errors.New("down")}}
	e.sync(t)
	e.c.Reviewers = accounts
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "+mike-gh", "-mike-gh", "+mike-gh", "-mike-gh", "+mike-gh", "-mike-gh")
}

// A patch set austin uploads asks again whoever last requested changes on
// GitHub (Gerrit hands nobody the turn then); not whoever approved, and
// not for a patch set the bridge or a stranger uploaded.
func TestNewPatchSetAsksChangeRequestersAgain(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	e.g.addReviewer(austin, jane)
	e.sync(t)
	// GitHub counts each one's latest approval or change request; a
	// comment after either does not change it.
	e.reviewed(janeGH, "CHANGES_REQUESTED")
	e.reviewed(mikeGH, "APPROVED")
	e.reviewed(mikeGH, "CHANGES_REQUESTED")
	e.reviewed(mikeGH, "COMMENTED")
	e.reviewed(janeGH, "APPROVED")
	e.reviewed(github.User{Login: "lint[bot]", ID: 90, Type: "Bot"}, "CHANGES_REQUESTED")
	e.sync(t)
	e.g.upload(bridgeGA, 3, "sha3")
	e.sync(t)
	e.exported(t, stranger, 4, "sha4")
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh")
	e.exported(t, austin, 5, "sha5")
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh", "+mike-gh")
	e.exported(t, austin, 6, "sha6") // asked already: not again
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh", "+mike-gh")
	// Changes requested again: asked again by the next patch set, not
	// before.
	e.reviewed(mikeGH, "CHANGES_REQUESTED")
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh", "+mike-gh")
	// Nor while a draft; once it is ready.
	e.h.draft = true
	e.exported(t, austin, 7, "sha7")
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh", "+mike-gh")
	e.h.draft = false
	e.sync(t)
	e.wantAsks(t, "+jane-gh", "+mike-gh", "+mike-gh", "+mike-gh")
}

// Someone asked on GitHub becomes a Gerrit reviewer (whose turn it is);
// that is the bridge's doing, and not carried back. Withdrawn on GitHub
// (not ended by their review), it is no longer their turn in Gerrit;
// asked again, it is again.
func TestGitHubRequestsReachGerrit(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	if got := e.g.reviewers["REVIEWER"]; len(got) != 1 || got[0].Username != "jane" {
		t.Fatalf("Gerrit reviewers = %+v", e.g.reviewers)
	}
	e.sync(t)
	e.wantAsks(t)
	e.h.requested = nil
	e.sync(t)
	if _, in := e.g.attention[jane.AccountID]; in {
		t.Errorf("still jane's turn: %+v", e.g.attention)
	}
	e.sync(t)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	if _, in := e.g.attention[jane.AccountID]; !in {
		t.Errorf("not jane's turn: %+v", e.g.attention)
	}
	if n := len(e.g.reviewers["REVIEWER"]); n != 1 {
		t.Errorf("jane added again: %+v", e.g.reviewers)
	}
	// Ended by her review: no longer her turn.
	e.reviewed(janeGH, "APPROVED")
	e.sync(t)
	if _, in := e.g.attention[jane.AccountID]; in {
		t.Errorf("still jane's turn after her review: %+v", e.g.attention)
	}
	e.wantAsks(t)
	// Asked on GitHub when it is her turn already (a stranger gave it
	// her, which asks nobody): nothing to do.
	e.g.reply(stranger, "PTAL", []gerrit.AccountInfo{jane}, nil)
	e.sync(t)
	reviews := e.g.reviews
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	if e.g.reviews != reviews {
		t.Errorf("Gerrit written to: %+v", e.g.attention)
	}
	e.h.requested = nil
	e.c.Users = failingUsers{} // ended, who is who unknown: tried again
	e.sync(t)
	e.c.Users = &Users{Overrides: map[string]string{"mike-gh": "mikebauer", "jane-gh": "jane", "owen-gh": "owen"}}
	e.sync(t)
	if _, in := e.g.attention[jane.AccountID]; in {
		t.Errorf("still jane's turn: %+v", e.g.attention)
	}
	// The owner asked on GitHub (a bot-opened PR): not made a reviewer.
	e.h.requested = []github.User{ownerGH}
	e.sync(t)
	if n := len(e.g.reviewers["REVIEWER"]); n != 1 {
		t.Errorf("the owner was made a reviewer: %+v", e.g.reviewers)
	}
	if _, in := e.g.attention[ownerGA.AccountID]; in {
		t.Errorf("the owner's turn: %+v", e.g.attention)
	}
}

// A PR made and worked on GitHub alone, nobody doing anything in Gerrit,
// never has the bridge write to GitHub for its reviewers: what Gerrit
// shows of it is the bridge's (imports, mirrored reviews, reviewers it
// added), though done as the PR's trusted author.
// A patch set is someone's upload only once the bridge has recorded
// exporting it: until then it may be an import whose state the bridge
// has yet to save (third review).
func TestUnrecordedPatchSetsWait(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(bridgeGA, mike)
	e.g.addReviewer(bridgeGA, jane)
	e.g.setAttention(jane, e.g.tick(), "x", false)
	e.sync(t)
	e.reviewed(mikeGH, "CHANGES_REQUESTED")
	e.sync(t)
	// owen pushes to his PR; the bridge imports it, and its state is not
	// saved. Gerrit says owen uploaded it, and hands jane the turn with it.
	ts := e.g.upload(ownerGA, 3, "sha3")
	e.g.setAttention(jane, ts, "Someone replied", true)
	e.sync(t)
	e.wantAsks(t)
	// Recorded later as the import it was: still nobody asked.
	if err := updateStateFn(context.Background(), e.st, 5, "import", func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: 3, GerritSHA: "sha3", GitHubSHA: "sha3", Imported: true})
	}); err != nil {
		t.Fatal(err)
	}
	e.sync(t)
	e.wantAsks(t)
	// austin's Gerrit upload waits for its export to be recorded, then
	// asks.
	e.g.upload(austin, 4, "sha4")
	e.sync(t)
	e.wantAsks(t)
	if got := e.seen(t).PatchSet; got != 3 {
		t.Errorf("seen patch set %d before it is recorded, want 3", got)
	}
	if err := updateStateFn(context.Background(), e.st, 5, "export", func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: 4, GerritSHA: "sha4", GitHubSHA: "sha4"})
	}); err != nil {
		t.Fatal(err)
	}
	e.sync(t)
	e.wantAsks(t, "+mike-gh")
}

func TestGitHubOnlyPRNeverAsksOnGitHub(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.addReviewer(bridgeGA, mike)
	e.g.addReviewer(bridgeGA, jane)
	e.sync(t)
	e.reviewed(mikeGH, "CHANGES_REQUESTED")
	e.sync(t)
	// owen pushes to the PR: imported as him, which Gerrit says he
	// uploaded (were Gerrit to hand jane the turn with it, too).
	e.g.setAttention(jane, e.g.tick(), "x", false)
	e.sync(t)
	ts := e.imported(t, ownerGA, 3, "sha3")
	e.g.setAttention(jane, ts, "Someone replied", true)
	e.sync(t)
	// A commit-message edit the bridge made as him (Run-As).
	ts = e.g.uploadAs(ownerGA, &bridgeGA, 4, "sha4")
	e.g.setAttention(jane, ts, "Someone replied", false)
	e.sync(t)
	e.g.setAttention(jane, e.g.tick(), "x", true) // by nobody named
	e.sync(t)
	// owen comments on GitHub: mirrored as him (Run-As), handing mike
	// the turn; once tagged as the bridge's, once only said to be
	// another's.
	e.g.setAttention(mike, e.g.tick(), "Someone replied", false)
	e.sync(t)
	ts = e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m1", Author: ownerGA, Tag: commentTag, Date: ts, Message: "Patch Set 3:\n\n(1 comment)"})
	e.g.setAttention(mike, ts, "Someone replied", true)
	e.sync(t)
	e.g.setAttention(mike, e.g.tick(), "Someone replied", false)
	e.sync(t)
	// A vote mirrored as him: Gerrit makes him a reviewer, as his doing.
	ts = e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m2", Author: ownerGA, RealAuthor: &bridgeGA, Date: ts, Message: "Patch Set 3: Code-Review+1"})
	e.g.updates = append(e.g.updates, gerrit.ReviewerUpdate{Updated: ts, UpdatedBy: ownerGA, Reviewer: ownerGA, State: "REVIEWER"})
	e.g.setAttention(mike, ts, "Someone replied", true)
	e.sync(t)
	e.wantAsks(t)

	e, _ = reviewersEnv(t)
	e.g.upload(bridgeGA, 3, "sha3") // imported
	e.h.requested = []github.User{janeGH, mikeGH}
	e.sync(t)
	e.reviewed(mikeGH, "CHANGES_REQUESTED")
	// The bridge mirrors mike's review as him, which hands owen the turn
	// and takes mike's.
	ts = e.g.tick()
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "mm", Author: mike, Tag: commentTag, Date: ts, Message: "Patch Set 3:\n\n(1 comment)"})
	e.g.setAttention(ownerGA, ts, "Someone else replied", true)
	e.g.setAttention(mike, ts, "Someone replied", false)
	e.sync(t)
	e.g.upload(bridgeGA, 4, "sha4") // a push to the PR
	e.h.requested = nil             // withdrawn
	e.sync(t)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	e.sync(t)
	e.wantAsks(t)
}

// A PR from before the bridge started is taken as it is: nothing on it
// is carried over until something changes.
func TestPRsFromBeforeAreTakenAsTheyAre(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.c.Since = time.Now()
	e.h.createdAt = e.c.Since.Add(-time.Hour)
	e.g.addReviewer(austin, mike)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	e.wantAsks(t)
	if e.g.reviews != 0 {
		t.Errorf("Gerrit written to: %+v", e.g.reviewers)
	}
	e.g.reply(austin, "PTAL", []gerrit.AccountInfo{jane}, nil)
	e.g.addReviewer(austin, jane)
	e.sync(t)
	e.wantAsks(t)                                                                    // jane is asked already
	e.g.reply(austin, "and you", []gerrit.AccountInfo{}, []gerrit.AccountInfo{mike}) // mike's turn ends
	e.sync(t)
	e.g.reply(austin, "PTAL", []gerrit.AccountInfo{mike}, nil)
	e.sync(t)
	e.wantAsks(t, "+mike-gh")
}

// While a draft, nobody is asked; once ready, those whose turn it is are.
// A closed PR is left alone, and what it saw kept.
func TestDraftsAndClosedPRs(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.h.draft = true
	e.g.addReviewer(austin, mike)
	e.sync(t)
	e.wantAsks(t)
	e.h.draft = false
	e.sync(t)
	e.wantAsks(t, "+mike-gh")

	e, _ = reviewersEnv(t)
	e.sync(t)
	e.h.closed = true
	e.g.addReviewer(austin, mike)
	e.sync(t)
	e.wantAsks(t)
	if e.seen(t) == nil {
		t.Error("forgot what it saw")
	}
}

// A request GitHub refuses for good (422: not a collaborator) is not made
// again; one that failed otherwise, or whose GitHub account could not be
// found for now, is tried again.
func TestRefusedRequests(t *testing.T) {
	e, a := reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	a.unknown = &IdentityUnknown{errors.New("down")}
	e.sync(t)
	a.unknown = nil
	e.h.refuseAsk = errors.New("GitHub is down")
	e.sync(t)
	e.h.refuseAsk = nil
	e.sync(t)
	e.wantAsks(t, "+mike-gh")

	e, a = reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	e.h.refuseAsk = &github.APIError{Status: 422, Message: "Reviews may only be requested from collaborators."}
	e.sync(t)
	asked := a.asked
	e.h.refuseAsk = nil
	e.sync(t)
	if len(e.h.asks) != 0 || a.asked != asked {
		t.Errorf("a refused request was made again: %v", e.h.asks)
	}

	// Withdrawing, refused for now: tried again.
	e, _ = reviewersEnv(t)
	e.g.addReviewer(austin, mike)
	e.sync(t)
	e.g.removeReviewer(austin, mike)
	e.h.refuseAsk = errors.New("GitHub is down")
	e.sync(t)
	e.h.refuseAsk = nil
	e.sync(t)
	e.wantAsks(t, "+mike-gh", "-mike-gh")

	// Gerrit refusing for good (an inactive account, say): not again; a
	// conflict is for now.
	for _, refuse := range []func(*fakeGerrit){
		func(f *fakeGerrit) { f.refuseReviewers = true },
		func(f *fakeGerrit) { f.refuseNotes = true },
	} {
		for _, status := range []int{400, 409} {
			e, _ = reviewersEnv(t)
			e.c.Users = &Users{Overrides: map[string]string{"jane-gh": "jane"}}
			e.h.requested = []github.User{janeGH, {Login: "new-gh", ID: 81, Type: "User"}}
			refuse(e.g)
			e.g.refuseStatus = status
			e.sync(t)
			attempts := e.g.attempts
			e.sync(t)
			if again := e.g.attempts != attempts; again != (status == 409) {
				t.Errorf("refused %d: tried again %v", status, again)
			}
		}
	}
	e, _ = reviewersEnv(t)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	e.h.requested = nil
	e.g.refuseAttention, e.g.refuseStatus = true, 403
	e.sync(t)
	attempts := e.g.attempts
	e.sync(t)
	if e.g.attempts != attempts {
		t.Errorf("a refusal for good was tried again: %d -> %d", attempts, e.g.attempts)
	}

	// Gerrit refusing for now: tried again.
	e, _ = reviewersEnv(t)
	e.h.requested = []github.User{janeGH}
	e.g.refuseReviewers = true
	e.sync(t)
	e.g.refuseReviewers = false
	e.sync(t)
	if len(e.g.reviewers["REVIEWER"]) != 1 {
		t.Errorf("not tried again: %+v", e.g.reviewers)
	}
	e.h.requested = nil
	e.g.refuseAttention = true
	e.sync(t)
	e.g.refuseAttention = false
	e.sync(t)
	if _, in := e.g.attention[jane.AccountID]; in {
		t.Errorf("not tried again: %+v", e.g.attention)
	}
}

// Someone asked on GitHub with no Gerrit account (yet) is said so in
// Gerrit, once per request (not for a bot, and not again for the mapping
// being unknown for a while); once they have one, the next sync makes
// them a Gerrit reviewer, if still asked.
func TestGitHubReviewerWithoutAGerritAccount(t *testing.T) {
	e, _ := reviewersEnv(t)
	newcomer := github.User{Login: "new-gh", ID: 81, Type: "User"}
	e.h.requested = []github.User{newcomer, {Login: "Copilot", ID: 82, Type: "Bot"}}
	e.sync(t)
	notes := func() (got []string) {
		for _, m := range e.g.messages {
			if strings.Contains(m.Message, "asked to review on GitHub") {
				got = append(got, m.Message)
			}
		}
		return got
	}
	if got := notes(); len(got) != 1 || !strings.Contains(got[0], "@new-gh") || len(e.h.requested) != 2 {
		t.Fatalf("notes %q, requested %v", got, e.h.requested)
	}
	e.sync(t)
	e.c.Users = failingUsers{}
	e.sync(t)
	e.c.Users = &Users{}
	e.sync(t)
	if got := notes(); len(got) != 1 || len(e.h.asks) != 0 {
		t.Fatalf("said again, or the request was withdrawn: %q %v", got, e.h.asks)
	}
	e.c.Users = &Users{Overrides: map[string]string{"new-gh": "newcomer"}}
	e.sync(t)
	if got := e.g.reviewers["REVIEWER"]; len(got) != 1 || got[0].Username != "newcomer" {
		t.Errorf("Gerrit reviewers = %+v", e.g.reviewers)
	}
	if s := e.seen(t); len(s.Noted) != 0 {
		t.Errorf("still noted: %+v", s)
	}

	// Who is who unknown when first asked: tried again.
	e, _ = reviewersEnv(t)
	e.c.Users = failingUsers{}
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	e.c.Users = &Users{Overrides: map[string]string{"jane-gh": "jane"}}
	e.sync(t)
	if len(e.g.reviewers["REVIEWER"]) != 1 {
		t.Errorf("not tried again: %+v", e.g.reviewers)
	}

	// Gerrit refusing the note: said next time.
	e, _ = reviewersEnv(t)
	e.h.requested = []github.User{newcomer}
	e.g.refuseNotes = true
	e.sync(t)
	e.g.refuseNotes = false
	e.sync(t)
	if len(notes()) != 1 {
		t.Errorf("not said after Gerrit refused it: %q", notes())
	}
}

// failingUsers cannot say who anyone is for now.
type failingUsers struct{}

func (failingUsers) Lookup(context.Context, github.User) (string, bool, error) {
	return "", false, &IdentityUnknown{errors.New("down")}
}

// fakeDirectoryOf gives Gerrit users GitHub ids.
type fakeDirectoryOf struct {
	m   map[string]int64
	err error
}

func (d fakeDirectoryOf) ByGerritUser(user string) (ldap.Account, bool, error) {
	id, ok := d.m[user]
	return ldap.Account{GerritUser: user, GitHubID: id, GitHubLogin: "old-login"}, ok, d.err
}

type fakeUsersByID map[int64]github.User

func (f fakeUsersByID) UserByID(_ context.Context, id int64) (github.User, error) {
	u, ok := f[id]
	if !ok {
		return github.User{}, &github.APIError{Status: 404}
	}
	return u, nil
}

func (f fakeUsersByID) UserID(_ context.Context, login string) (int64, time.Time, error) {
	for id, u := range f {
		if strings.EqualFold(u.Login, login) {
			return id, time.Time{}, nil
		}
	}
	return 0, time.Time{}, &github.APIError{Status: 404}
}

// fakeIdentity maps GitHub ids to Gerrit users.
type fakeIdentity map[int64]string

func (f fakeIdentity) Lookup(_ context.Context, gh github.User) (string, bool, error) {
	u, ok := f[gh.ID]
	return u, ok, nil
}

// A Gerrit account's GitHub account is the one it linked, else the
// directory's, by id under its login now, and only while the mapping maps
// it back.
func TestReviewerAccounts(t *testing.T) {
	ctx := context.Background()
	users := fakeUsersByID{77: mikeGH, 78: janeGH}
	r := &ReviewerAccounts{
		Linked: func(_ context.Context, account int) (github.User, bool, error) {
			if account == jane.AccountID {
				return github.User{Login: "jane-old", ID: 78}, true, nil
			}
			return github.User{}, false, nil
		},
		Directory: fakeDirectoryOf{m: map[string]int64{"mikebauer": 77, "gone": 5, "liar": 78, "jane": 5}},
		Users:     fakeIdentity{77: "mikebauer", 78: "jane"},
		GitHub:    users,
	}
	for _, c := range []struct {
		a    gerrit.AccountInfo
		want github.User
	}{
		{mike, mikeGH}, // by the directory, under the login GitHub gives now
		{jane, janeGH}, // linked, whatever the directory says
		{a: gerrit.AccountInfo{AccountID: 30, Username: "gone"}},   // no such GitHub account now
		{a: gerrit.AccountInfo{AccountID: 31, Username: "liar"}},   // the mapping says 78 is jane's
		{a: gerrit.AccountInfo{AccountID: 32, Username: "nobody"}}, // not in the directory
		{a: gerrit.AccountInfo{AccountID: 20}},                     // no username
	} {
		got, ok, err := r.GitHubFor(ctx, c.a)
		if err != nil || ok != (c.want.ID != 0) || got != c.want {
			t.Errorf("%+v: %+v %v %v, want %+v", c.a, got, ok, err, c.want)
		}
	}
	r.Directory = fakeDirectoryOf{err: errors.New("down")}
	if _, _, err := r.GitHubFor(ctx, mike); !isIdentityUnknown(err) {
		t.Errorf("an unreadable directory: %v", err)
	}

	// Without a directory: an override's login, else the username if
	// usernames are matched; each only if the mapping agrees.
	users[90] = github.User{Login: "AustinSchuhBRT", ID: 90}
	users[91] = github.User{Login: "a-squatter", ID: 91}
	r = &ReviewerAccounts{Users: fakeIdentity{77: "mikebauer", 90: "AustinSchuhBRT"}, GitHub: users,
		Overrides: map[string]string{"0-gone": "mikebauer", "a-squatter": "mikebauer", "mike-gh": "MikeBauer"}}
	brt := gerrit.AccountInfo{AccountID: 40, Username: "AustinSchuhBRT"}
	for _, c := range []struct {
		a     gerrit.AccountInfo
		match bool
		want  github.User
	}{
		{a: mike, want: mikeGH},                // past a login GitHub does not have, and one the mapping disagrees with
		{a: brt},                               // not matching usernames
		{a: brt, match: true, want: users[90]}, // matching them
		{a: jane, match: true},                 // no such login
		{a: gerrit.AccountInfo{AccountID: 41, Username: "a-squatter"}, match: true}, // the mapping says 91 is nobody's
	} {
		r.MatchUsernames = c.match
		got, ok, err := r.GitHubFor(ctx, c.a)
		if err != nil || ok != (c.want.ID != 0) || got.ID != c.want.ID || got.Login != c.want.Login {
			t.Errorf("%+v (match %v): %+v %v %v, want %+v", c.a, c.match, got, ok, err, c.want)
		}
	}
	// With a directory, it alone says (as for Lookup).
	r.Directory, r.MatchUsernames = fakeDirectoryOf{}, true
	if got, ok, err := r.GitHubFor(ctx, brt); ok || err != nil {
		t.Errorf("with a directory, by username: %+v %v", got, err)
	}
}
