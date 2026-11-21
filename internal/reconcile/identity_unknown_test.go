package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// downIdentity is a mapping whose lookups cannot answer while *down (the
// directory failed partway through a reconcile), and are known's after.
type downIdentity struct {
	known Identity
	down  *bool
}

func (d downIdentity) Lookup(ctx context.Context, gh github.User) (string, bool, error) {
	if *d.down {
		return "", false, &IdentityUnknown{errors.New("the directory cannot be read")}
	}
	return d.known.Lookup(ctx, gh)
}

// A directory lookup that cannot answer is unknown, not nobody: the
// directory unreadable, or Gerrit not saying whether what it names is
// active. A pin that cannot be read is no reason (the directory decides).
// Fifteenth review: each of these answered nobody mid-reconcile.
func TestDirectoryLookupUnknownIsNotNobody(t *testing.T) {
	ctx := context.Background()
	s := &ldapSearcher{entries: []ldap.Entry{ldapEntry("austin", "AustinSchuh", "294043")}}
	u, d := ldapUsers(t, s)
	austin := github.User{Login: "AustinSchuh", ID: 294043}
	u.Pins = errPins{errors.New("Gerrit is down")}
	if user, ok, err := u.Lookup(ctx, austin); user != "austin" || !ok || err != nil {
		t.Errorf("pins unreadable: %q %v %v, want austin", user, ok, err)
	}
	u.Pins = nil
	active := u.Active
	u.Active = errAccounts{errors.New("Gerrit is down")}
	if _, ok, err := u.Lookup(ctx, austin); ok || !isIdentityUnknown(err) {
		t.Errorf("Gerrit not answering: %v %v, want unknown", ok, err)
	}
	u.Active = active
	s.err = errors.New("connection refused")
	d.Read(ctx)
	if _, ok, err := u.Lookup(ctx, austin); ok || !isIdentityUnknown(err) {
		t.Errorf("directory down: %v %v, want unknown", ok, err)
	}
	// Not in it is nobody, still.
	s.err = nil
	d.Read(ctx)
	if _, ok, err := u.Lookup(ctx, github.User{Login: "bob", ID: 31}); ok || err != nil {
		t.Errorf("unlisted: %v %v, want nobody", ok, err)
	}
	// Where usernames match (Gerrit owns, as on AOS), as main: never
	// unknown.
	plain := &Users{Gerrit: errAccounts{errors.New("Gerrit is down")}}
	if _, ok, err := plain.Lookup(ctx, austin); ok || err != nil {
		t.Errorf("username matching, Gerrit down: %v %v, want nobody as main", ok, err)
	}
}

// errAccounts is a Gerrit that answers no account query.
type errAccounts struct{ err error }

func (e errAccounts) QueryAccounts(context.Context, string) ([]gerrit.AccountInfo, error) {
	return nil, e.err
}

// errPins are pins that cannot be read or written.
type errPins struct{ err error }

func (e errPins) LoadIdentities(context.Context) (*state.Identities, error) { return nil, e.err }
func (e errPins) SaveIdentities(context.Context, *state.Identities, string) error {
	return e.err
}

