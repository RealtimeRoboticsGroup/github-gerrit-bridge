package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// failingOpener is an owner's linked account whose PR creation GitHub
// refuses (an App without pull-request write, SSO not authorized, a
// rate limit).
type failingOpener struct{}

func (failingOpener) CreatePull(context.Context, github.NewPull) (*github.PullRequest, error) {
	return nil, errors.New("403 Resource not accessible by integration")
}

// Round 21: where GitHub owns the branches, a Gerrit-born change's PR is
// opened as its owner (ac9af12). When that fails, the bridge's
// gerrit/<Change-Id> branch is pushed already, and anyone who can read the
// repository may open a PR from it: the bridge then adopts that PR (plan
// prFor by head, Code.link) as the change's, opened by a stranger, not
// its owner -- the very thing OpenAs is for.
func TestStrangersPROnBridgeBranchNotAdopted(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	c, store := s.code(ctx)
	c.GitHubOwned = true
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	c.OpenAs = func(context.Context, int) (PullOpener, bool, error) { return failingOpener{}, true, nil }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	msg := "Feature\n\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(m0, "f.txt", "f\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 8}
	ci.Revisions[rev], ci.Owner = r, gerrit.AccountInfo{AccountID: 8}

	_ = c.Reconcile(ctx, Key{Branch: "main"}) // the owner's CreatePull fails
	head := "gerrit/" + cid(1)
	if s.gitOut("github.git", "rev-parse", "--verify", "-q", "refs/heads/"+head) == "" {
		t.Skip("branch not pushed before the PR; nothing to adopt")
	}
	// A stranger opens a PR from the bridge's branch.
	s.pr(10, head, rev, "main", "mallory")
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	if n := store.ChangeForPR(10); n == 1 {
		t.Errorf("change 1 (owner 8) adopted @mallory's PR #10 as its own: its PR is not its owner's")
	}
}

// Kills A1 (ac9af12 says "PRs that exist already are left as they are"):
// a change whose PR exists is not withheld because its owner has no
// linked account; only one with no PR yet waits for its owner.
func TestExistingPRNotWithheldForItsOwner(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	msg := "One\n\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(base, "one.txt", "1\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 8, Username: "eight"}                    // linked
	ci.Revisions[rev], ci.Owner = r, gerrit.AccountInfo{AccountID: 9, Username: "nine"} // not linked
	head := "gerrit/" + cid(1)
	s.push("github.git", rev, "refs/heads/"+head)
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
		HeadBranch: head, Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: rev, GitHubSHA: rev}}})
	s.pr(10, head, rev, "main", "bot")
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Withheld[1] {
		t.Error("a change whose PR exists (opened as the bot before) is withheld because its owner is not linked")
	}
	// No PR yet: it waits for its owner.
	s2, c2, base2 := followSites(t)
	c2.Publishable = c.Publishable
	rev2 := s2.commit(base2, "one.txt", "1\n", msg)
	ci2 := s2.change(1, cid(1), rev2, msg, "NEW")
	r2 := ci2.Revisions[rev2]
	r2.Uploader = gerrit.AccountInfo{AccountID: 8, Username: "eight"}
	ci2.Revisions[rev2], ci2.Owner = r2, gerrit.AccountInfo{AccountID: 9, Username: "nine"}
	snap2, err := c2.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap2.Withheld[1] {
		t.Error("a change with no PR whose owner is not linked is not withheld")
	}
}

// Round 21: a PR's import whose link was lost (a crash, a refused state
// push: Importer.ownUpload adopts it later) is the bridge's change, with
// no record yet. withhold runs before markUnlinked, takes it for a
// Gerrit-born change with no PR and an owner (the bridge) who has no
// linked account, and tells its PR's author, in Gerrit, to link: the
// bridge's account can never link.
func TestLostLinkImportGetsNoOwnerNotice(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.BridgeAccount, c.BridgeUser = 99, "bridge"
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	msg := "Topic\n\nGitHub-PR: https://github.com/o/r/pull/7\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(base, "one.txt", "1\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 99, Username: "bridge"}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	s.push("github.git", rev, "refs/heads/topic")
	s.pr(7, "topic", rev, "main", "stranger")
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	for _, m := range s.reviewMsgs[1] {
		t.Errorf("notice on a lost link's import: %q", m)
	}
	if len(s.pulls) != 1 {
		t.Errorf("PRs %d, want only #7", len(s.pulls))
	}
}

