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

// Twenty-first review: a PR someone else opened from the bridge's branch for a
// Gerrit-born change (its commit is anyone's to fetch from Gerrit) is
// taken for the change's PR, so GitHub knows it as theirs, not the
// owner's, and counts the owner's approval: what OpenAs is for.
func TestSomeoneElsesPRFromTheBridgeBranchIsNotAdopted(t *testing.T) {
	for _, author := range []string{"mallory", "eight-gh", "bot"} {
		t.Run(author, func(t *testing.T) { prFromTheBridgeBranch(t, author, author != "mallory") })
	}
}

// prFromTheBridgeBranch: change 1, owned by 8 (linked as eight-gh), and an
// open PR by author from its branch, unlinked; adopted says whether it is
// taken for its PR.
func prFromTheBridgeBranch(t *testing.T, author string, adopted bool) {
	ctx := context.Background()
	s := newSites(t)
	c, store := s.code(ctx)
	c.LinkedAs = func(_ context.Context, a int) (github.User, bool, error) {
		return github.User{Login: "eight-gh"}, a == 8, nil
	}
	c.GitHubOwned = true
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	msg := "Feature\n\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(m0, "f.txt", "f\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 8}
	ci.Revisions[rev], ci.Owner = r, gerrit.AccountInfo{AccountID: 8}
	// Mallory, before the bridge: the change's commit, on the branch the
	// bridge would push it to, and a PR of her own from it.
	head := HeadBranch("main", "main", cid(1))
	s.push("github.git", rev, "refs/heads/"+head)
	s.pr(50, head, rev, "main", author)
	_ = c.Reconcile(ctx, Key{Branch: "main"}) // mallory's PR makes GitHub refuse the owner's: only that change waits
	st, err := store.Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if (st.State.PR == 50) != adopted {
		t.Errorf("change 1 (owner 8) linked to #%d, want #50 %v: opened by @%s; opened as owner: %v", st.State.PR, adopted, author, s.openedBy)
	}
}
