package reconcile

// Gerrit-owned regressions: what the default direction (branch_owner:
// gerrit, as AOS runs it) does, kept so the GitHub-owned work can go on
// without changing it unseen (the owner's ask, 2026-11-21). Each test
// pins a regression found in review: what main (a01d401) did, which the
// branch broke, or, where a test says so, the rule the branch means to
// keep in place of main's (an intended change, docs/deploying.md), which
// a fix broke. Add one for every such find.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Fourteenth review: a stranger's Gerrit-born change the bridge mirrored to its
// own gerrit/* branch (every change is, where Gerrit owns the branches)
// is still a stranger's below a GitHub-born change. The branch's rule
// (main did not look below at all), which a fix broke.
func TestGerritOwnedMirroredStrangerBelowIsStillAStrangers(t *testing.T) {
	for _, owned := range []bool{false, true} {
		ctx := context.Background()
		s, c, base := followSites(t)
		c.GitHubOwned = owned
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
		low := s.commit(base, "low", "l\n", fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)))
		ci := s.change(1, cid(1), low, fmt.Sprintf("C1\n\nChange-Id: %s\n", cid(1)), "NEW")
		r := ci.Revisions[low]
		r.Uploader = gerrit.AccountInfo{AccountID: 9}
		ci.Revisions[low], ci.Owner = r, r.Uploader
		s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
			Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low}}})
		top := s.commit(low, "top", "t\n", fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)))
		ci = s.change(2, cid(2), top, fmt.Sprintf("C2\n\nChange-Id: %s\n", cid(2)), "NEW")
		r = ci.Revisions[top]
		r.Uploader = gerrit.AccountInfo{AccountID: 8}
		ci.Revisions[top], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
		s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
			HeadBranch: "feature", Lifecycle: "NEW"}})
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !snap.Foreign[2] {
			t.Errorf("GitHub owns %v: a stranger's mirrored change below is not foreign", owned)
		}
	}
}

// Fourteenth review: where Gerrit owns the branches, a PR shown merged into the
// target branch is replication's doing (only it may write main): the PR
// gets no "merged on GitHub, abandon it in Gerrit" notice.
//
// Fifteenth review: nor when GitHub credits it to the bridge's own login
// (replication pushing with the bridge's GitHub account, as the test
// instance's does): it said "stays open in Gerrit", on the PR and on the
// change.
func TestGerritOwnedReplicationMergeGetsNoNotice(t *testing.T) {
	for _, login := range []string{"replicator", "bot"} {
		s, c, _ := mergedSites(t, "One (#10)\n")
		c.GitHubOwned = false
		c.BridgeLogin = "bot"
		s.pulls[10].MergedBy = &github.User{Login: login}
		mirrorStates(t, c, false)
		for _, body := range s.comments[10] {
			if strings.Contains(body, "Gerrit owns this repository's branches") || strings.Contains(body, "Abandon") || strings.Contains(body, "stays open") {
				t.Errorf("merged by %s: notice on a replication merge: %q", login, body)
			}
		}
		for _, m := range s.changes[1].Messages {
			if strings.Contains(m.Message, "stays open") {
				t.Errorf("merged by %s: notice in Gerrit: %q", login, m.Message)
			}
		}
	}
}

// Fourteenth review: where Gerrit owns the branches, an author's fork PR above
// their own other PR (imported by the bridge) may have Gerrit's squash
// swapped in: the code below is the author's own. The branch's rule (on
// main the swap read only the uploader), which a fix broke.
func TestGerritOwnedSwapOfAnAuthorsOwnForkStack(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 6, true }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	low := s.commit(m0, "low.txt", "l\n", "Low\n\nChange-Id: "+cid(2)+"\n")
	ci := s.change(2, cid(2), low, "Low\n\nChange-Id: "+cid(2)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	r := ci.Revisions[low]
	r.Uploader = gerrit.AccountInfo{AccountID: 1} // the bridge's import of alice's PR
	ci.Revisions[low] = r
	s.save(ctx, store, 2, &state.Change{State: state.ChangeState{Change: 2, PR: 9, Branch: "main", Origin: "github", HeadBranch: "alice/r:low", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low, Vetted: true}}})
	sq := s.commit(low, "f.txt", "2\n", "Feature\n\nChange-Id: "+cid(1)+"\n")
	ci = s.change(1, cid(1), sq, "Feature\n\nChange-Id: "+cid(1)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	r = ci.Revisions[sq]
	r.Uploader = gerrit.AccountInfo{AccountID: 1}
	ci.Revisions[sq] = r
	st := &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "alice/r:feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: "1111111111111111111111111111111111111111"}}}
	full, err := c.Gerrit.GetChange(ctx, 1, "ALL_REVISIONS", "ALL_COMMITS", "DETAILED_ACCOUNTS")
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := full.Current()
	if err := c.maySwap(ctx, full, rev, st, nil); err != nil {
		t.Errorf("swap refused: %v", err)
	}
	// Into another fork, the lower PR's code is not its own: refused.
	st.State.HeadBranch = "mallory/r:feature"
	if err := c.maySwap(ctx, full, rev, st, nil); err == nil {
		t.Error("swapped another fork's import into a different fork")
	}
}

