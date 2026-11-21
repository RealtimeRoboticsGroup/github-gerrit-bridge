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
