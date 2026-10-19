package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

type fakeSubmitter struct {
	calls  []string // "user@rev"
	err    error
	cannot string // CanSubmitAs refuses with this
	// rev is the current patch set, uploaded at uploaded.
	rev      string
	uploaded time.Time
}

func (f *fakeSubmitter) Current(context.Context, int) (string, time.Time, error) {
	if f.rev == "" {
		return "sha2", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	return f.rev, f.uploaded, nil
}

func (f *fakeSubmitter) CanSubmitAs(_ context.Context, user string, change int, rev string) (bool, string, error) {
	return f.cannot == "", f.cannot, nil
}

func (f *fakeSubmitter) SubmitAs(_ context.Context, user string, change int, rev string) error {
	f.calls = append(f.calls, user)
	if f.rev != "" {
		f.calls[len(f.calls)-1] += "@" + rev
	}
	return f.err
}

func newCommands(t *testing.T) (*Commands, *fakeGitHub, *fakeSubmitter) {
	e := newCommentsEnv(t) // change 5 linked to PR 1, lifecycle NEW
	// Both patch sets the tests submit are on GitHub.
	st, err := e.st.Load(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	st.Revisions = []state.Revision{{PatchSet: 2, GerritSHA: "sha2", GitHubSHA: "sha2"}, {PatchSet: 3, GerritSHA: "sha3", GitHubSHA: "gh3"}}
	if err := e.st.Save(context.Background(), 5, st, "revisions"); err != nil {
		t.Fatal(err)
	}
	sub := &fakeSubmitter{}
	return &Commands{GitHub: e.h, Submitter: sub, Store: e.st, Identity: &Users{Overrides: map[string]string{"AustinSchuh": "alice"}},
		BridgeLogin: "bridge", Logger: slog.Default(), Now: func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }}, e.h, sub
}

func TestSubmitCommandRunsOnce(t *testing.T) {
	c, h, sub := newCommands(t)
	h.ics = append(h.ics,
		github.IssueComment{ID: 1, Body: "Looks good.\n/submit\n", User: github.User{Login: "AustinSchuh"}},
		github.IssueComment{ID: 2, Body: "please don't /submit yet", User: github.User{Login: "AustinSchuh"}})
	ctx := context.Background()
	for range 3 {
		if err := c.Run(ctx, 5); err != nil {
			t.Fatal(err)
		}
	}
	if len(sub.calls) != 1 || sub.calls[0] != "alice" {
		t.Errorf("submits = %v, want one as alice", sub.calls)
	}
	last := h.ics[len(h.ics)-1].Body
	if !strings.Contains(last, "submitted change 5") || !strings.Contains(last, "<!-- bridge:cmd/1 -->") {
		t.Errorf("reply = %q", last)
	}
}

// Anyone with write access can edit the bridge's replies: changing a
// reply's marker neither runs its command again nor, put on another
// reply, keeps a pending one from running.
func TestEditedRepliesDoNotDecideWhatWasHandled(t *testing.T) {
	c, h, sub := newCommands(t)
	ctx := context.Background()
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)})
	if err := c.Run(ctx, 5); err != nil {
		t.Fatal(err)
	}
	r := len(h.ics) - 1 // the bridge's reply
	h.ics[r].Body = strings.Replace(h.ics[r].Body, "cmd/1", "cmd/99", 1) + "\n\n<!-- bridge:cmd/3 -->"
	h.edits = map[int64]string{h.ics[r].ID: "mallory"}
	h.ics = append(h.ics, github.IssueComment{ID: 3, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)})
	if err := c.Run(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 2 {
		t.Errorf("submits = %v, want #1 once and #3 once", sub.calls)
	}
}