// Fifteenth review (passed on main): an author's own Gerrit-born change
// below their GitHub-born PR is theirs, not a stranger's: main pushed
// patch set on top, and the branch made it foreign. Each of the change
// below's patch sets must still be the owner's (or the trusted's): one a
// stranger uploaded there is not, and the pass is the owner's on their
// own changes only (those two are the branch's intended change, §7.1:
// main did not look below).
func TestGerritOwnedAuthorsOwnChangeBelow(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		lowerOwner, lowerUploader int
		foreign                   bool
	}{
		{"the author's own", 5, 5, false},
		{"a stranger's patch set on it", 5, 9, true},
		{"another's change, though the author uploaded it", 9, 5, true},
	} {
		ctx := context.Background()
		s := newSites(t)
		base := s.commit("", "a", "a\n", "Base")
		s.push("gerrit.git", base, "refs/heads/main")
		s.push("github.git", base, "refs/heads/main")
		c, _ := s.code(ctx)
		c.BridgeAccount = 1
		c.Trusted = func(_ context.Context, a int) (bool, bool) { return false, true }
		low := s.commit(base, "low", "l\n", "C1\n\nChange-Id: "+cid(1)+"\n")
		ci := s.change(1, cid(1), low, "C1\n\nChange-Id: "+cid(1)+"\n", "NEW")
		r := ci.Revisions[low]
		r.Uploader = gerrit.AccountInfo{AccountID: tc.lowerUploader}
		ci.Revisions[low], ci.Owner = r, gerrit.AccountInfo{AccountID: tc.lowerOwner}
		s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
			HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low}}})
		top := s.commit(low, "top", "t\n", "C2\n\nChange-Id: "+cid(2)+"\n")
		ci = s.change(2, cid(2), top, "C2\n\nChange-Id: "+cid(2)+"\n", "NEW")
		r = ci.Revisions[top]
		r.Uploader = gerrit.AccountInfo{AccountID: 5}
		ci.Revisions[top], ci.Owner = r, r.Uploader
		s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
			HeadBranch: "alice/r:feature", Lifecycle: "NEW"}})
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[2] != tc.foreign {
			t.Errorf("%s below: foreign %v, want %v", tc.name, snap.Foreign[2], tc.foreign)
		}
	}
}

// Fifteenth review (passed on main): state main wrote for Gerrit's own
// patch set found on a PR's branch (Importer.recordGerritHead: no Found
// field then), a stranger's patch set someone pushed there, stays foreign
// after the upgrade: its text never becomes the PR's description. The
// branch read the record as GitHub's own.
func TestGerritOwnedMainsRecordOfAFoundHeadStaysForeign(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, store := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 7, true }
	msg := fmt.Sprintf("Top\n\nChange-Id: %s\n", cid(2))
	p1 := s.commit(base, "top", "1\n", msg)
	p2 := s.commit(base, "top", "2\n", "Stranger's text\n\nChange-Id: "+cid(2)+"\n")
	ci := s.change(2, cid(2), p2, "Stranger's text\n\nChange-Id: "+cid(2)+"\n", "NEW")
	r2 := ci.Revisions[p2]
	r2.Number, r2.Ref, r2.Uploader = 2, "refs/changes/02/2/2", gerrit.AccountInfo{AccountID: 9}
	ci.Revisions = map[string]gerrit.RevisionInfo{
		p1: {Number: 1, Ref: "refs/changes/02/2/1", Kind: "REWORK", Uploader: gerrit.AccountInfo{AccountID: 1},
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, p1))},
		p2: r2,
	}
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	s.push("gerrit.git", p1, "refs/changes/02/2/1")
	s.push("gerrit.git", p2, "refs/changes/02/2/2")
	s.save(ctx, store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
		HeadBranch: "feature", Lifecycle: "NEW"}, Revisions: []state.Revision{
		{PatchSet: 1, GerritSHA: p1, GitHubSHA: "1111111111111111111111111111111111111111"},
		{PatchSet: 2, GerritSHA: p2, GitHubSHA: p2}, // "Gerrit's patch set 2 is on GitHub", as main records it
	}})
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Foreign[2] {
		t.Error("main's record of a found head lets a stranger's patch set become the PR's")
	}
}