// A comment sync and a /submit while who is who is unknown post nothing
// (not as the bot, nor "not linked") and wait; the next try, known,
// posts as the author and submits once.
func TestUnknownIdentityWaits(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	e.c.Now = func() time.Time { return now }
	down := true
	e.c.Users = downIdentity{e.c.Users, &down}
	e.h.human("a.txt", 3, "sha2", "retest", 0)
	e.h.ics = append(e.h.ics, github.IssueComment{ID: 2, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: now.Add(-time.Hour)})
	sub := &fakeSubmitter{}
	d := &Dispatcher{Code: &Code{}, Comments: e.c, Store: e.st, Logger: slog.Default(),
		Commands: &Commands{GitHub: e.h, Submitter: sub, Store: e.st, Identity: downIdentity{&Users{Overrides: map[string]string{"AustinSchuh": "alice"}}, &down},
			BridgeLogin: "bridge", Logger: slog.Default(), Now: func() time.Time { return now }}}
	ctx := context.Background()
	comments, posts := len(e.g.comments), e.h.posts
	err := d.Reconcile(ctx, Key{Comments: 5})
	var later *errLater
	if !errors.As(err, &later) || later.after != identityRetry {
		t.Fatalf("unknown: err %v, want to wait %v", err, identityRetry)
	}
	if len(e.g.comments) != comments || e.h.posts != posts || len(sub.calls) != 0 {
		t.Fatalf("unknown: posted %d to Gerrit, %d to GitHub, submitted %v", len(e.g.comments)-comments, e.h.posts-posts, sub.calls)
	}
	down = false
	if err := d.Reconcile(ctx, Key{Comments: 5}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(e.g.comments[comments:], func(gc gerrit.CommentInfo) bool {
		return gc.Author.Username == "bobg" && gc.Message == "retest"
	}) {
		t.Errorf("known: bob's comment not posted as bob: %+v", e.g.comments[comments:])
	}
	if len(sub.calls) != 1 || sub.calls[0] != "alice" {
		t.Errorf("known: submits %v, want one as alice", sub.calls)
	}
}

// A /submit whose author is unknown for now is not spent: no reply, not
// handled, and run when known.
func TestSubmitWaitsForWhoAsked(t *testing.T) {
	c, h, sub := newCommands(t)
	down := true
	c.Identity = downIdentity{c.Identity, &down}
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}})
	ctx := context.Background()
	n := len(h.ics)
	if err := c.Run(ctx, 5); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if len(h.ics) != n || len(sub.calls) != 0 {
		t.Fatalf("unknown: replied %v, submitted %v", h.ics[n:], sub.calls)
	}
	if st, _ := c.Store.Load(ctx, 5); len(st.State.HandledCommands) != 0 {
		t.Fatalf("unknown: handled %v", st.State.HandledCommands)
	}
	down = false
	if err := c.Run(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 || !strings.Contains(h.ics[len(h.ics)-1].Body, "submitted change 5") {
		t.Errorf("known: submits %v, reply %q", sub.calls, h.ics[len(h.ics)-1].Body)
	}
}

// A push imported while who pushed it is unknown is not uploaded as the
// bridge (the change's for good): it waits, and is uploaded as its
// pusher once known.
func TestImportWaitsForWhoPushed(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	down := true
	im.Identity = downIdentity{im.Identity, &down}
	h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
	ref := "refs/heads/gerrit/" + ids[0]
	s.push("github.git", h, ref)
	s.pulls[10].Head.SHA = h
	im.Pushers.Record("o/r", ref, s.before("o/r", ref, h), h, github.User{Login: "austin", ID: 1})
	ctx := context.Background()
	if err := im.Import(ctx, "main"); !isIdentityUnknown(err) || len(up.uploads) != 0 {
		t.Fatalf("unknown: err %v, uploads %v", err, up.uploads)
	}
	if sts := s.statuses[h]; len(sts) == 0 || sts[len(sts)-1].State != "pending" {
		t.Errorf("unknown: gerrit/sync %+v, want pending", sts)
	}
	down = false
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], "austin ") {
		t.Errorf("known: uploads %v, want one as austin", up.uploads)
	}
}

