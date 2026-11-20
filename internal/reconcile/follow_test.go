package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// followSites is sites whose main is the same commit on both sides, and a
// Code where GitHub owns the branches.
func followSites(t *testing.T) (*sites, *Code, string) {
	t.Helper()
	ctx := context.Background()
	s := newSites(t)
	base := s.commit("", "a", "a\n", "Base")
	s.push("gerrit.git", base, "refs/heads/main")
	s.push("github.git", base, "refs/heads/main")
	c, _ := s.code(ctx)
	c.GitHubOwned = true
	return s, c, base
}

// Design §3.1: what merges on GitHub reaches Gerrit's branch.
func TestFollowCopiesGitHubsBranch(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	merged := s.commit(base, "b", "b\n", "Squashed on GitHub (#7)\n\nChange-Id: I0000000000000000000000000000000000000001")
	s.push("github.git", merged, "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != merged {
		t.Errorf("Gerrit's main = %s, want GitHub's %s", got, merged)
	}
	// In sync: nothing more to do.
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
}

// The owner's decision: a force-push on GitHub is followed, not refused.
func TestFollowForcePush(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	ahead := s.commit(base, "b", "b\n", "On both")
	s.push("github.git", ahead, "refs/heads/main")
	s.push("gerrit.git", ahead, "refs/heads/main")
	rewritten := s.commit(base, "c", "c\n", "Force-pushed over it")
	s.push("github.git", rewritten, "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != rewritten {
		t.Errorf("Gerrit's main = %s, want GitHub's force-pushed %s", got, rewritten)
	}
}

// Gerrit's branches are its own where Gerrit owns them.
func TestFollowOnlyWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.GitHubOwned = false
	s.push("github.git", s.commit(base, "b", "b\n", "On GitHub"), "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != base {
		t.Errorf("Gerrit's main = %s, want it left at %s", got, base)
	}
}