// Sixteenth review: a bridge-owned import (alice's fork PR, alice
// unlinked) on top of another bridge-owned import (a stranger's PR the
// bridge uploaded) is foreign: the owner's pass below (c1a023d) is a
// person's, never the bridge's, or the stranger's code is pushed to
// alice's branch. The branch's rule (main read nothing below), which a
// fix broke.
func TestGerritOwnedBridgeImportBelowABridgeImport(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, _ := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return false, true }
	bridge := gerrit.AccountInfo{AccountID: 1}
	low := s.commit(base, "low", "stranger's\n", "C1\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), low, "C1\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[low]
	r.Uploader = bridge
	ci.Revisions[low], ci.Owner = r, bridge
	top := s.commit(low, "top", "t\n", "C2\n\nChange-Id: "+cid(2)+"\n")
	ci = s.change(2, cid(2), top, "C2\n\nChange-Id: "+cid(2)+"\n", "NEW")
	r = ci.Revisions[top]
	r.Uploader = bridge
	ci.Revisions[top], ci.Owner = r, bridge
	s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
		HeadBranch: "alice/r:feature", Lifecycle: "NEW"}})
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Foreign[2] {
		t.Error("a stranger's bridge import below a bridge-owned PR: not foreign (pushed to alice's branch)")
	}
}

// Sixteenth review, a regression (main pushed it): alice's own fork PR,
// imported as alice through Run-As (Gerrit says the bridge uploaded it
// on her behalf), below her own in-repo PR, is hers: her patch set is
// not foreign.
func TestGerritOwnedAuthorsOwnRunAsImportBelow(t *testing.T) {
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, _ := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return false, true }
	low := s.commit(base, "low", "alice's\n", "Low\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), low, "Low\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[low]
	r.Uploader, r.RealUploader = gerrit.AccountInfo{AccountID: 5}, &gerrit.AccountInfo{AccountID: 1}
	ci.Revisions[low], ci.Owner = r, gerrit.AccountInfo{AccountID: 5}
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
		HeadBranch: "alice/r:low", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: "1111111111111111111111111111111111111111", Vetted: true}}})
	msg := "Top\n\nChange-Id: " + cid(2) + "\n"
	p1 := s.commit(low, "top", "1\n", msg)
	p2 := s.commit(low, "top", "2\n", msg)
	ci = s.change(2, cid(2), p2, msg, "NEW")
	r2 := ci.Revisions[p2]
	r2.Number, r2.Ref, r2.Uploader, r2.Kind = 2, "refs/changes/02/2/2", gerrit.AccountInfo{AccountID: 5}, "REWORK"
	ci.Revisions = map[string]gerrit.RevisionInfo{
		p1: {Number: 1, Ref: "refs/changes/02/2/1", Kind: "REWORK", Uploader: gerrit.AccountInfo{AccountID: 5}, RealUploader: &gerrit.AccountInfo{AccountID: 1},
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, p1))},
		p2: r2,
	}
	ci.Owner = gerrit.AccountInfo{AccountID: 5}
	s.push("gerrit.git", p1, "refs/changes/02/2/1")
	s.push("gerrit.git", p2, "refs/changes/02/2/2")
	s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
		HeadBranch: "feature", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: p1, GitHubSHA: "2222222222222222222222222222222222222222", Vetted: true}}})
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Foreign[2] {
		t.Error("alice's patch set over her own Run-As import is foreign")
	}
}