// A /submit waiting for who asked spends none of its author's replies:
// twenty tries (ten minutes at identityRetry) left none, and it was
// recorded handled, silently, and never run (sixteenth review).
func TestSubmitWaitingSpendsNoReplies(t *testing.T) {
	c, h, sub := newCommands(t)
	c.Replies = &Budget{N: 20, Per: time.Hour}
	down := true
	c.Identity = downIdentity{c.Identity, &down}
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh", ID: 7}})
	ctx := context.Background()
	for range 21 {
		if err := c.Run(ctx, 5); !isIdentityUnknown(err) {
			t.Fatalf("unknown: err %v", err)
		}
	}
	if st, _ := c.Store.Load(ctx, 5); len(st.State.HandledCommands) != 0 {
		t.Errorf("unknown 21 times: handled %v", st.State.HandledCommands)
	}
	if left := c.Replies.Left(ctx, 7); left != 20 {
		t.Errorf("replies left %d, want 20", left)
	}
	down = false
	if err := c.Run(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 {
		t.Errorf("known: submits %v, want one", sub.calls)
	}
}

// A name Gerrit refuses to query (400, every time) is nobody, not unknown
// for as long as the entry stands; a 500 is unknown still (sixteenth
// review).
func TestDirectoryNameGerritCannotQueryIsNobody(t *testing.T) {
	s := &ldapSearcher{entries: []ldap.Entry{ldapEntry(`bad"name`, "Bad", "55")}}
	u, _ := ldapUsers(t, s)
	bad := github.User{Login: "Bad", ID: 55}
	u.Active = errAccounts{&gerrit.Error{Status: 400, Body: "line 1:10 mismatched character"}}
	if _, ok, err := u.Lookup(context.Background(), bad); ok || err != nil {
		t.Errorf("a query Gerrit refuses for good: %v %v, want nobody", ok, err)
	}
	u.Active = errAccounts{&gerrit.Error{Status: 500}}
	if _, ok, err := u.Lookup(context.Background(), bad); ok || !isIdentityUnknown(err) {
		t.Errorf("Gerrit failing: %v %v, want unknown", ok, err)
	}
}

// downFor is a mapping whose lookups of the logins in down cannot answer
// while *on, and are known's otherwise.
type downFor struct {
	known Identity
	down  map[string]bool
	on    *bool
}

func (d downFor) Lookup(ctx context.Context, gh github.User) (string, bool, error) {
	if *d.on && d.down[gh.Login] {
		return "", false, &IdentityUnknown{errors.New("the directory cannot be read")}
	}
	return d.known.Lookup(ctx, gh)
}

// A new GitHub-born PR whose author, or whose pusher, is unknown for now
// is not uploaded as the bridge (its change would be the bridge's for
// good): it waits, and is uploaded as its author once known. Sixteenth
// review: nothing tested firstUploader's waits.
func TestFirstUploadWaitsForWhoWroteAndPushedIt(t *testing.T) {
	for _, who := range []string{"austin", "austin-laptop"} {
		t.Run(who, func(t *testing.T) {
			s, im, up, m1, _, _ := gerritBornWorld(t, false)
			ctx := context.Background()
			on := true
			// austin pushes as another GitHub account of theirs, which the
			// mapping also gives austin.
			im.Identity = downFor{&Users{Overrides: map[string]string{"austin": "austin", "austin-laptop": "austin"}}, map[string]bool{who: true}, &on}
			h := s.commit(m1, "new.txt", "n\n", "New")
			s.push("github.git", h, "refs/heads/topic")
			s.pushed("o/r", "refs/heads/topic", h, "austin-laptop")
			s.pushLog["o/r refs/heads/topic"][0].before = m1
			s.pr(60, "topic", h, "main", "austin")
			if err := im.Import(ctx, "main"); !isIdentityUnknown(err) || len(up.uploads) != 0 {
				t.Fatalf("unknown: err %v, uploads %v", err, up.uploads)
			}
			if sts := s.statuses[h]; len(sts) == 0 || sts[len(sts)-1].State != "pending" {
				t.Errorf("unknown: gerrit/sync %+v, want pending", sts)
			}
			on = false
			if err := im.Import(ctx, "main"); err != nil {
				t.Fatal(err)
			}
			if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], "austin ") {
				t.Errorf("known: uploads %v, want one as austin", up.uploads)
			}
		})
	}
}