// Only branches both sides have: none is made or removed in Gerrit, and
// the bridge's own GitHub branches are never target branches.
func TestFollowOnlyBranchesBothHave(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	other := s.commit(base, "b", "b\n", "Elsewhere")
	s.push("github.git", other, "refs/heads/feature")
	s.push("gerrit.git", other, "refs/heads/release")
	s.push("gerrit.git", base, "refs/heads/gerrit/x")
	s.push("github.git", other, "refs/heads/gerrit/x")
	for _, b := range []string{"feature", "release", "gerrit/x"} {
		if err := c.Follow(ctx, b); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	if got := s.gitOut("gerrit.git", "rev-parse", "--verify", "--quiet", "refs/heads/feature"); got != "" {
		t.Errorf("GitHub's feature branch was made in Gerrit: %s", got)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != other {
		t.Errorf("Gerrit's release = %s, want it kept (GitHub has none)", got)
	}
	if got := s.ref("gerrit.git", "refs/heads/gerrit/x"); got != base {
		t.Errorf("a bridge branch was followed: %s", got)
	}
}

func TestFollowDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.DryRun = true
	s.push("github.git", s.commit(base, "b", "b\n", "On GitHub"), "refs/heads/main")
	if err := c.Follow(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != base {
		t.Errorf("dry run moved Gerrit's main to %s", got)
	}
}

// A branch's reconcile follows GitHub first, so what it plans is from
// what merged there.
func TestReconcileFollowsGitHubFirst(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	merged := s.commit(base, "b", "b\n", "Merged on GitHub")
	s.push("github.git", merged, "refs/heads/main")
	if err := c.Reconcile(ctx, Key{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/main"); got != merged {
		t.Errorf("Gerrit's main = %s, want GitHub's %s", got, merged)
	}
}

// Nothing replicates a Gerrit merge to GitHub where GitHub owns the
// branch, so no PR is reshaped for one.
func TestPrepareSubmitLeavesPRsWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	store := c.Store
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	g1 := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: I1\n")
	h1 := s.commit(g1, "one.txt", "1b\n", "Patchset 2")
	g1b := s.commit(base, "one.txt", "1b\n", "One\n\nChange-Id: I1\n")
	ci := s.change(1, "I1", g1b, "One\n\nChange-Id: I1\n", "NEW")
	ci.Revisions[g1b] = gerritRev(ci.Revisions[g1b], 2)
	s.push("gerrit.git", g1b, ci.Revisions[g1b].Ref)
	s.together[1] = []int{1}
	s.push("github.git", h1, "refs/heads/gerrit/I1")
	pr := s.pr(10, "gerrit/I1", h1, "gerrit-base/x", "bot")
	pr.Stack = &github.PRStack{Number: 30}
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/I1", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 2, GerritSHA: g1b, GitHubSHA: h1}}})
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/I1"); got != h1 {
		t.Errorf("gerrit/I1 = %s, want its history head %s left", got, h1)
	}
	if len(s.unstacks) != 0 || s.pulls[10].Base.Ref != "gerrit-base/x" {
		t.Errorf("unstacked %v, base %s: want the PR left as it is", s.unstacks, s.pulls[10].Base.Ref)
	}
}

// The plan learns who owns the branch from what the reconcile observed.
func TestObserveSaysGitHubOwns(t *testing.T) {
	ctx := context.Background()
	_, c, _ := followSites(t)
	for _, owned := range []bool{true, false} {
		c.GitHubOwned = owned
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if snap.GitHubOwned != owned {
			t.Errorf("GitHubOwned %v: snapshot says %v", owned, snap.GitHubOwned)
		}
	}
}

// mergedSites is followSites with change 1 open in Gerrit, and its PR #10
// merged on GitHub as merged, a commit whose message is msg.
func mergedSites(t *testing.T, msg string) (*sites, *Code, string) {
	t.Helper()
	ctx := context.Background()
	s, c, base := followSites(t)
	rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	s.may[1] = []string{"bridge"}
	s.save(ctx, c.Store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "gerrit",
		HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"}})
	merged := s.commit(base, "one.txt", "1\n", msg)
	s.push("github.git", merged, "refs/heads/main")
	pr := s.pr(10, "gerrit/"+cid(1), rev, "main", "bot")
	pr.State, pr.Merged, pr.MergeCommitSHA, pr.MergedBy = "closed", true, merged, &github.User{Login: "alice"}
	return s, c, merged
}

// mirrorStates follows GitHub's main, observes it and mirrors the PRs'
// states, as a reconcile's first pass does.
func mirrorStates(t *testing.T, c *Code, follow bool) {
	t.Helper()
	ctx := context.Background()
	if follow {
		if err := c.Follow(ctx, "main"); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.mirrorPRStates(ctx, snap, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
}

// Design §3.1: a PR merged on GitHub whose commit Gerrit did not take as
// the change's (its Change-Id edited out, or co-author lines after it)
// leaves the change open in Gerrit: the bridge marks it merged at that
// commit (Gerrit's check-and-fix), saying where it merged.
func TestMergedOnGitHubIsMarkedMerged(t *testing.T) {
	s, c, merged := mergedSites(t, "One (#10)\n\nChange-Id: "+cid(1)+"\n\n---------\n\nCo-authored-by: Bob <bob@example.com>\n")
	s.maintainServer = true
	mirrorStates(t, c, true)
	if ci := s.changes[1]; ci.Status != "MERGED" || ci.CurrentRevision != merged {
		t.Fatalf("change 1 is %s at %s, want MERGED at %s", ci.Status, ci.CurrentRevision, merged)
	}
	want := "Merged on GitHub by @\u200dalice in PR #10, as " + merged + " on main."
	if msgs := s.reviewMsgs[1]; len(msgs) == 0 || msgs[len(msgs)-1] != want {
		t.Errorf("notices %q, want %q", msgs, want)
	}
}

// Without Maintain Server, Gerrit will not: the change is abandoned, the
// note saying where it merged.
func TestMergedOnGitHubIsAbandonedIfGerritWillNot(t *testing.T) {
	s, c, merged := mergedSites(t, "One (#10)\n\nThe trailer was edited out.\n")
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "ABANDONED" {
		t.Fatalf("change 1 is %s, want ABANDONED", got)
	}
	want := "Merged on GitHub by @\u200dalice in PR #10, as " + merged + " on main, but Gerrit could not show it merged."
	if !strings.HasPrefix(s.actionMsgs[1], want) {
		t.Errorf("message %q, want it to start %q", s.actionMsgs[1], want)
	}
}

// A PR merged into another branch (a stacked PR into the one below) does
// not merge as its change: abandoned, saying where it went, at once.
func TestMergedOnGitHubIntoAnotherBranchIsAbandoned(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	s.maintainServer = true
	s.pulls[10].Base.Ref = "gerrit/I2"
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "ABANDONED" || !strings.Contains(s.actionMsgs[1], "PR #10 into gerrit/I2, not main") {
		t.Errorf("change 1 is %s: %q", got, s.actionMsgs[1])
	}
}

// Not before the merge is on Gerrit's branch (Gerrit may yet close it as
// merged), nor where Gerrit owns the branch.
func TestMergedOnGitHubWaitsForItsMerge(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("not followed yet: change 1 is %s", got)
	}
	if err := c.Follow(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	c.GitHubOwned = false
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("Gerrit owns main: change 1 is %s", got)
	}
	c.GitHubOwned = true
	mirrorStates(t, c, false)
	if got := s.changes[1].Status; got != "ABANDONED" {
		t.Errorf("merged and followed: change 1 is %s", got)
	}
}

// GitHub's word for the merge commit reaches git only as a full SHA: a
// ref name there would name whatever it names.
func TestMergedOnGitHubNeedsAMergeCommitSHA(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	s.pulls[10].MergeCommitSHA = "refs/remotes/gerrit/heads/main"
	mirrorStates(t, c, true)
	if got := s.changes[1].Status; got != "NEW" {
		t.Errorf("change 1 is %s, abandoned for a merge commit named by ref", got)
	}
}

// A reconcile abandons it and then plans from what Gerrit says now: the
// merged PR is left alone, not reopened for a change it thought open.
func TestMergedOnGitHubReconcile(t *testing.T) {
	s, c, _ := mergedSites(t, "One (#10)\n")
	if err := c.Reconcile(context.Background(), Key{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got, pr := s.changes[1].Status, s.pulls[10]; got != "ABANDONED" || pr.State != "closed" || !pr.Merged {
		t.Errorf("change 1 is %s, PR #10 %s merged %v; want abandoned, and the PR merged", got, pr.State, pr.Merged)
	}
}

// Where GitHub owns the branches, who may have Gerrit patch sets pushed to
// a GitHub-born PR's branch is who may push to the repository themselves
// (Code.Publishable), not CI's trusted group: the importer agrees with
// markForeign, and never restores (Gerrit wins) the code of a trusted
// member whose GitHub account may not push.
func TestImporterForeignIsByPublishable(t *testing.T) {
	for _, tc := range []struct {
		publishable, owner bool
		restored           bool
	}{{false, false, false}, {true, false, true}, {false, true, false}} {
		publishable := tc.publishable
		s, im, up, m1, g, ids := gerritBornWorld(t, false)
		ctx := context.Background()
		im.Trusted = func(_ context.Context, account int) (bool, bool) { return account == 9, true }
		im.Publishable = func(_ context.Context, account int) (bool, bool) { return publishable && account == 9, true }
		uploader := gerrit.AccountInfo{AccountID: 9, Username: "trusted"}
		if tc.owner {
			// The owner's own: no pass on the repository's own branch.
			uploader = s.accounts["austin"]
		}
		st, _ := im.Store.Load(ctx, 1)
		st.State.Origin, st.State.HeadBranch = "github", "feature"
		if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
			t.Fatal(err)
		}
		ci := s.changes[1]
		ci.Owner = s.accounts["austin"]
		first := ci.Revisions[g[0]]
		first.Kind, first.Uploader = "REWORK", s.accounts["austin"]
		ci.Revisions[g[0]] = first
		code := s.commit(g[0], "one.txt", "theirs\n", "Their message\n\nChange-Id: "+ids[0]+"\n")
		s.push("gerrit.git", code, "refs/changes/01/1/2")
		ci.Revisions[code] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2",
			Uploader: uploader, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, code))}
		ci.CurrentRevision = code
		s.pulls[10].Head.Ref, s.pulls[10].User.Login = "feature", "austin"
		h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
		s.push("github.git", h, "refs/heads/feature")
		s.pulls[10].Head.SHA = h
		s.webhookPush(im.Pushers, 10, "austin")
		if err := im.Import(ctx, "main"); err != nil {
			t.Fatal(err)
		}
		restored := false
		for _, u := range up.uploads {
			if c := strings.Fields(u)[0]; strings.HasPrefix(u, " ") && s.git("m.git", "rev-parse", c+"^{tree}") == s.git("work", "rev-parse", code+"^{tree}") {
				restored = true
			}
		}
		if restored != tc.restored {
			t.Errorf("%+v: their code restored %v (uploads %v)", tc, restored, up.uploads)
		}
	}
}

// markForeign asks the same where GitHub owns the branches, and nobody
// is publishable without a way to tell.
func TestForeignIsByPublishableWhereGitHubOwns(t *testing.T) {
	ctx := context.Background()
	nobody := func(context.Context, int) (bool, bool) { return false, true }
	for _, tc := range []struct {
		owned       bool
		publishable func(context.Context, int) (bool, bool)
		uploader    int    // 5 is the owner
		head        string // the PR's
		foreign     bool
	}{
		{false, nil, 7, "feature", false}, // CI's trusted group decides
		{true, nobody, 7, "feature", true},
		{true, func(context.Context, int) (bool, bool) { return true, true }, 7, "feature", false},
		{true, nil, 7, "feature", true},
		// The owner's own upload: no pass on the repository's branch
		// (they may have lost write access); on their fork's, theirs.
		{false, nil, 5, "feature", false},
		{true, nobody, 5, "feature", true},
		{true, nobody, 5, "someone/r:feature", false},
	} {
		s := newSites(t)
		c, _ := s.code(ctx)
		c.DryRun = true // no notices
		c.GitHubOwned, c.Publishable = tc.owned, tc.publishable
		c.Trusted = func(_ context.Context, account int) (bool, bool) { return account == 7, true }
		snap := &Snapshot{Changes: map[int]*GerritChange{1: {Number: 1, Status: "NEW", Revision: "r1", PatchSet: 1,
			OwnerID: 5, UploaderID: tc.uploader, Kind: "REWORK"}},
			State: map[int]*state.Change{1: {State: state.ChangeState{Change: 1, Origin: "github", HeadBranch: tc.head}}}}
		if err := c.markForeign(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[1] != tc.foreign {
			t.Errorf("%+v: foreign %v, want %v", tc, snap.Foreign[1], tc.foreign)
		}
	}
}

// Design §3.1: where GitHub owns the branches, a Gerrit-born change
// reaches GitHub only if whoever uploaded it, and made its code, has a
// linked GitHub account that may push there: a pushed branch runs the
// repository's workflows with its secrets. The rest are withheld, with a
// notice once per patch set saying how to fix it.
func TestGerritBornNeedsPublishableUploaders(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, owned bool, publishable func(context.Context, int) (bool, bool)) (*sites, *Code, *gerrit.ChangeInfo) {
		s, c, base := followSites(t)
		c.GitHubOwned, c.Publishable, c.LinkURL = owned, publishable, "https://bridge.example.com/github/link"
		rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
		ci := s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
		r := ci.Revisions[rev]
		// Its owner too: owning the change is no pass.
		r.Uploader = gerrit.AccountInfo{AccountID: 7, Username: "seven"}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		return s, c, ci
	}
	only := func(who int, known bool) func(context.Context, int) (bool, bool) {
		return func(_ context.Context, a int) (bool, bool) { return a == who, known }
	}
	withheld := func(t *testing.T, c *Code) bool {
		t.Helper()
		snap, err := c.observe(ctx, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Withheld[1]
	}

	s, c, _ := setup(t, true, only(8, true))
	if !withheld(t, c) || !withheld(t, c) {
		t.Fatal("an unlinked uploader's change is not withheld")
	}
	if msgs := s.reviewMsgs[1]; len(msgs) != 1 || !strings.Contains(msgs[0], "Link yours at https://bridge.example.com/github/link") {
		t.Errorf("notices %q, want one saying where to link", msgs)
	}
	if _, c, _ := setup(t, true, only(7, true)); withheld(t, c) {
		t.Error("a publishable uploader's change is withheld")
	}
	if _, c, _ := setup(t, false, only(8, true)); withheld(t, c) {
		t.Error("withheld where Gerrit owns the branches")
	}
	if s, c, _ := setup(t, true, only(7, false)); !withheld(t, c) || len(s.reviewMsgs[1]) != 0 {
		t.Errorf("cannot tell: withheld %v, notices %q; want withheld quietly", withheld(t, c), s.reviewMsgs[1])
	}
	if _, c, _ := setup(t, true, nil); !withheld(t, c) {
		t.Error("no way to tell who may push, and not withheld")
	}

	// Someone publishable editing the message of an unlinked upload: the
	// code is still the unlinked one's.
	s, c, ci := setup(t, true, only(8, true))
	first := ci.Revisions[ci.CurrentRevision]
	first.Kind = "REWORK"
	ci.Revisions[ci.CurrentRevision] = first
	edit := s.commit(strings.TrimSpace(s.gitOut("work", "rev-parse", ci.CurrentRevision+"^")), "one.txt", "1\n", "One, better\n\nChange-Id: "+cid(1)+"\n")
	s.push("gerrit.git", edit, "refs/changes/01/1/2")
	ci.Revisions[edit] = gerrit.RevisionInfo{Number: 2, Kind: "NO_CODE_CHANGE", Ref: "refs/changes/01/1/2",
		Uploader: gerrit.AccountInfo{AccountID: 8, Username: "eight"}, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, edit))}
	ci.CurrentRevision = edit
	if !withheld(t, c) {
		t.Error("a publishable member's message edit of an unlinked upload is not withheld")
	}
}