// Sixteenth review, a regression (main swapped and retargeted it): /submit
// of a stack, a GitHub-born PR (brt's) on top of a Gerrit-born change of
// a registered user not in CI's trusted group, submitted together: Gerrit's
// squash is swapped into the PR's branch and the PR retargeted, so GitHub
// shows it merged when replication lands both. The change below is about
// to land too, chosen by whoever submits. One not in the submit is still
// a stranger's below.
func TestGerritOwnedSwapOverAnotherOwnersChangeSubmittedTogether(t *testing.T) {
	for _, together := range []bool{true, false} {
		t.Run(fmt.Sprint("together=", together), func(t *testing.T) { swapOverAnotherOwnersChange(t, together) })
	}
}

func swapOverAnotherOwnersChange(t *testing.T, together bool) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return false, true }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	// Change 2: carol's Gerrit-born change, mirrored to gerrit/<cid2>.
	low := s.commit(m0, "low.txt", "c\n", "Low\n\nChange-Id: "+cid(2)+"\n")
	ci := s.change(2, cid(2), low, "Low\n\nChange-Id: "+cid(2)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 8}
	r := ci.Revisions[low]
	r.Uploader = gerrit.AccountInfo{AccountID: 8}
	ci.Revisions[low] = r
	s.push("gerrit.git", low, "refs/changes/02/2/1")
	s.push("github.git", low, "refs/heads/gerrit/"+cid(2))
	s.pr(9, "gerrit/"+cid(2), low, "main", "carol")
	s.save(ctx, store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 9, Branch: "main", Origin: "gerrit",
		HeadBranch: "gerrit/" + cid(2), Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low, GitHubSHA: low}}})
	// Change 1: brt's PR on top of it, imported.
	h := s.commit(low, "f.txt", "2\n", "done")
	sq := s.commit(low, "f.txt", "2\n", "Feature\n\nChange-Id: "+cid(1)+"\n")
	ci = s.change(1, cid(1), sq, "Feature\n\nChange-Id: "+cid(1)+"\n", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 6}
	r = ci.Revisions[sq]
	r.Uploader = gerrit.AccountInfo{AccountID: 6}
	ci.Revisions[sq] = r
	s.push("gerrit.git", sq, "refs/changes/01/1/1")
	s.push("github.git", h, "refs/heads/feature")
	s.pr(10, "feature", h, "gerrit/"+cid(2), "brt")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
		HeadBranch: "feature", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: h}}})
	s.together[1] = []int{2, 1}
	if !together {
		// Carol's change is not part of this submit: her code below
		// is still a stranger's, and is not pushed to brt's branch.
		s.together[1] = []int{1}
	}
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !together {
		if got := s.ref("github.git", "refs/heads/feature"); got != h {
			t.Errorf("feature = %s, want brt's own %s: carol's change is not being submitted", short(got), short(h))
		}
		return
	}
	if got := s.ref("github.git", "refs/heads/feature"); got != sq {
		t.Errorf("feature = %s, want Gerrit's squash %s swapped in", short(got), short(sq))
	}
	if b := s.pulls[10].Base.Ref; b != "main" {
		t.Errorf("PR #10 based on %s, want retargeted to main", b)
	}
}

// Where Gerrit owns the branches, a push to a GitHub-born PR while CI's
// trusted group cannot say about the uploader of Gerrit's newer patch set
// does not wait, as on main: the patch set is taken for foreign, and the
// push is uploaded. Sixteenth review: the wait is GitHub-owned only.
func TestGerritOwnedUnsureTrustDoesNotWait(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	st, _ := im.Store.Load(ctx, 1)
	st.State.Origin, st.State.HeadBranch = "github", "feature"
	if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
		t.Fatal(err)
	}
	ci := s.changes[1]
	ci.Owner = s.accounts["austin"]
	bob := s.commit(g[0], "one.txt", "bob's\n", "One\n\nChange-Id: "+ids[0]+"\n")
	s.push("gerrit.git", bob, "refs/changes/01/1/2")
	ci.Revisions[bob] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2", Uploader: gerrit.AccountInfo{AccountID: 9, Username: "bob"},
		Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, bob))}
	ci.CurrentRevision = bob
	im.Trusted = func(_ context.Context, account int) (bool, bool) { return false, account != 9 }
	s.pulls[10].Head.Ref, s.pulls[10].User.Login = "feature", "austin"
	h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
	s.push("github.git", h, "refs/heads/feature")
	s.pulls[10].Head.SHA = h
	s.webhookPush(im.Pushers, 10, "austin")
	if err := im.Import(ctx, "main"); err != nil || len(up.uploads) != 1 {
		t.Errorf("err %v, uploads %v; want the push uploaded, as main", err, up.uploads)
	}
}