// A PR whose commits carry the Change-Id of its author's own change, while
// who the author is is unknown, is neither given a keyed Change-Id of its
// own (a second change, and the author's never linked) nor refused: it
// waits; once known, it is the author's to sort out, as ever. Sixteenth
// review: nothing tested this wait.
func TestOwnChangeIDWaitsForWhoTheAuthorIs(t *testing.T) {
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	ctx := context.Background()
	up.alone = true // as deployed: no peer key, so no other lookup of the author
	on := true
	im.Identity = downFor{im.Identity, map[string]bool{"austin": true}, &on}
	msg := "New\n\nChange-Id: " + cid(77) + "\n"
	h := s.commit(m1, "new.txt", "n\n", msg)
	s.push("github.git", h, "refs/heads/topic")
	s.pr(60, "topic", h, "main", "austin")
	s.pushed("o/r", "refs/heads/topic", h, "austin")
	ci := s.change(77, cid(77), s.commit(m1, "own.txt", "o\n", msg), msg, "NEW")
	ci.Owner = s.accounts["austin"]
	if err := im.Import(ctx, "main"); !isIdentityUnknown(err) || len(up.uploads) != 0 {
		t.Fatalf("unknown: err %v, uploads %v", err, up.uploads)
	}
	if sts := s.statuses[h]; len(sts) == 0 || sts[len(sts)-1].State != "pending" {
		t.Errorf("unknown: gerrit/sync %+v, want pending", sts)
	}
	on = false
	if err := im.Import(ctx, "main"); err != nil || len(up.uploads) != 0 {
		t.Fatalf("known: err %v, uploads %v", err, up.uploads)
	}
	if sts := s.statuses[h]; sts[len(sts)-1].State != "failure" {
		t.Errorf("known: gerrit/sync %+v, want the PR refused (the author's own change)", sts)
	}
}

// Where GitHub owns the branches, a /submit whose merger cannot say who
// asked (the directory, or their token's check) is not spent: no reply,
// not handled, and merged once known. Sixteenth review: only the Gerrit
// submit's wait was tested.
func TestMergeWaitsForWhoAsked(t *testing.T) {
	c, h, _, m := mergeCommands(t, "gerrit")
	m.err = &IdentityUnknown{errors.New("the directory cannot be read")}
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh", ID: 3}})
	ctx := context.Background()
	n := len(h.ics)
	if err := c.Run(ctx, 5); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if len(h.ics) != n {
		t.Fatalf("unknown: replied %v", h.ics[n:])
	}
	if st, _ := c.Store.Load(ctx, 5); len(st.State.HandledCommands) != 0 {
		t.Fatalf("unknown: handled %v", st.State.HandledCommands)
	}
	m.err = nil
	if err := c.Run(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if last := h.ics[len(h.ics)-1].Body; !strings.Contains(last, "merged on GitHub as you") {
		t.Errorf("known: reply %q", last)
	}
}

// A PR closed on GitHub by someone the bridge cannot place for now does
// not abandon the change as the bridge, nor post a notice: it waits, and
// abandons it as them once known. Sixteenth review: untested.
func TestCloseWaitsForWhoClosedIt(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "austin")
	on := true
	c.Identity = downFor{c.Identity, map[string]bool{"austin": true}, &on}
	ctx := context.Background()
	if _, err := c.mirrorPRStates(ctx, snap, map[int]bool{}); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if s.changes[7].Status != "NEW" || len(s.comments[70]) != 0 {
		t.Fatalf("unknown: status %s, notices %v", s.changes[7].Status, s.comments[70])
	}
	on = false
	if changed, err := c.mirrorPRStates(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("known: changed %v, err %v", changed, err)
	}
	if ci := s.changes[7]; ci.Status != "ABANDONED" || ci.Messages[len(ci.Messages)-1].Author.Username != "austin" {
		t.Errorf("known: status %s, messages %+v; want abandoned as austin", ci.Status, ci.Messages)
	}
}