// Where GitHub owns the branches, a stranger's withheld patch set of a
// Gerrit-born change is not restored over a push to its PR ("Gerrit
// wins"): restored, it would be the bridge's upload, which the gate lets
// through to GitHub. A publishable uploader's is restored, as before.
func TestGerritWinsNeverRestoresUnpublishedCode(t *testing.T) {
	for _, tc := range []struct{ publishable, owner bool }{{false, false}, {true, false}, {false, true}} {
		publishable := tc.publishable
		s, im, up, m1, g, ids := gerritBornWorld(t, false)
		ctx := context.Background()
		im.Publishable = func(_ context.Context, account int) (bool, bool) {
			return account == 5 || (publishable && account == 9), true
		}
		ci := s.changes[1]
		ci.Owner = s.accounts["austin"]
		if tc.owner { // owning the change is no pass
			ci.Owner = gerrit.AccountInfo{AccountID: 9, Username: "stranger"}
		}
		first := ci.Revisions[g[0]]
		first.Kind = "REWORK"
		ci.Revisions[g[0]] = first
		code := s.commit(g[0], "one.txt", "theirs\n", "Their message\n\nChange-Id: "+ids[0]+"\n")
		s.push("gerrit.git", code, "refs/changes/01/1/2")
		ci.Revisions[code] = gerrit.RevisionInfo{Number: 2, Kind: "REWORK", Ref: "refs/changes/01/1/2",
			Uploader: gerrit.AccountInfo{AccountID: 9, Username: "stranger"}, Commit: commitInfo(s.gitOut("work", "log", "-1", "--format="+commitInfoFormat, code))}
		ci.CurrentRevision = code
		// Someone with write access pushes to the PR's branch meanwhile.
		h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
		s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
		s.pulls[10].Head.SHA = h
		s.webhookPush(im.Pushers, 10, "austin")
		if err := im.Import(ctx, "main"); err != nil {
			t.Fatal(err)
		}
		restored := false
		for _, u := range up.uploads {
			if c := strings.Fields(u)[0]; strings.HasPrefix(u, " ") && s.git("m.git", "rev-parse", c+"^{tree}") == s.git("work", "rev-parse", code+"^{tree}") {
				restored = true
			}
		}
		if restored != publishable {
			t.Errorf("stranger publishable %v, owner %v: their code restored %v (uploads %v)", publishable, tc.owner, restored, up.uploads)
		}
	}
}

