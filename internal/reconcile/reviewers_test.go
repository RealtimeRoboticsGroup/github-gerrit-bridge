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

// fakeGitHubAccounts maps Gerrit usernames to GitHub accounts; unknown, if set,
// is its error.
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
	mike    = gerrit.AccountInfo{AccountID: 20, Username: "mikebauer"}
	mikeGH  = github.User{Login: "mike-gh", ID: 77}
	jane    = gerrit.AccountInfo{AccountID: 21, Username: "jane"}
	janeGH  = github.User{Login: "jane-gh", ID: 78}
	ownerGA = gerrit.AccountInfo{AccountID: 22, Username: "owen"}
	ownerGH = github.User{Login: "owen-gh", ID: 79}
)

// reviewersEnv is a comment sync where GitHub owns the branches: change 5
// is owen's, PR 1 opened as him.
func reviewersEnv(t *testing.T) (*commentsEnv, *fakeGitHubAccounts) {
	e := newCommentsEnv(t)
	e.c.GitHubOwned = true
	a := &fakeGitHubAccounts{m: map[string]github.User{"mikebauer": mikeGH, "jane": janeGH, "owen": ownerGH}}
	e.c.Reviewers = a
	e.c.Users = &Users{Overrides: map[string]string{"mike-gh": "mikebauer", "jane-gh": "jane", "owen-gh": "owen"}}
	e.g.owner = ownerGA
	e.h.author = ownerGH
	e.h.ids = map[string]int64{"mike-gh": 77, "jane-gh": 78, "owen-gh": 79}
	return e, a
}

func (e *commentsEnv) pairs(t *testing.T) []state.Reviewer {
	t.Helper()
	st, err := e.st.Load(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	return st.State.Reviewers
}

// git push origin HEAD:refs/for/main%r=mikebauer: mikebauer is asked to
// review the PR, once. A review on GitHub ends the request; it is not
// made again.
func TestGerritReviewerIsAskedOnGitHub(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
	// Gerrit lists every reviewer with each label, at 0 until they vote.
	e.g.vote("mikebauer", "Code-Review", 0)
	e.sync(t)
	if !slices.Equal(e.h.asks, []string{"+mike-gh"}) {
		t.Fatalf("asks = %v", e.h.asks)
	}
	if got := e.pairs(t); !slices.Equal(got, []state.Reviewer{{GerritUser: "mikebauer", GitHubID: 77}}) {
		t.Errorf("pairs = %+v", got)
	}
	e.sync(t)
	if len(e.h.asks) != 1 {
		t.Errorf("asked again: %v", e.h.asks)
	}
	// Mike reviews on GitHub: GitHub drops the request.
	e.h.requested = nil
	e.h.reviews = append(e.h.reviews, github.Review{ID: 90, State: "COMMENTED", User: mikeGH, CommitID: "sha2"})
	e.sync(t)
	if len(e.h.asks) != 1 {
		t.Errorf("asked again after reviewing: %v", e.h.asks)
	}
}

// Only reviewers are asked: not CCs, the owner, the bridge, the PR's
// author, anyone who has voted, anyone already asked or who has reviewed
// on GitHub, or anyone with no GitHub account.
func TestOnlyWaitingReviewersAreAsked(t *testing.T) {
	e, a := reviewersEnv(t)
	bridge := gerrit.AccountInfo{AccountID: bridgeID, Username: "bridge"}
	a.m["bridge"] = github.User{Login: "bridge", ID: 1}
	voter := gerrit.AccountInfo{AccountID: 23, Username: "vic"}
	a.m["vic"] = github.User{Login: "vic-gh", ID: 80}
	nobody := gerrit.AccountInfo{AccountID: 24, Username: "nobody"}
	e.g.vote("vic", "Code-Review", 1)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {ownerGA, bridge, voter, nobody}, "CC": {mike}}
	e.sync(t)
	if len(e.h.asks) != 0 {
		t.Errorf("asks = %v", e.h.asks)
	}
	// Already asked on GitHub, or already reviewed there: not asked, but
	// matched up.
	e.g.reviewers["REVIEWER"] = append(e.g.reviewers["REVIEWER"], mike, jane)
	e.h.requested = []github.User{mikeGH}
	e.h.reviews = append(e.h.reviews, github.Review{ID: 91, State: "APPROVED", User: janeGH, CommitID: "sha2"})
	e.sync(t)
	if len(e.h.asks) != 0 {
		t.Errorf("asks = %v", e.h.asks)
	}
	if got := e.pairs(t); len(got) != 2 {
		t.Errorf("pairs = %+v", got)
	}
}