// A PR's title edited by someone the bridge cannot place for now is not
// made a commit message as the bridge, nor refused: it waits, and is
// made as them once known. Sixteenth review: untested.
func TestPREditWaitsForWhoEditedIt(t *testing.T) {
	s, c, snap := editsWorld(t, "github")
	s.pulls[70].Title = "Seven, better"
	s.activity[70] = github.PRActivity{TitleEditor: github.User{Login: "austin"}, BodyEditor: github.User{Login: "austin"}}
	on := true
	c.Identity = downFor{c.Identity, map[string]bool{"austin": true}, &on}
	ctx := context.Background()
	if _, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if len(s.calls) != 0 || len(s.comments[70]) != 0 {
		t.Fatalf("unknown: calls %v, notices %v", s.calls, s.comments[70])
	}
	on = false
	if changed, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("known: changed %v, err %v", changed, err)
	}
	if len(s.calls) != 1 || s.calls[0] != "message 7 as austin" {
		t.Errorf("known: calls %v", s.calls)
	}
}

// A lost import's change owned by its PR's author is not taken for a
// Gerrit-born change (and given a second PR) while who the author is is
// unknown: markUnlinked waits. Sixteenth review: untested.
func TestUnlinkedImportWaitsForWhoTheAuthorIs(t *testing.T) {
	ctx := context.Background()
	sites := newSites(t)
	c, _ := sites.code(ctx)
	c.BridgeAccount = 1
	on := true
	c.Identity = downFor{&Users{Overrides: map[string]string{"austin": "austin"}}, map[string]bool{"austin": true}, &on}
	open := map[int]*github.PullRequest{7: {Number: 7, User: github.User{Login: "austin"}}}
	trailer := "T\n\nBody.\n\nGitHub-PR: https://github.com/o/r/pull/7\nChange-Id: I1\n"
	s := &Snapshot{State: map[int]*state.Change{}, Changes: map[int]*GerritChange{2: {Message: trailer, OwnerID: 5, Owner: "austin", PatchSet: 1}}}
	if err := c.markUnlinked(ctx, s, "o/r", open); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	on = false
	if err := c.markUnlinked(ctx, s, "o/r", open); err != nil || !s.Unlinked[2] {
		t.Errorf("known: err %v, unlinked %v", err, s.Unlinked)
	}
}

// A linked user's Gerrit +2 while whose links are is unknown for now is
// neither posted without its approval nor as the bridge: nothing is
// posted, and it becomes their review once known. Sixteenth review:
// untested.
func TestLinkedVoteWaitsForWhoseLinksAre(t *testing.T) {
	e, a := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2\n\nShip it.", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	a.unknown = &IdentityUnknown{errors.New("the directory cannot be read")}
	posts := e.h.posts
	if err := e.c.Sync(context.Background(), 5); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if e.h.posts != posts || len(e.h.reviews) != 0 {
		t.Fatalf("unknown: posted %d, reviews %+v", e.h.posts-posts, e.h.reviews)
	}
	a.unknown = nil
	e.sync(t)
	if len(e.h.reviews) != 1 || e.h.reviews[0].State != "APPROVED" || e.h.reviews[0].User != aliceGH {
		t.Errorf("known: reviews %+v, want alice's approval", e.h.reviews)
	}
}

// A linked user's copy on GitHub whose record was lost is not posted
// again while whose links are is unknown for now (its marker cannot be
// told theirs): the sync waits, and recovers it once known. Sixteenth
// review: recover's error was untested.
func TestRecoverWaitsForWhoseLinksAre(t *testing.T) {
	e, a := linkedEnv(t)
	e.g.user("c1", gerrit.PatchSetLevel, 2, 0, "One.", "", false)
	e.sync(t)
	if len(e.h.ics) != 1 || e.h.ics[0].User != aliceGH {
		t.Fatalf("posted %+v", e.h.ics)
	}
	updateStateFn(context.Background(), e.st, 5, "wipe", func(s *state.Change) { s.Comments, s.Threads = nil, nil })
	a.unknown = &IdentityUnknown{errors.New("the directory cannot be read")}
	if err := e.c.Sync(context.Background(), 5); !isIdentityUnknown(err) {
		t.Fatalf("unknown: err %v", err)
	}
	if len(e.h.ics) != 1 {
		t.Fatalf("unknown: posted again %+v", e.h.ics)
	}
	a.unknown = nil
	e.sync(t)
	if m := mapped(t, e); len(e.h.ics) != 1 || m["c1"].GitHubID != e.h.ics[0].ID {
		t.Errorf("known: mapped %+v, PR comments %+v", m["c1"], e.h.ics)
	}
}