// A PR imported as the bridge says so on gerrit/sync. Where Gerrit owns
// the branches, with the advice main gave (link your GitHub account);
// where GitHub owns them, without it: who is who comes from the directory,
// and linking changes no owner. The second is the branch's rule.
func TestBridgeOwnedImportStatus(t *testing.T) {
	for _, owned := range []bool{false, true} {
		s, im, up, m1, _, _ := gerritBornWorld(t, false)
		up.alone, up.create = true, true // no peer key: every import is the bridge's
		im.link = func(ctx context.Context, n int, origin, branch, changeID string, pr *github.PullRequest) error {
			return updateStateFn(ctx, im.Store, n, "Link", func(st *state.Change) {
				st.State.Change, st.State.ChangeID, st.State.Branch, st.State.Origin, st.State.PR = n, changeID, branch, origin, pr.Number
			})
		}
		im.GitHubOwned = owned
		im.Publishable = func(context.Context, int) (bool, bool) { return true, true }
		h := s.commit(m1, "new.txt", "n\n", "New")
		s.push("github.git", h, "refs/heads/topic")
		s.pushed("o/r", "refs/heads/topic", h, "austin")
		s.pr(60, "topic", h, "main", "austin")
		if err := im.Import(context.Background(), "main"); err != nil {
			t.Fatal(err)
		}
		sts := s.statuses[h]
		if len(sts) == 0 {
			t.Fatalf("GitHub owns %v: no gerrit/sync", owned)
		}
		desc := sts[len(sts)-1].Description
		want := map[bool]string{false: "; owned by the bridge (link your GitHub account to own it)", true: "; owned by the bridge in Gerrit"}[owned]
		if !strings.Contains(desc, want) {
			t.Errorf("GitHub owns %v: gerrit/sync %q, want %q in it", owned, desc, want)
		}
	}
}

// Seventeenth review, a regression (main pushed it): unlinked alice's two
// stacked fork PRs, both the bridge's imports. Her lower PR's description
// edit made a message-only patch set the bridge uploaded, which GitHub
// never had; a trusted maintainer rebases her upper change onto it in
// Gerrit. Her own change below (the same fork's) is hers: not foreign.
// Another fork's bridge import below still is: that case is the branch's
// rule (main read nothing below).
func TestGerritOwnedUnlinkedAuthorsOwnBridgeImportBelow(t *testing.T) {
	// "alice/r" is a branch of the repository itself named like her fork
	// (eighteenth review: sameFork's ok on the change below was untested).
	for _, lowHead := range []string{"alice/r:low", "Alice/R:low", "mallory/r:low", "alice/r"} {
		t.Run(lowHead, func(t *testing.T) {
			unlinkedAuthorsOwnBelow(t, lowHead, strings.HasPrefix(strings.ToLower(lowHead), "alice/r:"))
		})
	}
}