// The change's owner is not asked though someone else opened its PR
// (the bot, before PRs were opened as owners); nor is the PR's author
// though not the owner.
func TestOwnerAndAuthorAreNotAsked(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.h.author = github.User{Login: "bridge", ID: 1}
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {ownerGA}}
	e.sync(t)
	if len(e.h.asks) != 0 {
		t.Errorf("the owner was asked: %v", e.h.asks)
	}
	e.h.author = janeGH
	e.g.reviewers["REVIEWER"] = append(e.g.reviewers["REVIEWER"], jane)
	e.sync(t)
	if len(e.h.asks) != 0 {
		t.Errorf("the PR's author was asked: %v", e.h.asks)
	}
}

// Nothing is asked on a closed PR or change.
func TestClosedChangesReviewersAreNotAsked(t *testing.T) {
	for _, closed := range []func(*commentsEnv){
		func(e *commentsEnv) { e.h.closed = true },
		func(e *commentsEnv) { e.g.status = "ABANDONED" },
	} {
		e, _ := reviewersEnv(t)
		e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
		closed(e)
		e.sync(t)
		if len(e.h.asks) != 0 {
			t.Errorf("asks = %v", e.h.asks)
		}
	}
}

// A reviewer removed in Gerrit is no longer asked on GitHub; added back,
// they are asked again.
func TestRemovedReviewerIsNoLongerAsked(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
	e.sync(t)
	e.g.reviewers["REVIEWER"] = nil
	e.sync(t)
	if !slices.Equal(e.h.asks, []string{"+mike-gh", "-mike-gh"}) || len(e.h.requested) != 0 {
		t.Fatalf("asks = %v, requested %v", e.h.asks, e.h.requested)
	}
	if got := e.pairs(t); len(got) != 0 {
		t.Errorf("pairs = %+v", got)
	}
	// Withdrawn on GitHub, not added back in Gerrit as from GitHub.
	if e.g.reviews != 0 {
		t.Errorf("Gerrit reviews: %d", e.g.reviews)
	}
	e.g.reviewers["REVIEWER"] = []gerrit.AccountInfo{mike}
	e.sync(t)
	if !slices.Equal(e.h.asks, []string{"+mike-gh", "-mike-gh", "+mike-gh"}) {
		t.Errorf("asks = %v", e.h.asks)
	}
	// A withdrawal GitHub refuses is tried again.
	e.g.reviewers["REVIEWER"] = nil
	e.h.refuseAsk = errors.New("GitHub is down")
	e.sync(t)
	if len(e.pairs(t)) != 1 {
		t.Errorf("a failed withdrawal was forgotten: %+v", e.pairs(t))
	}
	e.h.refuseAsk = nil
	e.sync(t)
	if len(e.h.requested) != 0 || len(e.pairs(t)) != 0 {
		t.Errorf("not withdrawn: %v %+v", e.h.requested, e.pairs(t))
	}
	e.g.reviewers["REVIEWER"] = []gerrit.AccountInfo{mike}
	e.sync(t)
	// Moved to CC: no longer asked either.
	e.g.reviewers = map[string][]gerrit.AccountInfo{"CC": {mike}}
	e.sync(t)
	if len(e.h.requested) != 0 {
		t.Errorf("requested = %v", e.h.requested)
	}
}