// While who a reviewer is is unknown for now, their stale approval is not
// dismissed, and their dismissed approval's vote not withdrawn (as the
// bridge, or not at all): the sync waits, and does it once known.
// Sixteenth review: dismissStale's and dropDismissed's waits were
// untested.
func TestDismissalsWaitForWhoTheReviewerIs(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		e := newCommentsEnv(t)
		e.runAs()
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
		e.sync(t)
		on := true
		e.c.Users = downFor{e.c.Users, map[string]bool{"bob": true}, &on}
		e.g.vote("bobg", "Code-Review", -2)
		if err := e.c.Sync(context.Background(), 5); !isIdentityUnknown(err) || len(e.h.dismissed) != 0 {
			t.Fatalf("unknown: err %v, dismissed %v", err, e.h.dismissed)
		}
		on = false
		e.sync(t)
		if len(e.h.dismissed) != 1 {
			t.Errorf("known: dismissed %v", e.h.dismissed)
		}
	})
	t.Run("dismissed", func(t *testing.T) {
		e := newCommentsEnv(t)
		e.runAs()
		e.h.reviews = append(e.h.reviews, github.Review{ID: 70, State: "APPROVED", CommitID: "sha2", User: github.User{Login: "bob"},
			HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-70"})
		e.sync(t)
		on := true
		e.c.Users = downFor{e.c.Users, map[string]bool{"bob": true}, &on}
		e.h.dismiss(70, "maint", e.g.now())
		votes := len(e.g.votes)
		if err := e.c.Sync(context.Background(), 5); !isIdentityUnknown(err) || len(e.g.votes) != votes || e.g.labels["Code-Review"]["bobg"] != 2 {
			t.Fatalf("unknown: err %v, votes %v", err, e.g.votes[votes:])
		}
		on = false
		e.sync(t)
		if v := e.g.labels["Code-Review"]["bobg"]; v != 0 {
			t.Errorf("known: bobg's Code-Review = %d, want withdrawn", v)
		}
	})
}

// A thread resolved on GitHub by someone the bridge cannot place for now
// is not resolved in Gerrit as the bridge: it waits, and is resolved as
// them once known. Sixteenth review: syncResolution's wait was untested.
func TestResolutionWaitsForWhoResolvedIt(t *testing.T) {
	e := newCommentsEnv(t)
	e.runAs()
	e.g.user("c1", "a.txt", 2, 2, "Please fix.", "", true)
	e.sync(t)
	root := e.h.rcs[0].ID
	e.h.resolved[root] = true
	e.h.resolvedBy = map[int64]github.User{root: {Login: "bob", ID: 1}}
	on := true
	e.c.Users = downFor{e.c.Users, map[string]bool{"bob": true}, &on}
	comments := len(e.g.comments)
	if err := e.c.Sync(context.Background(), 5); !isIdentityUnknown(err) || len(e.g.comments) != comments {
		t.Fatalf("unknown: err %v, posted %+v", err, e.g.comments[comments:])
	}
	on = false
	e.sync(t)
	if gc := e.g.comments[len(e.g.comments)-1]; gc.Message != "Resolved on GitHub." || gc.Author.Username != "bobg" {
		t.Errorf("known: %+v, want resolved as bobg", gc)
	}
}