// One commenter's /submit is answered at most so often: each answer is a
// comment written with the bridge's token. The rest are recorded as
// handled, unanswered.
func TestSubmitAnswersAreBounded(t *testing.T) {
	c, h, _ := newCommands(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c.Replies = &Budget{N: 2, Per: time.Hour, now: func() time.Time { return now }}
	for i := int64(1); i <= 4; i++ {
		h.ics = append(h.ics, github.IssueComment{ID: i, Body: "/submit", User: github.User{Login: "stranger", ID: 99}})
	}
	before := len(h.ics)
	for range 2 {
		if err := c.Run(context.Background(), 5); err != nil {
			t.Fatal(err)
		}
	}
	if answers := len(h.ics) - before; answers != 2 {
		t.Errorf("%d answers, want 2", answers)
	}
	// The unanswered ones are not run later, once there is room.
	now = now.Add(2 * time.Hour)
	if err := c.Run(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if answers := len(h.ics) - before; answers != 2 {
		t.Errorf("%d answers after the budget refilled, want still 2", answers)
	}
}

func TestSubmitCommandUnmappedAndRefused(t *testing.T) {
	c, h, sub := newCommands(t)
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "stranger"}})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 0 || !strings.Contains(h.ics[len(h.ics)-1].Body, "needs a linked Gerrit account") {
		t.Errorf("unmapped user: calls=%v reply=%q", sub.calls, h.ics[len(h.ics)-1].Body)
	}

	sub.err = &gerrit.Error{Status: 409, Body: "Change 5: needs Verified"}
	h.ics = append(h.ics, github.IssueComment{ID: 7, Body: "/submit", User: github.User{Login: "AustinSchuh"}})
	c.Run(context.Background(), 5)
	if got := h.ics[len(h.ics)-1].Body; !strings.Contains(got, "> Change 5: needs Verified") {
		t.Errorf("refusal reply = %q", got)
	}
}

func TestSubmitPreparesPRsFirst(t *testing.T) {
	c, h, sub := newCommands(t)
	var order []string
	c.BeforeSubmit = func(_ context.Context, n int) error {
		order = append(order, "prepare")
		if len(sub.calls) != 0 {
			t.Error("submitted before preparing the PRs")
		}
		return nil
	}
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}})
	c.Run(context.Background(), 5)
	if len(order) != 1 || len(sub.calls) != 1 {
		t.Errorf("prepare=%v submits=%v", order, sub.calls)
	}
}

func TestSubmitCommandAbuse(t *testing.T) {
	c, h, sub := newCommands(t)
	prepared := 0
	c.BeforeSubmit = func(context.Context, int) error { prepared++; return nil }
	created := time.Unix(1700000000, 0)
	h.ics = append(h.ics,
		// Someone edited /submit into the maintainer's old comment.
		github.IssueComment{ID: 1, Body: "LGTM\n/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: created, UpdatedAt: created.Add(time.Hour)},
		// A forged "handled" marker from someone else does not hide a command.
		github.IssueComment{ID: 2, Body: "/submit", User: github.User{Login: "AustinSchuh"}},
		github.IssueComment{ID: 3, Body: "<!-- bridge:cmd/2 -->", User: github.User{Login: "mallory"}})
	h.edits = map[int64]string{1: "mallory"}
	c.Run(context.Background(), 5)
	if len(sub.calls) != 1 {
		t.Errorf("submits = %v, want one (comment 2)", sub.calls)
	}
	if !strings.Contains(h.ics[3].Body, "was edited") {
		t.Errorf("reply to the edited comment = %q", h.ics[3].Body)
	}
	// When Gerrit would refuse, PRs are left alone.
	sub.cannot = "Needs Verified"
	h.ics = append(h.ics, github.IssueComment{ID: 9, Body: "/submit", User: github.User{Login: "AustinSchuh"}})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 1 || prepared != 1 {
		t.Errorf("submits=%v prepared=%d after a refusal", sub.calls, prepared)
	}
	if got := h.ics[len(h.ics)-1].Body; !strings.Contains(got, "Needs Verified") {
		t.Errorf("refusal reply = %q", got)
	}
}

// A bridge comment carrying someone's words (a mirrored Gerrit reply)
// cannot mark a /submit handled: only a marker at the end, as the
// bridge's replies end, counts.
func TestHandledMarkerOnlyAtTheEnd(t *testing.T) {
	c, h, sub := newCommands(t)
	h.ics = append(h.ics,
		github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}},
		github.IssueComment{ID: 2, Body: "**Mallory** says <!-- bridge:cmd/1 --> hi\n\n<!-- bridge:gerrit/5/x -->", User: github.User{Login: "bridge"}})
	if err := c.Run(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 {
		t.Errorf("submits = %v, want the /submit run", sub.calls)
	}
}