// Someone asked to review on GitHub becomes a Gerrit reviewer, once; the
// request is not then made again from Gerrit, and withdrawing it on
// GitHub leaves Gerrit alone.
func TestGitHubRequestMakesAGerritReviewer(t *testing.T) {
	e, _ := reviewersEnv(t)
	e.h.requested = []github.User{janeGH}
	e.sync(t)
	if got := e.g.reviewers["REVIEWER"]; len(got) != 1 || got[0].Username != "jane" {
		t.Fatalf("Gerrit reviewers = %+v", e.g.reviewers)
	}
	if len(e.h.asks) != 0 {
		t.Errorf("asks = %v", e.h.asks)
	}
	if got := e.pairs(t); !slices.Equal(got, []state.Reviewer{{GerritUser: "jane", GitHubID: 78}}) {
		t.Errorf("pairs = %+v", got)
	}
	reviews := e.g.reviews
	e.h.requested = nil
	e.sync(t)
	if e.g.reviews != reviews || len(e.g.reviewers["REVIEWER"]) != 1 || len(e.h.asks) != 0 {
		t.Errorf("after the request was withdrawn: reviews %d->%d, reviewers %+v, asks %v", reviews, e.g.reviews, e.g.reviewers, e.h.asks)
	}
	// Someone already a Gerrit reviewer is not added again.
	e.g.reviewers["REVIEWER"] = append(e.g.reviewers["REVIEWER"], mike)
	e.h.requested = []github.User{mikeGH}
	e.sync(t)
	if e.g.reviews != reviews {
		t.Errorf("mikebauer added again: %+v", e.g.reviewers)
	}
	// Nor is a reviewer who voted (and so was not asked from Gerrit),
	// asked again on GitHub.
	e.g.vote("vic", "Code-Review", 1)
	e.g.reviewers["REVIEWER"] = append(e.g.reviewers["REVIEWER"], gerrit.AccountInfo{AccountID: 23, Username: "vic"})
	e.c.Users = &Users{Overrides: map[string]string{"mike-gh": "mikebauer", "jane-gh": "jane", "vic-gh": "vic"}}
	e.h.requested = []github.User{{Login: "vic-gh", ID: 80}}
	e.sync(t)
	if e.g.reviews != reviews {
		t.Errorf("vic added again: %+v", e.g.reviewers)
	}
	// Nobody the mapping knows is not added.
	e.h.requested = []github.User{mikeGH, {Login: "stranger", ID: 99}}
	e.sync(t)
	if len(e.g.reviewers["REVIEWER"]) != 3 {
		t.Errorf("a stranger was added: %+v", e.g.reviewers)
	}
}

// A request GitHub refuses for good (422: not a collaborator) is not made
// every sync; one that failed otherwise is tried again; who is who being
// unknown asks nobody.
func TestRefusedRequests(t *testing.T) {
	// Gerrit refusing the reviewer: tried again.
	e, _ := reviewersEnv(t)
	e.h.requested = []github.User{janeGH}
	e.g.refuseReviewers = true
	e.sync(t)
	if len(e.pairs(t)) != 0 {
		t.Fatalf("pairs = %+v", e.pairs(t))
	}
	e.g.refuseReviewers = false
	e.sync(t)
	if len(e.g.reviewers["REVIEWER"]) != 1 {
		t.Fatalf("not tried again: %+v", e.g.reviewers)
	}

	e, a := reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
	a.unknown = &IdentityUnknown{errors.New("down")}
	e.sync(t)
	if len(e.pairs(t)) != 0 {
		t.Fatalf("pairs = %+v", e.pairs(t))
	}
	a.unknown = nil
	e.h.refuseAsk = errors.New("GitHub is down")
	e.sync(t)
	if len(e.pairs(t)) != 0 {
		t.Fatalf("pairs = %+v", e.pairs(t))
	}
	e.h.refuseAsk = nil
	e.sync(t)
	if !slices.Equal(e.h.asks, []string{"+mike-gh"}) {
		t.Fatalf("not tried again: %v", e.h.asks)
	}

	e, a = reviewersEnv(t)
	e.g.reviewers = map[string][]gerrit.AccountInfo{"REVIEWER": {mike}}
	e.h.refuseAsk = &github.APIError{Status: 422, Message: "Reviews may only be requested from collaborators."}
	e.sync(t)
	asked := a.asked
	e.h.refuseAsk = nil
	e.sync(t)
	if len(e.h.asks) != 0 || a.asked != asked {
		t.Errorf("a refused request was made again: %v", e.h.asks)
	}
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
		if err != nil || ok != (c.want.ID != 0) || got != c.want {
			t.Errorf("%+v (match %v): %+v %v %v, want %+v", c.a, c.match, got, ok, err, c.want)
		}
	}
	// With a directory, it alone says (as for Lookup).
	r.Directory, r.MatchUsernames = fakeDirectoryOf{}, true
	if got, ok, err := r.GitHubFor(ctx, brt); ok || err != nil {
		t.Errorf("with a directory, by username: %+v %v", got, err)
	}
}