// Gerrit refusing GitHub's branch (a commit validator, say) does not stop
// the rest of the branch: changes are still mirrored, and the reconcile
// fails, to be tried again.
func TestRefusedFollowStillMirrorsTheBranch(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(context.Context, int) (bool, bool) { return true, true }
	hook := filepath.Join(s.dir, "gerrit.git", "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do [ \"$ref\" = refs/heads/main ] && { echo 'missing Signed-off-by' >&2; exit 1; }; done\nexit 0\n"), 0o755)
	s.push("github.git", s.commit(base, "b", "b\n", "Merged on GitHub"), "refs/heads/main")
	rev := s.commit(base, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), rev, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[rev]
	r.Uploader = gerrit.AccountInfo{AccountID: 7, Username: "seven"}
	r.Commit.Parents = []struct {
		Commit string `json:"commit"`
	}{{base}}
	ci.Revisions[rev], ci.Owner = r, r.Uploader
	if err := c.Reconcile(ctx, Key{Branch: "main"}); err == nil || !strings.Contains(err.Error(), "following GitHub's main") {
		t.Errorf("err = %v, want the refused follow", err)
	}
	if st, _ := c.Store.Load(ctx, 1); st.State.PR == 0 {
		t.Error("the change got no PR: the refused follow stopped the branch")
	}
}