func unlinkedAuthorsOwnBelow(t *testing.T, lowHead string, hers bool) {
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, _ := s.code(ctx)
	c.BridgeAccount = 1
	c.Trusted = func(_ context.Context, a int) (bool, bool) { return a == 7, true }
	bridge := gerrit.AccountInfo{AccountID: 1}
	lowMsg := "Low\n\nChange-Id: " + cid(1) + "\n"
	low1 := s.commit(base, "low", "alice's\n", lowMsg)
	low2 := s.commit(base, "low", "alice's\n", "Low, edited on GitHub\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), low2, lowMsg, "NEW")
	ci.Revisions = map[string]gerrit.RevisionInfo{
		low1: {Number: 1, Ref: "refs/changes/01/1/1", Kind: "REWORK", Uploader: bridge,
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, low1))},
		low2: {Number: 2, Ref: "refs/changes/01/1/2", Kind: "NO_CODE_CHANGE", Uploader: bridge,
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, low2))},
	}
	ci.Owner = bridge
	s.push("gerrit.git", low1, "refs/changes/01/1/1")
	s.push("gerrit.git", low2, "refs/changes/01/1/2")
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
		HeadBranch: lowHead, Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: low1, GitHubSHA: "1111111111111111111111111111111111111111", Vetted: true}}})
	msg := "Top\n\nChange-Id: " + cid(2) + "\n"
	p1 := s.commit(low1, "top", "1\n", msg)
	p2 := s.commit(low2, "top", "1\n", msg)
	ci = s.change(2, cid(2), p2, msg, "NEW")
	ci.Revisions = map[string]gerrit.RevisionInfo{
		p1: {Number: 1, Ref: "refs/changes/02/2/1", Kind: "REWORK", Uploader: bridge,
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, p1))},
		p2: {Number: 2, Ref: "refs/changes/02/2/2", Kind: "TRIVIAL_REBASE", Uploader: gerrit.AccountInfo{AccountID: 7},
			Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, p2))},
	}
	ci.Owner = bridge
	s.push("gerrit.git", p1, "refs/changes/02/2/1")
	s.push("gerrit.git", p2, "refs/changes/02/2/2")
	s.save(ctx, c.Store, 2, &state.Change{State: state.ChangeState{Change: 2, ChangeID: cid(2), PR: 20, Branch: "main", Origin: "github",
		HeadBranch: "alice/r:top", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: p1, GitHubSHA: "2222222222222222222222222222222222222222", Vetted: true}}})
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Foreign[2] == hers {
		t.Errorf("lower change from %s: foreign %v (main pushed alice's own)", lowHead, snap.Foreign[2])
	}
}

// Eighteenth review (regression guard): where Gerrit owns the branches
// GitHub's approvals are not the gate, and the owner's GitHub approval of
// their own change is their +2, dismissed by nothing, as on main. (This
// file's fakes are newer than main's: the test itself does not build there.)
func TestGerritOwnedOwnersGitHubApprovalVotes(t *testing.T) {
	bob := gerrit.AccountInfo{AccountID: 7, Name: "Bob G", Username: "bobg"}
	e := ownApproval(t, false, bob, map[int]gerrit.AccountInfo{1: bob, 2: bob})
	if len(e.h.dismissed) != 0 || len(e.g.votes) != 1 {
		t.Errorf("dismissed %v, Gerrit votes %v; want one vote, none dismissed", e.h.dismissed, e.g.votes)
	}
}

// Eighteenth review (regression, passes on main): a linked, budgeted
// user's vote-only +2 that is not posted (its commit is no longer the
// PR's head) is looked at again each sync; no look may spend their
// PostBudget, or their own words are deferred.
func TestGerritOwnedStaleVoteSpendsNoBudget(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	e.c.PostBudget = &Budget{N: 3, Per: time.Hour}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.h.head = "sha3"
	for i := 0; i < 4; i++ {
		e.sync(t)
	}
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m2", Author: alice, Message: "Patch Set 2:\n\nPlease also fix the docs.", RevisionNumber: 2})
	e.sync(t)
	if !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool { return strings.Contains(ic.Body, "Please also fix the docs.") }) {
		t.Errorf("alice's words deferred: her budget was spent on re-looking at an unposted vote (ics %d, reviews %+v)", len(e.h.ics), e.h.reviews)
	}
}

// Eighteenth review (regression guard, passes on main): a linked user's +2 on their own Gerrit-born
// change, on the PR's head and still their vote, is their GitHub
// approval, as on main (GitHub-owned's ownCode rule must not apply).
func TestGerritOwnedOwnChangeApprovalPosted(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.owner = alice
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2\n\nLGTM.", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.sync(t)
	if !slices.ContainsFunc(e.h.reviews, func(rv github.Review) bool {
		return rv.State == "APPROVED" && rv.User == aliceGH && rv.CommitID == "sha2"
	}) {
		t.Errorf("no approval as alice on sha2: %+v", e.h.reviews)
	}
}

