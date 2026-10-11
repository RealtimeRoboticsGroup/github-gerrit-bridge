package reconcile

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

type fakeSubmitter struct {
	calls  []string
	err    error
	cannot string // CanSubmitAs refuses with this
}

func (f *fakeSubmitter) CanSubmitAs(_ context.Context, user string, change int) (bool, string, error) {
	return f.cannot == "", f.cannot, nil
}

func (f *fakeSubmitter) SubmitAs(_ context.Context, user string, change int) error {
	f.calls = append(f.calls, user)
	return f.err
}

func newCommands(t *testing.T) (*Commands, *fakeGitHub, *fakeSubmitter) {
	e := newCommentsEnv(t) // change 5 linked to PR 1, lifecycle NEW
	sub := &fakeSubmitter{}
	return &Commands{GitHub: e.h, Submitter: sub, Store: e.st, Identity: &Users{Overrides: map[string]string{"AustinSchuh": "alice"}},
		BridgeLogin: "bridge", Logger: slog.Default()}, e.h, sub
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
