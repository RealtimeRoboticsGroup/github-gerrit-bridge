package reconcile

import (
	"context"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// refusingOpener is an owner's token GitHub refuses (403: no access for
// this token, an SSO session ended, a secondary rate limit).
type refusingOpener struct{}

func (refusingOpener) CreatePull(context.Context, github.NewPull) (*github.PullRequest, error) {
	return nil, &github.APIError{Method: "POST", Path: "/repos/o/r/pulls", Status: 403, Message: "Resource not accessible by integration"}
}

// Twenty-first review: one owner whose linked token GitHub refuses must not stop
// every other change on the branch getting its PR.
func TestOneOwnersRefusedTokenStopsOnlyTheirChange(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	c, _ := s.code(ctx)
	c.GitHubOwned = true
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8 || a == 7, true }
	ok := c.OpenAs
	c.OpenAs = func(ctx context.Context, account int) (PullOpener, bool, error) {
		if account == 8 {
			return refusingOpener{}, true, nil
		}
		return ok(ctx, account)
	}
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	for i, owner := range []int{8, 7} {
		n := i + 1
		msg := "Feature\n\nChange-Id: " + cid(n) + "\n"
		rev := s.commit(m0, "f.txt", "f\n", msg)
		ci := s.change(n, cid(n), rev, msg, "NEW")
		r := ci.Revisions[rev]
		r.Uploader = gerrit.AccountInfo{AccountID: owner}
		ci.Revisions[rev], ci.Owner = r, gerrit.AccountInfo{AccountID: owner}
	}
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	opened := false
	for _, who := range s.openedBy {
		opened = opened || who == 7
	}
	if !opened {
		t.Errorf("change 2 (owner 7, linked, fine) got no PR because change 1's owner's token was refused: opened %v", s.openedBy)
	}
}