// Only the default branch follows a force-push. Anyone with write access
// can make a branch on GitHub: one named like a branch Gerrit has (an
// old release) does not replace it; once it builds on Gerrit's, it is
// followed.
func TestFollowOtherBranchesOnlyForward(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	release := s.commit(base, "r", "r\n", "Release")
	s.push("gerrit.git", release, "refs/heads/release")
	other := s.commit(base, "x", "x\n", "Someone's branch")
	s.push("github.git", other, "refs/heads/release")
	if err := c.Follow(ctx, "release"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != release {
		t.Errorf("Gerrit's release = %s, replaced by an unrelated GitHub branch", got)
	}
	forward := s.commit(release, "r2", "r2\n", "Release fix")
	s.push("github.git", forward, "refs/heads/release")
	if err := c.Follow(ctx, "release"); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("gerrit.git", "refs/heads/release"); got != forward {
		t.Errorf("Gerrit's release = %s, want GitHub's %s", got, forward)
	}
}

// Where GitHub owns the branches, a change based on a withheld one is
// withheld too, its children as well: its PR (or frozen base) would bring
// the withheld code to GitHub. A notice says why, once.
func TestChildrenOfWithheldChangesAreWithheld(t *testing.T) {
	ctx := context.Background()
	for _, owned := range []bool{true, false} {
		s := newSites(t)
		c, store := s.code(ctx)
		c.GitHubOwned = owned
		snap := &Snapshot{Changes: map[int]*GerritChange{
			1: {Number: 1, Status: "NEW", Revision: "r1", Parent: "m0", PatchSet: 1},
			2: {Number: 2, Status: "NEW", Revision: "r2", Parent: "r1", PatchSet: 1},
			3: {Number: 3, Status: "NEW", Revision: "r3", Parent: "r2", PatchSet: 1},
			4: {Number: 4, Status: "NEW", Revision: "r4", Parent: "f1", PatchSet: 1},
			5: {Number: 5, Status: "NEW", Revision: "f1", Parent: "m0", PatchSet: 1},
			6: {Number: 6, Status: "NEW", Revision: "r6", Parent: "m0", PatchSet: 1},
		}, State: map[int]*state.Change{}, Withheld: map[int]bool{1: true}, Foreign: map[int]bool{5: true}}
		for n := range snap.Changes {
			s.change(n, cid(n), s.commit("", fmt.Sprint(n), "x\n", "x"), "x", "NEW")
		}
		if err := c.withholdChildren(ctx, snap); err != nil {
			t.Fatal(err)
		}
		want := map[int]bool{1: true}
		if owned {
			want = map[int]bool{1: true, 2: true, 3: true, 4: true}
		}
		if !reflect.DeepEqual(snap.Withheld, want) {
			t.Errorf("GitHub owns %v: withheld %v, want %v", owned, snap.Withheld, want)
		}
		if owned {
			if msgs := s.reviewMsgs[3]; len(msgs) != 1 || !strings.Contains(msgs[0], "based on change 2") {
				t.Errorf("change 3's notices %q", msgs)
			}
			for n := range snap.Changes {
				snap.State[n], _ = store.Load(ctx, n)
			}
			snap.Withheld = map[int]bool{1: true}
			c.withholdChildren(ctx, snap)
			if msgs := s.reviewMsgs[3]; len(msgs) != 1 {
				t.Errorf("notice again: %q", msgs)
			}
		}
	}
}

// A reconcile's look at the branch does it: a publishable member's change
// on top of an unlinked one's is withheld with it.
func TestObserveWithholdsChildrenOfWithheld(t *testing.T) {
	ctx := context.Background()
	s, c, base := followSites(t)
	c.Publishable = func(_ context.Context, a int) (bool, bool) { return a == 8, true }
	add := func(n int, parent string, who int) string {
		rev := s.commit(parent, fmt.Sprint(n), "x\n", fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)))
		ci := s.change(n, cid(n), rev, fmt.Sprintf("C%d\n\nChange-Id: %s\n", n, cid(n)), "NEW")
		r := ci.Revisions[rev]
		r.Uploader = gerrit.AccountInfo{AccountID: who}
		ci.Revisions[rev], ci.Owner = r, r.Uploader
		return rev
	}
	add(2, add(1, base, 7), 8)
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Withheld[1] || !snap.Withheld[2] {
		t.Errorf("withheld %v, want both", snap.Withheld)
	}
}