// Someone asked on GitHub who has no Gerrit account (yet) stays asked on
// GitHub only, and is not recorded: once they have one (signed in to
// Gerrit), the next sync makes them a Gerrit reviewer, if still asked.
func TestGitHubReviewerWithoutAGerritAccount(t *testing.T) {
	e, _ := reviewersEnv(t)
	newcomer := github.User{Login: "new-gh", ID: 81, Type: "User"}
	e.h.requested = []github.User{newcomer, {Login: "Copilot", ID: 82, Type: "Bot"}}
	e.sync(t)
	// Said once in Gerrit (not for a bot), and still asked on GitHub.
	notes := func() (got []string) {
		for _, m := range e.g.messages {
			if strings.Contains(m.Message, "asked to review on GitHub") {
				got = append(got, m.Message)
			}
		}
		return got
	}
	if got := notes(); len(got) != 1 || !strings.Contains(got[0], "@new-gh") || len(e.g.reviewers["REVIEWER"]) != 0 || len(e.h.requested) != 2 {
		t.Fatalf("notes %q, reviewers %+v, requested %v", got, e.g.reviewers, e.h.requested)
	}
	e.sync(t)
	if got := notes(); len(got) != 1 || len(e.h.asks) != 0 {
		t.Fatalf("said again, or the request was withdrawn: %q %v", got, e.h.asks)
	}
	// Gerrit refusing the note: said next time.
	e2, _ := reviewersEnv(t)
	e2.h.requested = []github.User{newcomer}
	e2.g.refuseNotes = true
	e2.sync(t)
	e2.g.refuseNotes = false
	e2.sync(t)
	if e2.g.reviews != 1 {
		t.Errorf("not said after Gerrit refused it: %d", e2.g.reviews)
	}
	// They sign in to Gerrit: made a reviewer.
	e.c.Users = &Users{Overrides: map[string]string{"new-gh": "newcomer"}}
	e.sync(t)
	if got := e.g.reviewers["REVIEWER"]; len(got) != 1 || got[0].Username != "newcomer" {
		t.Errorf("Gerrit reviewers = %+v", e.g.reviewers)
	}
	if got := e.pairs(t); !slices.Equal(got, []state.Reviewer{{GerritUser: "newcomer", GitHubID: 81}}) {
		t.Errorf("pairs = %+v", got)
	}
	// Asked again later with no account: said again.
	e.c.Users = &Users{}
	e.h.requested = []github.User{{Login: "other-gh", ID: 83, Type: "User"}}
	e.sync(t)
	e.h.requested = nil
	e.sync(t)
	if len(e.pairs(t)) != 1 {
		t.Errorf("a request that ended is still kept: %+v", e.pairs(t))
	}
	e.h.requested = []github.User{{Login: "other-gh", ID: 83, Type: "User"}}
	e.sync(t)
	if got := notes(); len(got) != 3 {
		t.Errorf("notes = %q", got)
	}
}