// Kills A5: whether a change's owner may push cannot be told (GitHub's
// collaborator API failing, the directory down): it is withheld, silently,
// and no PR is opened, as no one.
func TestOwnerUnknownWithholds(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	c, _ := s.code(ctx)
	c.GitHubOwned = true
	c.Publishable = func(_ context.Context, a int) (bool, bool) {
		if a == 9 {
			return false, false
		}
		return a == 8, true
	}
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	msg := "Feature\n\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(m0, "f.txt", "f\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 8}
	ci.Revisions[rev], ci.Owner = r, gerrit.AccountInfo{AccountID: 9}
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	if len(s.pulls) != 0 || len(s.reviewMsgs[1]) != 0 {
		t.Errorf("PRs %d, notices %q: want it withheld, unsaid", len(s.pulls), s.reviewMsgs[1])
	}
	if got := s.gitOut("github.git", "rev-parse", "--verify", "-q", "refs/heads/gerrit/"+cid(1)); got != "" {
		t.Errorf("its branch was pushed (%s) though its owner's link is unknown", got)
	}
}

// Kills A9: a change whose recorded PR is gone (GitHub 404s it) is
// planned a new PR without withhold's owner check, which runs only while
// no PR is recorded. Where GitHub owns the branches it is still never
// opened as the bot, even with no way to open it as anyone.
func TestRecordedPRGoneNotReopenedAsBot(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	c.OpenAs = nil
	msg := "One\n\nChange-Id: " + cid(1) + "\n"
	rev := s.commit(base, "one.txt", "1\n", msg)
	ci := s.change(1, cid(1), rev, msg, "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 8, Username: "eight"}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
		HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}})
	_ = c.Reconcile(ctx, Key{Branch: "main"})
	if len(s.pulls) != 0 {
		for _, pr := range s.pulls {
			t.Errorf("PR #%d opened as %q with no owner to open it as", pr.Number, pr.User.Login)
		}
	}
}

// Kills D2: only a message that IS Gerrit's removal text withdraws a +2;
// bob's own review quoting it ("Patch Set 1:\n\nRemoved Code-Review+2 by
// <GERRIT_ACCOUNT_1>") is no removal of alice's vote.
func TestQuotedRemovalKeepsApproval(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob", Username: "bobg"}
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	e.h.head = "sha1"
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, Message: "Patch Set 1: Code-Review+2", RevisionNumber: 1},
		{ID: "m2", Author: bob, Message: "Patch Set 1:\n\nRemoved Code-Review+2 by <GERRIT_ACCOUNT_1>", RevisionNumber: 1},
	}
	e.sync(t)
	approved := false
	for _, rv := range e.h.reviews {
		approved = approved || (rv.State == "APPROVED" && rv.User == aliceGH)
	}
	if !approved {
		t.Errorf("bob's comment quoting a removal withdrew alice's +2: reviews %+v", e.h.reviews)
	}
}

// Kills G5: a merge into another branch by the PR's author, who has no
// Gerrit account, abandons as the bridge only a change the bridge owns
// for them; not alice's change whose PR someone else opened (one opened
// before OpenAs, or one adopted by its head).
func TestMergeElsewhereByStrangerKeepsOwnersChange(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	c.GitHubOwned, c.BridgeUser = true, "bridge"
	s.changes[1].Owner = gerrit.AccountInfo{AccountID: 5, Username: "alice"}
	s.pulls[10].Base.Ref = "scratch"
	s.pulls[10].User = github.User{Login: "stranger"}
	s.pulls[10].MergedBy = &github.User{Login: "stranger"}
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got == "ABANDONED" {
		t.Errorf("a stranger's merge of their PR abandoned alice's change as the bridge")
	}
}