// Eighteenth review (regression guard, passes on main): a vote the bridge cast as a linked user
// (Run-As: real_author is the bridge, as when a GitHub approval is
// mirrored) is the bridge's: never mirrored back as a GitHub review or
// comment, and not taken for a later vote of theirs that withdraws
// their own earlier +2.
func TestGerritOwnedBridgeRunAsVoteNotMirrored(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	bridge := gerrit.AccountInfo{AccountID: bridgeID, Name: "Bridge", Username: "bridge"}
	e.g.messages = []gerrit.ChangeMessage{
		{ID: "m1", Author: alice, RealAuthor: &bridge, Message: "Patch Set 2: Code-Review-1\n\nFrom GitHub.", RevisionNumber: 2},
	}
	e.sync(t)
	if len(e.h.reviews) != 0 || len(e.h.ics) != 0 {
		t.Errorf("the bridge's own Run-As vote was mirrored back: reviews %+v ics %+v", e.h.reviews, e.h.ics)
	}
}

// Eighteenth review (regression, passes on main): the same for a +2
// Gerrit no longer has (removed by the owner), vote-only.
func TestGerritOwnedRemovedVoteSpendsNoBudget(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	e.c.PostBudget = &Budget{N: 3, Per: time.Hour}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 0)
	for i := 0; i < 4; i++ {
		e.sync(t)
	}
	e.g.messages = append(e.g.messages, gerrit.ChangeMessage{ID: "m2", Author: alice, Message: "Patch Set 2:\n\nPlease also fix the docs.", RevisionNumber: 2})
	e.sync(t)
	if !slices.ContainsFunc(e.h.ics, func(ic github.IssueComment) bool { return strings.Contains(ic.Body, "Please also fix the docs.") }) {
		t.Errorf("alice's words deferred (ics %d, reviews %+v)", len(e.h.ics), e.h.reviews)
	}
}

// Eighteenth review (regression, passes on main): a linked user's +2 with words, not posted as an approval
// because the PR's head moved on (sixteenth review's rule), goes as a PR
// comment with her own token; it must name the patch set it was given on
// (docs/deploying.md: "its words are a comment naming the patch set"),
// not read as her +2 of the PR as it is now. Main: her approval of
// sha2's commit, which GitHub shows on that commit.
func TestGerritOwnedLateVoteCommentNamesItsPatchSet(t *testing.T) {
	e, _ := linkedEnv(t)
	e.runAs()
	e.c.Users = &Users{Overrides: map[string]string{"alice-gh": "alice"}}
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice Trusted", Username: "alice"}
	e.g.messages = []gerrit.ChangeMessage{{ID: "m1", Author: alice, Message: "Patch Set 2: Code-Review+2\n\nLGTM, ship it.", RevisionNumber: 2}}
	e.g.vote("alice", "Code-Review", 2)
	e.h.head = "sha3"
	e.sync(t)
	if slices.ContainsFunc(e.h.reviews, func(rv github.Review) bool { return rv.State == "APPROVED" && rv.CommitID == "sha2" }) {
		return // main: an approval GitHub shows on sha2
	}
	found := false
	for _, ic := range e.h.ics {
		if strings.Contains(ic.Body, "LGTM, ship it.") {
			found = true
			if !strings.Contains(strings.ToLower(ic.Body), "patch set 2") {
				t.Errorf("her +2 of patch set 2 posted as %s without naming it, on a PR now at sha3: %q", ic.User.Login, ic.Body)
			}
		}
	}
	if !found {
		t.Errorf("her words were not posted: %+v", e.h.ics)
	}
}

// Nineteenth review (regression guard, as on main): where Gerrit owns
// the branches Gerrit is the gate, and a GitHub change request whose -1
// Gerrit no longer has (not copied to a new patch set) is dismissed.
func TestGerritOwnedChangeRequestWithoutItsVoteIsDismissed(t *testing.T) {
	alice := gerrit.AccountInfo{AccountID: 1, Name: "Alice", Username: "alice"}
	e := changeRequested(t, false, alice)
	e.g.revs["sha3"] = 3
	e.g.current = "sha3"
	e.g.vote("bobg", "Code-Review", 0)
	e.pushed(t, "sha3", e.g.now().Add(-time.Hour))
	e.sync(t)
	if len(e.h.dismissed) != 1 {
		t.Errorf("dismissed %v, want bob's change request", e.h.dismissed)
	}
}