// Errors that are not Gerrit's refusals stay in the log: they can name
// internal hosts.
func TestSubmitErrorsDoNotLeakDetails(t *testing.T) {
	c, h, _ := newCommands(t)
	c.Submitter.(*fakeSubmitter).err = errors.New("Post \"http://gerrit.internal:8080/a/changes/5/submit\": dial tcp: refused")
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}})
	c.Run(context.Background(), 5)
	if got := h.ics[len(h.ics)-1].Body; strings.Contains(got, "gerrit.internal") || !strings.Contains(got, "see its log") {
		t.Errorf("reply = %q", got)
	}
}

// /submit is for the patch set its author saw.
func TestSubmitCommandIsForThePatchSetItsAuthorSaw(t *testing.T) {
	c, h, sub := newCommands(t)
	posted := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sub.rev, sub.uploaded = "sha3", posted.Add(time.Minute) // pushed after the comment
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: posted})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 0 || !strings.Contains(h.ics[len(h.ics)-1].Body, "a patch set newer than your comment") {
		t.Errorf("calls=%v reply=%q", sub.calls, h.ics[len(h.ics)-1].Body)
	}
	// Posted after the patch set: that revision is what is submitted.
	sub.uploaded = posted.Add(-time.Minute)
	h.ics = append(h.ics, github.IssueComment{ID: 2, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: posted})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 1 || sub.calls[0] != "alice@sha3" {
		t.Errorf("calls = %v, want alice submitting sha3", sub.calls)
	}
}

func TestOldSubmitCommandsExpire(t *testing.T) {
	c, h, sub := newCommands(t)
	posted := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sub.rev, sub.uploaded = "sha3", posted.Add(-time.Hour)
	c.Now = func() time.Time { return posted.Add(maxCommandAge + time.Minute) }
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: posted})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 0 || !strings.Contains(h.ics[len(h.ics)-1].Body, "old. Post it again") {
		t.Errorf("calls=%v reply=%q", sub.calls, h.ics[len(h.ics)-1].Body)
	}
}

// /submit is for a patch set its author could have seen: one on GitHub
// for a minute when they wrote it.
func TestSubmitNeedsThePatchSetOnGitHub(t *testing.T) {
	c, h, sub := newCommands(t)
	posted := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sub.rev, sub.uploaded = "sha4", posted.Add(-time.Minute) // uploaded in Gerrit, not exported yet
	h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: posted})
	c.Run(context.Background(), 5)
	if len(sub.calls) != 0 || !strings.Contains(h.ics[len(h.ics)-1].Body, "was not on GitHub when you wrote it") {
		t.Errorf("calls=%v reply=%q", sub.calls, h.ics[len(h.ics)-1].Body)
	}
	// Nor one pushed to GitHub moments before the comment (fourth audit).
	for _, tc := range []struct {
		before time.Duration
		ran    bool
		old    bool // recorded before At was kept: Gerrit's upload time stands in
	}{{30 * time.Second, false, false}, {2 * time.Minute, true, false}, {30 * time.Second, false, true}} {
		c, h, sub := newCommands(t)
		sub.rev, sub.uploaded = "sha3", posted.Add(-time.Hour)
		st, _ := c.Store.Load(context.Background(), 5)
		st.Revisions[1].At = posted.Add(-tc.before)
		if tc.old {
			st.Revisions[1].At, sub.uploaded = time.Time{}, posted.Add(-tc.before)
		}
		if err := c.Store.Save(context.Background(), 5, st, "at"); err != nil {
			t.Fatal(err)
		}
		c.Now = func() time.Time { return posted.Add(time.Minute) }
		h.ics = append(h.ics, github.IssueComment{ID: 1, Body: "/submit", User: github.User{Login: "AustinSchuh"}, CreatedAt: posted})
		c.Run(context.Background(), 5)
		if ran := len(sub.calls) != 0; ran != tc.ran {
			t.Errorf("on GitHub %v before: ran %v, reply %q", tc.before, ran, h.ics[len(h.ics)-1].Body)
		}
	}
}
