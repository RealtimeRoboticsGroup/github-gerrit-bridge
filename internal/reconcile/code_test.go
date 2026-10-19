package reconcile

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// prStates is a change whose PR someone closed or reopened on GitHub,
// with its snapshot as Code.observe would make it.
func prStatesWorld(t *testing.T, status, prState string) (*sites, *Code, *Snapshot) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.change(7, "I7", m0, "Seven\n\nChange-Id: I7\n", status)
	s.may[7] = []string{"austin"}
	pr := s.pr(70, "gerrit/I7", m0, "main", "bot")
	pr.State = prState
	s.save(ctx, store, 7, &state.Change{State: state.ChangeState{Change: 7, PR: 70, Branch: "main", Origin: "gerrit", Lifecycle: status}})
	snap := &Snapshot{Branch: "main", DefaultBranch: "main",
		Changes: map[int]*GerritChange{7: {Number: 7, ChangeID: "I7", Status: status, URL: "https://g/c/p/+/7"}},
		PRs:     map[int]*github.PullRequest{70: pr}, State: map[int]*state.Change{}}
	st, err := store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	snap.State[7] = st
	return s, c, snap
}

func TestClosingOnGitHubAbandonsAsTheCloser(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "austin")
	changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	ci := s.changes[7]
	if !changed || ci.Status != "ABANDONED" || ci.Messages[len(ci.Messages)-1].Author.Username != "austin" {
		t.Errorf("changed=%v status=%s messages=%+v; want abandoned as austin", changed, ci.Status, ci.Messages)
	}
	if len(s.comments[70]) != 0 {
		t.Errorf("notice posted for a close that worked: %v", s.comments[70])
	}
}

func TestReopeningOnGitHubRestores(t *testing.T) {
	s, c, snap := prStatesWorld(t, "ABANDONED", "open")
	s.gerritStatus(7, "ABANDONED", "austin")
	s.ghEvent(70, "reopened", "austin")
	if changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{}); err != nil || !changed || s.changes[7].Status != "NEW" {
		t.Errorf("changed=%v err=%v status=%s; want restored", changed, err, s.changes[7].Status)
	}
}

func TestCloseGerritRefusesIsExplainedAndUndone(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "brt") // brt may not abandon change 7
	changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if s.changes[7].Status != "NEW" {
		t.Errorf("status = %s", s.changes[7].Status)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "Gerrit would not let brt abandon it (abandon not permitted). Reopening it.") {
		t.Errorf("notice = %v", s.comments[70])
	}
	// The plan then reopens it.
	if p := MakePlan(planSnapshot(snap)); !hasAction(p, UpdatePR, func(a Action) bool { return a.Update.State != nil && *a.Update.State == "open" }) {
		t.Errorf("plan does not reopen the PR: %v", p.Actions)
	}
}

func TestCloseByUserWithoutGerritAccountIsUndone(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "stranger")
	if changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{}); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "@stranger has no Gerrit account to abandon it with") {
		t.Errorf("notice = %v", s.comments[70])
	}
}

func TestBridgesOwnCloseIsLeftToThePlan(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "bot")
	if changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{}); err != nil || changed || s.changes[7].Status != "NEW" || len(s.comments[70]) != 0 {
		t.Errorf("changed=%v err=%v status=%s comments=%v; want nothing done", changed, err, s.changes[7].Status, s.comments[70])
	}
}

// A change restored in Gerrit after its PR was closed on GitHub: the old
// close must not abandon it again (Gerrit is newer).
func TestRestoreInGerritBeatsAnOlderGitHubClose(t *testing.T) {
	s, c, snap := prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "austin")
	s.gerritStatus(7, "ABANDONED", "austin") // the close, mirrored
	s.gerritStatus(7, "NEW", "austin")       // then restored in Gerrit
	if changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{}); err != nil || changed || s.changes[7].Status != "NEW" {
		t.Errorf("changed=%v err=%v status=%s; want the restore kept", changed, err, s.changes[7].Status)
	}
	// ...and a close after the restore counts again.
	s.ghEvent(70, "closed", "austin")
	if changed, _ := c.mirrorPRStates(context.Background(), snap, map[int]bool{}); !changed || s.changes[7].Status != "ABANDONED" {
		t.Errorf("a new close was ignored: status %s", s.changes[7].Status)
	}
}

// planSnapshot fills in what MakePlan needs beyond mirrorPRStates.
func planSnapshot(s *Snapshot) *Snapshot {
	s.GitHubBranches = map[string]string{}
	s.OnBranch = map[string]bool{}
	s.OnGitHubBranch = map[string]bool{}
	s.SameTree, s.HasParent, s.NoPush = map[int]string{}, map[int]bool{}, map[int]bool{}
	for n, c := range s.Changes {
		c.Revision, c.Parent, c.PatchSet = "r"+c.ChangeID, "m0", 1
		s.GitHubBranches[s.head(n, c.ChangeID)] = c.Revision
		s.State[n].Revisions = []state.Revision{{PatchSet: 1, GerritSHA: c.Revision, GitHubSHA: c.Revision}}
	}
	s.OnBranch["m0"] = true
	return s
}

func hasAction(p *Plan, kind ActionKind, ok func(Action) bool) bool {
	for _, a := range p.Actions {
		if a.Kind == kind && ok(a) {
			return true
		}
	}
	return false
}

// Design §6.7: before /submit, a Gerrit-born PR whose head is its
// patch-set history gets Gerrit's commit swapped in, leaves its stack and
// is retargeted, so GitHub shows it merged when replication lands it.
func TestPrepareSubmitSwapsInGerritsCommit(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	g1 := s.commit(m0, "one.txt", "1\n", "One\n\nChange-Id: I1\n")
	g2 := s.commit(g1, "two.txt", "2\n", "Two\n\nChange-Id: I2\n")
	h2 := s.commit(g2, "two.txt", "2b\n", "Patch set 2 from Gerrit") // GitHub history of change 2
	g2b := s.commit(g1, "two.txt", "2b\n", "Two\n\nChange-Id: I2\n") // Gerrit's patch set 2
	s.change(1, "I1", g1, "One\n\nChange-Id: I1\n", "NEW")
	ci2 := s.change(2, "I2", g2b, "Two\n\nChange-Id: I2\n", "NEW")
	ci2.Revisions[g2b] = gerritRev(ci2.Revisions[g2b], 2)
	s.push("gerrit.git", g2b, ci2.Revisions[g2b].Ref)
	s.together[2] = []int{2, 1}
	s.push("github.git", g1, "refs/heads/gerrit/I1")
	s.push("github.git", h2, "refs/heads/gerrit/I2")
	pr1 := s.pr(10, "gerrit/I1", g1, "main", "bot")
	pr2 := s.pr(20, "gerrit/I2", h2, "gerrit/I1", "bot")
	pr1.Stack = &github.PRStack{Number: 30}
	pr2.Stack = &github.PRStack{Number: 30}
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/I1", Lifecycle: "NEW"}})
	s.save(ctx, store, 2, &state.Change{State: state.ChangeState{Change: 2, PR: 20, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/I2", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 2, GerritSHA: g2b, GitHubSHA: h2}}})

	if err := c.PrepareSubmit(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/I2"); got != g2b {
		t.Errorf("gerrit/I2 = %s, want Gerrit's commit %s (history head was %s)", got, g2b, h2)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/I1"); got != g1 {
		t.Errorf("gerrit/I1 = %s, changed though it was Gerrit's commit already", got)
	}
	if len(s.unstacks) != 1 || s.unstacks[0] != 30 {
		t.Errorf("unstacked %v, want [30]", s.unstacks)
	}
	if s.pulls[20].Base.Ref != "main" || s.pulls[10].Base.Ref != "main" {
		t.Errorf("bases: #10 %s, #20 %s; want main", s.pulls[10].Base.Ref, s.pulls[20].Base.Ref)
	}
}

// The swap never pushes what the push guard would refuse.
func TestPrepareSubmitNeverSwapsInConfigChanges(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	g1 := s.commit(m0, "one.txt", "1\n", "One\n\nChange-Id: I1\n")
	g1b := s.commit(m0, "CODEOWNERS", "* @me\n", "One\n\nChange-Id: I1\n")
	ci := s.change(1, "I1", g1b, "One\n\nChange-Id: I1\n", "NEW")
	ci.Revisions[g1b] = gerritRev(ci.Revisions[g1b], 2)
	s.push("gerrit.git", g1b, ci.Revisions[g1b].Ref)
	s.push("github.git", g1, "refs/heads/gerrit/I1")
	s.pr(10, "gerrit/I1", g1, "main", "bot")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/I1", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: g1, GitHubSHA: g1}}})
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/I1"); got != g1 {
		t.Errorf("gerrit/I1 = %s: a CODEOWNERS change was swapped in", got)
	}
}

func gerritRev(r gerrit.RevisionInfo, ps int) gerrit.RevisionInfo {
	r.Number = ps
	r.Ref = strings.TrimSuffix(r.Ref, "/1") + "/" + strconv.Itoa(ps)
	return r
}

// A merged change's PR that GitHub marked merged while the plan was being
// made is not closed with a notice.
func TestClosePRSkipsAPRGitHubJustMerged(t *testing.T) {
	s, c, snap := prStatesWorld(t, "MERGED", "open")
	snap.Changes[7].Status = "MERGED"
	s.pulls[70].Merged, s.pulls[70].State = true, "closed" // GitHub caught up
	err := c.applyOne(context.Background(), snap, Action{Kind: ClosePR, Change: 7, PR: 70, Comment: "Merged in Gerrit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.comments[70]) != 0 {
		t.Errorf("notice posted on a merged PR: %v", s.comments[70])
	}
	// Still open on GitHub: closed, with the notice.
	s.pulls[70].Merged, s.pulls[70].State = false, "open"
	if err := c.applyOne(context.Background(), snap, Action{Kind: ClosePR, Change: 7, PR: 70, Comment: "Merged in Gerrit"}); err != nil {
		t.Fatal(err)
	}
	if len(s.comments[70]) != 1 || s.pulls[70].State != "closed" {
		t.Errorf("comments=%v state=%s; want closed with the notice", s.comments[70], s.pulls[70].State)
	}
}

// fakeUploader uploads to the sites' Gerrit: the commit becomes the next
// patch set of the change whose Change-Id it carries.
type fakeUploader struct {
	s       *sites
	m       *gitmirror.Repo
	uploads []string // "user commit opts"
	// create makes an upload with a new Change-Id a new change, as
	// Gerrit would.
	create bool
	// alone makes it unable to act as anyone.
	alone bool
}

func (u *fakeUploader) CanActAs() bool { return !u.alone }

func (u *fakeUploader) Upload(ctx context.Context, user, commit, target string, opts []string) error {
	u.uploads = append(u.uploads, fmt.Sprintf("%s %s %v", user, commit, opts))
	msg, err := u.m.Git(ctx, nil, "log", "-1", "--format=%B", commit)
	if err != nil {
		return err
	}
	// As Gerrit reads it: the last Change-Id footer of the last paragraph.
	paras := strings.Split(strings.TrimSpace(string(msg)), "\n\n")
	cid := ""
	for _, l := range strings.Split(paras[len(paras)-1], "\n") {
		if v, ok := strings.CutPrefix(l, "Change-Id: "); ok {
			cid = v
		}
	}
	if cid == "" {
		return fmt.Errorf("uploaded %s has no Change-Id:\n%s", commit, msg)
	}
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if u.create && !slices.ContainsFunc(slices.Collect(maps.Values(u.s.changes)), func(ci *gerrit.ChangeInfo) bool { return ci.ChangeID == cid }) {
		n := 100 + len(u.s.changes)
		owner, ok := u.s.accounts[user]
		if !ok {
			owner = gerrit.AccountInfo{AccountID: 1, Username: "bridge"}
		}
		ref := fmt.Sprintf("refs/changes/%02d/%d/1", n%100, n)
		if _, err := u.m.Git(ctx, nil, "push", "-q", filepath.Join(u.s.dir, "gerrit.git"), commit+":"+ref); err != nil {
			return err
		}
		u.s.changes[n] = &gerrit.ChangeInfo{Number: n, ChangeID: cid, Project: "p", Branch: "main", Status: "NEW", Owner: owner, CurrentRevision: commit,
			Revisions: map[string]gerrit.RevisionInfo{commit: {Number: 1, Ref: ref, Uploader: owner, Commit: gerrit.CommitInfo{Message: string(msg)}}}}
		return nil
	}
	for _, ci := range u.s.changes {
		if ci.ChangeID == cid {
			uploader, ok := u.s.accounts[user]
			if !ok {
				uploader = gerrit.AccountInfo{AccountID: 1, Username: "bridge"}
			}
			ci.Revisions[commit] = gerrit.RevisionInfo{Number: len(ci.Revisions) + 1, Uploader: uploader, Commit: gerrit.CommitInfo{Message: string(msg)}}
			ci.CurrentRevision = commit
		}
	}
	return nil
}

// gerritBornWorld is change 1 (and, stacked on it, change 2) uploaded in
// Gerrit on m0 and exported to GitHub; Gerrit's main has since moved to m1.
// ids are their Change-Ids: the top layer's sorts first, so nothing works
// by visiting branches in name order.
func gerritBornWorld(t *testing.T, stacked bool) (s *sites, im *Importer, up *fakeUploader, m1 string, g, ids []string) {
	ids = []string{cid(9), cid(1)}
	s = newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	m1 = s.commit(m0, "a.txt", "a\nmore\n", "main moves")
	s.push("gerrit.git", m1, "refs/heads/main")
	s.push("github.git", m1, "refs/heads/main")
	g = append(g, s.commit(m0, "one.txt", "1\n", "One\n\nChange-Id: "+ids[0]+"\n"))
	if stacked {
		g = append(g, s.commit(g[0], "two.txt", "2\n", "Two\n\nChange-Id: "+ids[1]+"\n"))
	}
	s.accounts["austin"] = gerrit.AccountInfo{AccountID: 5, Username: "austin", Name: "Austin", Email: "austin@example.com"}
	for i, rev := range g {
		n, id := i+1, ids[i]
		ci := s.change(n, id, rev, map[int]string{1: "One\n\nChange-Id: " + ids[0] + "\n", 2: "Two\n\nChange-Id: " + ids[1] + "\n"}[n], "NEW")
		r := ci.Revisions[rev]
		r.Uploader = s.accounts["austin"]
		ci.Revisions[rev] = r
		s.push("github.git", rev, "refs/heads/gerrit/"+id)
		base := "main"
		if i > 0 {
			base = "gerrit/" + ids[0]
		}
		s.pr(n*10, "gerrit/"+id, rev, base, "bot")
		s.save(ctx, store, n, &state.Change{State: state.ChangeState{Change: n, ChangeID: id, PR: n * 10, Branch: "main", Origin: "gerrit",
			HeadBranch: "gerrit/" + id, Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: rev, GitHubSHA: rev}}})
	}
	s.accounts["brt"] = gerrit.AccountInfo{AccountID: 6, Username: "brt", Name: "BRT", Email: "brt@example.com"}
	up = &fakeUploader{s: s, m: c.Mirror}
	im = &Importer{Project: "p", Gerrit: c.Gerrit, GitHub: c.GitHub, GitHubURL: c.GitHubURL, Mirror: c.Mirror, Store: store,
		Uploader: up, Identity: c.Identity, Bridge: gerrit.AccountInfo{AccountID: 1, Name: "Bridge", Email: "bridge@example.com"},
		DefaultBranch: "main", Pushers: &Pushers{}, Logger: slog.Default()}
	return s, im, up, m1, g, ids
}

// rebase rebases a Gerrit commit onto onto, as GitHub's "Rebase" would:
// same change, new parent, committed by GitHub.
func (s *sites) rebase(commit, onto string) string {
	s.t.Helper()
	s.git("work", "checkout", "-q", "--detach", onto)
	s.git("work", "cherry-pick", "--allow-empty", commit)
	return s.git("work", "rev-parse", "HEAD")
}

// Design §5.2.2: a push to a Gerrit-born PR's branch is a new patch set of
// its change, with Gerrit's commit message. A pure rebase (GitHub's Rebase
// button, here by brt) is uploaded on behalf of the current patch set's
// uploader, as Gerrit's own rebase is.
func TestPushToGerritBornPRBecomesPatchSet(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	r1 := s.rebase(g[0], m1)
	s.push("github.git", r1, "refs/heads/gerrit/"+ids[0])
	s.pulls[10].Head.SHA = r1
	s.webhookPush(im.Pushers, 10, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], "austin ") || !strings.HasSuffix(up.uploads[0], " []") {
		t.Fatalf("uploads = %v, want one on behalf of austin without options (Gerrit's WIP state stays)", up.uploads)
	}
	ps2 := s.changes[1].CurrentRevision
	if got := s.git("m.git", "log", "-1", "--format=%P%n%B", ps2); got != m1+"\nOne\n\nChange-Id: "+ids[0] {
		t.Errorf("patch set 2 = parent and message %q; want m1 and Gerrit's message", got)
	}
	if s.git("m.git", "rev-parse", ps2+"^{tree}") != s.git("work", "rev-parse", r1+"^{tree}") {
		t.Error("patch set 2 does not have the pushed tree")
	}
	if ce := s.git("m.git", "log", "-1", "--format=%ce", ps2); ce != "austin@example.com" {
		t.Errorf("committer %s, want the uploader's Gerrit account", ce)
	}
	st, _ := im.Store.Load(ctx, 1)
	if last := st.Revisions[len(st.Revisions)-1]; last.GerritSHA != ps2 || last.GitHubSHA != r1 || last.PatchSet != 2 {
		t.Errorf("recorded %+v, want patch set 2 = GitHub %s", last, short(r1))
	}
	if sts := s.statuses[r1]; len(sts) != 1 || sts[0].Context != SyncStatus || sts[0].State != "success" {
		t.Errorf("gerrit/sync = %+v", sts)
	}
	// Imported: nothing more to do.
	if err := im.Import(ctx, "main"); err != nil || len(up.uploads) != 1 {
		t.Errorf("err=%v uploads=%v; the push was imported again", err, up.uploads)
	}
}

// GitHub's "Rebase stack": both layers pushed at once become patch sets
// chained in Gerrit, bottom first.
func TestRestackedGerritStackImportsInOrder(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, true)
	ctx := context.Background()
	r1 := s.rebase(g[0], m1)
	r2 := s.rebase(g[1], r1)
	s.push("github.git", r1, "refs/heads/gerrit/"+ids[0])
	s.push("github.git", r2, "refs/heads/gerrit/"+ids[1])
	s.pulls[10].Head.SHA, s.pulls[20].Head.SHA = r1, r2
	s.webhookPush(im.Pushers, 10, "brt")
	s.webhookPush(im.Pushers, 20, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 2 || !strings.HasPrefix(up.uploads[0], "austin ") || !strings.HasPrefix(up.uploads[1], "austin ") {
		t.Fatalf("uploads = %v, want both on behalf of austin (pure rebases onto main and onto austin's own)", up.uploads)
	}
	p1, p2 := s.changes[1].CurrentRevision, s.changes[2].CurrentRevision
	if got := s.git("m.git", "rev-parse", p2+"^"); got != p1 {
		t.Errorf("layer 2's patch set sits on %s, want layer 1's new patch set %s", short(got), short(p1))
	}
	if got := s.git("m.git", "rev-parse", p1+"^"); got != m1 {
		t.Errorf("layer 1's patch set sits on %s, want main %s", short(got), short(m1))
	}
	if msg := s.git("m.git", "log", "-1", "--format=%B", p2); msg != "Two\n\nChange-Id: "+ids[1] {
		t.Errorf("layer 2 message %q", msg)
	}
}

// The bridge's own exports are not pushes to import, recorded or not.
func TestBridgesOwnHeadIsNotImported(t *testing.T) {
	s, im, up, _, g, _ := gerritBornWorld(t, false)
	ctx := context.Background()
	if err := im.Import(ctx, "main"); err != nil || len(up.uploads) != 0 {
		t.Errorf("err=%v uploads=%v; the recorded export was imported", err, up.uploads)
	}
	// Gerrit's current commit, pushed but not recorded yet (a crash
	// between the push and the record).
	st, _ := im.Store.Load(ctx, 1)
	st.Revisions = nil
	if err := im.Store.Save(ctx, 1, st, "forget"); err != nil {
		t.Fatal(err)
	}
	s.pulls[10].Head.SHA = g[0]
	if err := im.Import(ctx, "main"); err != nil || len(up.uploads) != 0 {
		t.Errorf("err=%v uploads=%v; Gerrit's own commit was imported", err, up.uploads)
	}
}

// Design §5.2.1: a new patch set goes on top of the PR's history; a
// rebased one is a merge of its new parent. Both have the patch set's tree.
func TestAppendCommitBuildsHistory(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	m1 := s.commit(m0, "a.txt", "a\nmore\n", "main moves")
	ps1 := s.commit(m0, "one.txt", "1\n", "One")
	ps2 := s.commit(m0, "one.txt", "1b\n", "One")    // amended
	ps3 := s.commit(m1, "one.txt", "1b\n", "One")    // rebased
	s.push("gerrit.git", ps3, "refs/changes/01/1/3") // the mirror has them all
	s.push("gerrit.git", ps2, "refs/changes/01/1/2")
	s.push("gerrit.git", ps1, "refs/changes/01/1/1")
	s.git("m.git", "fetch", "-q", "origin", "refs/changes/*:refs/changes/*")
	h2, err := c.appendCommit(ctx, Append{Change: 1, PatchSet: 2, Revision: ps2, Onto: ps1})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.git("m.git", "log", "-1", "--format=%P %s", h2); got != ps1+" Patch set 2 from Gerrit" {
		t.Errorf("amend: %q", got)
	}
	h3, err := c.appendCommit(ctx, Append{Change: 1, PatchSet: 3, Revision: ps3, Onto: h2, Merge: m1})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.git("m.git", "log", "-1", "--format=%P %s", h3); got != h2+" "+m1+" Patch set 3 from Gerrit, rebased" {
		t.Errorf("rebase: %q", got)
	}
	for h, ps := range map[string]string{h2: ps2, h3: ps3} {
		if s.git("m.git", "rev-parse", h+"^{tree}") != s.git("m.git", "rev-parse", ps+"^{tree}") {
			t.Errorf("%s does not have %s's tree", short(h), short(ps))
		}
	}
}

// observeHistory says whether a Gerrit-born head already contains the new
// patch set's parent (append) or not (merge), and spots same-tree heads.
func TestObserveHistory(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	m1 := s.commit(m0, "a.txt", "a\nmore\n", "main moves")
	ps1 := s.commit(m0, "one.txt", "1\n", "One")
	s.push("gerrit.git", m1, "refs/heads/main")
	s.push("github.git", ps1, "refs/heads/gerrit/"+cid(1))
	snapFor := func(rev, parent string) *Snapshot {
		s.push("gerrit.git", rev, "refs/changes/01/1/9")
		s.git("m.git", "fetch", "-q", "origin", "+refs/changes/01/1/9:refs/changes/01/1/9", "+refs/heads/main:refs/remotes/gerrit/heads/main")
		return &Snapshot{Branch: "main", DefaultBranch: "main",
			Changes:        map[int]*GerritChange{1: {Number: 1, ChangeID: cid(1), Status: "NEW", Revision: rev, Parent: parent}},
			GitHubBranches: map[string]string{"gerrit/" + cid(1): ps1}, SameTree: map[int]string{},
			State: map[int]*state.Change{1: {State: state.ChangeState{Origin: "gerrit"},
				Revisions: []state.Revision{{PatchSet: 1, GerritSHA: ps1, GitHubSHA: ps1}}}}}
	}
	env, _ := c.githubEnv(ctx)
	for _, tc := range []struct {
		name            string
		rev, parent     string
		hasParent, same bool
	}{
		{"amend", s.commit(m0, "one.txt", "1b\n", "One"), m0, true, false},
		{"message-only edit", s.commit(m0, "one.txt", "1\n", "One, better"), m0, true, true},
		{"rebase", s.commit(m1, "one.txt", "1\n", "One"), m1, false, false},
	} {
		snap := snapFor(tc.rev, tc.parent)
		if err := c.observeHistory(ctx, snap, env); err != nil {
			t.Fatal(err)
		}
		if snap.HasParent[1] != tc.hasParent || (snap.SameTree[1] == ps1) != tc.same {
			t.Errorf("%s: HasParent=%v SameTree=%q; want %v, same=%v", tc.name, snap.HasParent[1], snap.SameTree[1], tc.hasParent, tc.same)
		}
	}
}

// /submit of a GitHub-born PR: the author's branch gets Gerrit's squash,
// so GitHub shows it merged; a fork that does not allow maintainer edits
// is left alone (and closed with a notice later).
// A push the bridge has not imported yet is never overwritten: the PR is
// closed with a notice after the merge instead.
func TestPrepareSubmitKeepsUnimportedPush(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	h2 := s.commit(m0, "f.txt", "2\n", "done")
	sq := s.commit(m0, "f.txt", "2\n", "Feature\n\nChange-Id: "+cid(1)+"\n")
	h3 := s.commit(h2, "f.txt", "3\n", "pushed just now")
	s.change(1, cid(1), sq, "Feature\n\nChange-Id: "+cid(1)+"\n", "NEW")
	s.push("github.git", h3, "refs/heads/feature")
	s.pr(10, "feature", h3, "main", "brt")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: h2}}})
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/feature"); got != h3 {
		t.Errorf("feature = %s, want the unimported push %s kept", short(got), short(h3))
	}
}

func TestPrepareSubmitSwapsGitHubBornBranch(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	h1 := s.commit(m0, "f.txt", "1\n", "wip")
	h2 := s.commit(h1, "f.txt", "2\n", "done") // the author's two commits
	sq := s.commit(m0, "f.txt", "2\n", "Feature\n\nChange-Id: "+cid(1)+"\n")
	s.change(1, cid(1), sq, "Feature\n\nChange-Id: "+cid(1)+"\n", "NEW")
	s.push("github.git", h2, "refs/heads/feature")
	s.pr(10, "feature", h2, "main", "brt")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: h2}}})
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/feature"); got != sq {
		t.Errorf("feature = %s, want Gerrit's squash %s", short(got), short(sq))
	}

	// A fork without maintainer edits: untouched.
	s.pulls[10].Head = github.Ref{Ref: "main", SHA: h2, Repo: &struct {
		FullName string `json:"full_name"`
	}{"alice/r"}}
	st, _ := store.Load(ctx, 1)
	st.State.HeadBranch = "alice/r:main"
	s.save(ctx, store, 1, st)
	if err := c.PrepareSubmit(ctx, 1); err != nil {
		t.Errorf("PrepareSubmit with a locked fork: %v", err)
	}
}

// The same history check for a GitHub-born PR from a fork: its head is
// fetched from the base repository's refs/pull.
func TestObserveHistoryForkPR(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	m1 := s.commit(m0, "a.txt", "a\nmore\n", "main moves")
	h1 := s.commit(m0, "one.txt", "1\n", "the author's commit")
	s.push("gerrit.git", m1, "refs/heads/main")
	s.push("github.git", h1, "refs/pull/10/head")
	env, _ := c.githubEnv(ctx)
	for _, tc := range []struct {
		name, parent string
		hasParent    bool
	}{{"edit in Gerrit", m0, true}, {"rebase in Gerrit", m1, false}} {
		rev := s.commit(tc.parent, "one.txt", "1b\n", "One")
		s.push("gerrit.git", rev, "refs/changes/01/1/9")
		s.git("m.git", "fetch", "-q", "origin", "+refs/changes/01/1/9:refs/changes/01/1/9")
		snap := &Snapshot{Branch: "main", DefaultBranch: "main",
			Changes:        map[int]*GerritChange{1: {Number: 1, ChangeID: cid(1), Status: "NEW", Revision: rev, Parent: tc.parent}},
			GitHubBranches: map[string]string{"alice/r:main": h1}, SameTree: map[int]string{},
			State: map[int]*state.Change{1: {State: state.ChangeState{Origin: "github", PR: 10, HeadBranch: "alice/r:main"},
				Revisions: []state.Revision{{PatchSet: 1, GerritSHA: "s1", GitHubSHA: h1}}}}}
		if err := c.observeHistory(ctx, snap, env); err != nil {
			t.Fatal(err)
		}
		if snap.HasParent[1] != tc.hasParent {
			t.Errorf("%s: HasParent = %v", tc.name, snap.HasParent[1])
		}
	}
}

// editsWorld is change 7 and its PR in sync (title "Seven", description
// "Does seven things."), as record() leaves them.
func editsWorld(t *testing.T, origin string) (*sites, *Code, *Snapshot) {
	s, c, snap := prStatesWorld(t, "NEW", "open")
	msg := "Seven\n\nDoes seven things.\n\nChange-Id: " + cid(7) + "\nSigned-off-by: Austin <austin@example.com>\n"
	snap.Changes[7].Message, snap.Changes[7].Subject = msg, "Seven"
	snap.Changes[7].Owner, snap.Changes[7].Uploader = "austin", "austin"
	s.changes[7].Revisions[s.changes[7].CurrentRevision] = gerrit.RevisionInfo{Number: 1, Commit: gerrit.CommitInfo{Message: msg}}
	pr := s.pulls[70]
	pr.Title, pr.Body = "Seven", "Does seven things.\n\n"+linksStart+"\nblock\n"+linksEnd
	st := snap.State[7]
	st.State.Origin = origin
	st.State.SyncedText, st.State.SyncedWIP = hashString(gerritText(msg)), "ready"
	return s, c, snap
}

func TestPREditBecomesCommitMessageAsTheEditor(t *testing.T) {
	for _, origin := range []string{"gerrit", "github"} { // the same either way
		s, c, snap := editsWorld(t, origin)
		s.pulls[70].Title = "Seven, better"
		s.pulls[70].Body = "Does seven things well.\n\nBug: 7\n\n" + linksStart + "\nblock\n" + linksEnd
		s.activity[70] = github.PRActivity{TextEditor: "austin"}
		changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{})
		if err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", origin, changed, err)
		}
		ci := s.changes[7]
		want := "Seven, better\n\nDoes seven things well.\n\nBug: 7\nChange-Id: " + cid(7) + "\nSigned-off-by: Austin <austin@example.com>\n"
		if got := ci.Revisions[ci.CurrentRevision].Commit.Message; got != want {
			t.Errorf("%s: message\n%q\nwant\n%q", origin, got, want)
		}
		if len(s.calls) != 1 || s.calls[0] != "message 7 as austin" {
			t.Errorf("%s: calls = %v", origin, s.calls)
		}
	}
}

// A PR GitHub will not take an edit for is that change's problem only.
func TestOnePRGitHubRefusesDoesNotStopTheRest(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	s.pr(10, "a", "x", "main", "bot")
	s.pr(20, "b", "y", "main", "bot")
	s.refusePulls = map[int]bool{10: true}
	skip := map[int]bool{}
	p := &Plan{Actions: []Action{
		{Kind: UpdatePR, Change: 1, PR: 10, Update: &github.PullUpdate{State: ptr("closed")}},
		{Kind: UpdatePR, Change: 1, PR: 10, Update: &github.PullUpdate{Base: ptr("x")}},
		{Kind: UpdatePR, Change: 2, PR: 20, Update: &github.PullUpdate{State: ptr("closed")}},
	}}
	if err := c.apply(ctx, &Snapshot{Branch: "main"}, p, skip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(skip, map[int]bool{1: true}) || s.pulls[20].State != "closed" || strings.Join(s.calls, ",") != "update #10,update #20" {
		t.Errorf("skip = %v, #20 %s, calls %v", skip, s.pulls[20].State, s.calls)
	}
	p.drop(skip)
	if len(p.Actions) != 1 || p.Actions[0].Change != 2 {
		t.Errorf("after drop: %v", p.Actions)
	}
	if !refusedByGitHub(&github.APIError{Status: 404}) || refusedByGitHub(&github.APIError{Status: 403}) || refusedByGitHub(&github.APIError{Status: 429}) ||
		refusedByGitHub(&github.APIError{Status: 401}) || refusedByGitHub(&github.APIError{Status: 502}) || refusedByGitHub(errors.New("x")) {
		t.Error("refusedByGitHub")
	}
}

// A description the bridge shortened for GitHub is what the bridge wrote,
// not someone's edit to mirror back into the commit message.
func TestShortenedDescriptionIsNoEdit(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	msg := "Seven\n\n" + strings.Repeat("long ", 20000) + "\n\nChange-Id: " + cid(7) + "\n"
	snap.Changes[7].Message = msg
	body, hash := composeBody(msg, "")
	s.pulls[70].Body = body
	snap.PRs[70] = s.pulls[70]
	s.save(context.Background(), c.Store, 7, snap.State[7]) // synced before the message grew
	p := &Plan{Desired: map[int]*Desired{7: {Change: 7, PR: 70, Head: "gerrit/" + cid(7), Base: "main", Title: "Seven", Body: body, BodyHash: hash}}}
	if err := c.record(context.Background(), snap, p); err != nil {
		t.Fatal(err)
	}
	snap.State[7], _ = c.Store.Load(context.Background(), 7)
	s.activity[70] = github.PRActivity{TextEditor: "bot"}
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed || len(s.calls) != 0 || len(s.comments[70]) != 0 {
		t.Errorf("changed=%v err=%v calls=%v notices=%v", changed, err, s.calls, s.comments[70])
	}
}

// A PR's GitHub side is mirrored at most once per reconcile: someone
// editing, toggling or closing their PR over and over cannot keep the
// branch from converging.
func TestPRMirroredOncePerReconcile(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	s.pulls[70].Title = "Seven, better"
	s.activity[70] = github.PRActivity{TextEditor: "austin"}
	handled := map[int]bool{70: true}
	if changed, err := c.mirrorPREdits(context.Background(), snap, handled); err != nil || changed || len(s.calls) != 0 {
		t.Fatalf("handled PR mirrored again: changed=%v err=%v calls=%v", changed, err, s.calls)
	}
	handled = map[int]bool{}
	if changed, err := c.mirrorPREdits(context.Background(), snap, handled); err != nil || !changed || !handled[70] {
		t.Fatalf("changed=%v err=%v handled=%v", changed, err, handled)
	}

	s, c, snap = prStatesWorld(t, "NEW", "closed")
	s.ghEvent(70, "closed", "austin")
	if changed, err := c.mirrorPRStates(context.Background(), snap, map[int]bool{70: true}); err != nil || changed || s.changes[7].Status != "NEW" {
		t.Fatalf("handled PR's close mirrored again: changed=%v err=%v", changed, err)
	}
	handled = map[int]bool{}
	if changed, err := c.mirrorPRStates(context.Background(), snap, handled); err != nil || !changed || !handled[70] {
		t.Fatalf("changed=%v err=%v handled=%v", changed, err, handled)
	}
}

func TestPREditGerritRefusesIsExplained(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	snap.Changes[7].Uploader = "brt"
	s.pulls[70].Title = "Hijacked"
	s.activity[70] = github.PRActivity{TextEditor: "brt"} // may not edit change 7
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "@brt edited this PR's title or description, which would change the commit message of [change 7]") ||
		!strings.Contains(s.comments[70][0], "Gerrit would not let brt (edit not permitted). Putting Gerrit's back.") {
		t.Errorf("notice = %v", s.comments[70])
	}
	// The plan then puts Gerrit's title back.
	p := MakePlan(planSnapshot(snap))
	if !hasAction(p, UpdatePR, func(a Action) bool { return a.Update.Title != nil && *a.Update.Title == "Seven" }) {
		t.Errorf("plan does not restore the title: %v", p.Actions)
	}
}

// A commit message edit makes a new patch set, and CI trusts its
// uploader: retitling someone else's PR on GitHub must not make the editor
// the uploader of their code.
func TestPREditByNonUploaderIsRefused(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	s.may[7] = append(s.may[7], "brt")
	s.pulls[70].Title = "Seven, retitled"
	s.activity[70] = github.PRActivity{TextEditor: "brt"}
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed || len(s.calls) != 0 {
		t.Fatalf("changed=%v err=%v calls=%v", changed, err, s.calls)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "only the uploader of its current patch set") {
		t.Errorf("notice = %v", s.comments[70])
	}
}

// Someone without a Gerrit account edits only their own PR's change, which
// the bridge owns for them, as the bridge (as their pushes are). Anyone
// else would borrow the bridge's rights in Gerrit.
func TestPREditByUserWithoutGerritAccount(t *testing.T) {
	for _, tc := range []struct {
		name, author string
		mirrored     bool
	}{{"their own PR", "stranger", true}, {"someone else's PR", "austin", false}} {
		s, c, snap := editsWorld(t, "github")
		c.BridgeUser = "bridge"
		snap.Changes[7].Owner, snap.Changes[7].Uploader = "bridge", "bridge"
		s.may[7] = append(s.may[7], "bridge")
		s.pulls[70].User.Login = tc.author
		s.pulls[70].Title = "Seven, by a stranger"
		s.activity[70] = github.PRActivity{TextEditor: "stranger"}
		changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{})
		if err != nil || changed != tc.mirrored {
			t.Fatalf("%s: changed=%v err=%v", tc.name, changed, err)
		}
		if tc.mirrored && (len(s.calls) != 1 || s.calls[0] != "message 7 as bridge") {
			t.Errorf("%s: calls = %v, want the bridge", tc.name, s.calls)
		}
		if !tc.mirrored && (len(s.calls) != 0 || len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "@stranger has no Gerrit account")) {
			t.Errorf("%s: calls = %v, notice = %v", tc.name, s.calls, s.comments[70])
		}
	}
}

// Nobody known (a deleted account, or an edit GitHub's timeline does not
// show): nothing is done as the bridge.
func TestDraftToggleByNobodyIsRefused(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	s.may[7] = append(s.may[7], "bridge")
	s.pulls[70].Draft = true
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed || len(s.calls) != 0 {
		t.Fatalf("changed=%v err=%v calls=%v", changed, err, s.calls)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "Someone marked this PR as a draft") || !strings.Contains(s.comments[70][0], "GitHub does not say who") {
		t.Errorf("notice = %v", s.comments[70])
	}
}

func TestPREditAndGerritEditBothGerritWins(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	s.pulls[70].Title = "GitHub's"
	snap.Changes[7].Message, snap.Changes[7].Subject = "Gerrit's\n\nChange-Id: "+cid(7)+"\n", "Gerrit's"
	s.activity[70] = github.PRActivity{TextEditor: "austin"}
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed || len(s.calls) != 0 {
		t.Fatalf("changed=%v err=%v calls=%v", changed, err, s.calls)
	}
	if len(s.comments[70]) != 1 || !strings.Contains(s.comments[70][0], "Gerrit's is kept") {
		t.Errorf("notice = %v", s.comments[70])
	}
}

func TestDraftToggleBecomesWIPAsTheToggler(t *testing.T) {
	for _, origin := range []string{"gerrit", "github"} {
		s, c, snap := editsWorld(t, origin)
		s.pulls[70].Draft = true
		s.activity[70] = github.PRActivity{DraftActor: "austin"}
		if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", origin, changed, err)
		}
		if !s.changes[7].WorkInProgress || len(s.calls) != 1 || s.calls[0] != "wip 7 as austin" {
			t.Errorf("%s: wip=%v calls=%v", origin, s.changes[7].WorkInProgress, s.calls)
		}
	}
}

// Before anything was synced (state from an older bridge), Gerrit's
// version stands: nothing is sent to Gerrit.
func TestPREditWithoutSyncedStateLeavesGerrit(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	snap.State[7].State.SyncedText, snap.State[7].State.SyncedWIP = "", ""
	s.pulls[70].Title, s.pulls[70].Draft = "Edited", true
	if changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{}); err != nil || changed || len(s.calls) != 0 {
		t.Errorf("changed=%v err=%v calls=%v", changed, err, s.calls)
	}
}

func TestCommitMessage(t *testing.T) {
	cur := "Old\n\nOld body.\n\nBug: 1\nChange-Id: I1\nSigned-off-by: A <a@x>\nGitHub-PR: u\n"
	for _, tc := range []struct{ title, body, want string }{
		{"New", "New body.", "New\n\nNew body.\n\nChange-Id: I1\nSigned-off-by: A <a@x>\nGitHub-PR: u\n"},
		{"New", "New body.\n\nBug: 2", "New\n\nNew body.\n\nBug: 2\nChange-Id: I1\nSigned-off-by: A <a@x>\nGitHub-PR: u\n"},
		{"New", "", "New\n\nChange-Id: I1\nSigned-off-by: A <a@x>\nGitHub-PR: u\n"},
	} {
		got := commitMessage(tc.title, tc.body, cur)
		if got != tc.want {
			t.Errorf("commitMessage(%q, %q) =\n%q\nwant\n%q", tc.title, tc.body, got, tc.want)
		}
		// It round-trips: the PR text it stands for is what was asked.
		if gerritText(got) != tc.title+"\x00"+tc.body {
			t.Errorf("round trip of %q: %q", got, gerritText(got))
		}
	}
}

// A later push to a GitHub-born PR keeps the change's commit message (PR
// text reaches it separately), adding the new commits' sign-offs.
func TestLaterImportKeepsMessageAddsSignOffs(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	st, _ := im.Store.Load(ctx, 1)
	st.State.Origin, st.State.HeadBranch = "github", "feature"
	if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
		t.Fatal(err)
	}
	s.pulls[10].Head.Ref, s.pulls[10].User.Login = "feature", "brt"
	s.pulls[10].Title, s.pulls[10].Body = "Some other PR title", "Not the commit message."
	r1 := s.rebase(g[0], m1)
	h := s.commit(r1, "one.txt", "1 fixed\n", "Fix\n\nSigned-off-by: BRT <brt@example.com>")
	s.push("github.git", h, "refs/heads/feature")
	s.pulls[10].Head.SHA = h
	s.webhookPush(im.Pushers, 10, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasSuffix(up.uploads[0], " []") {
		t.Fatalf("uploads = %v, want one keeping the WIP state", up.uploads)
	}
	msg := s.git("m.git", "log", "-1", "--format=%B", s.changes[1].CurrentRevision)
	if want := "One\n\nChange-Id: " + ids[0] + "\nSigned-off-by: BRT <brt@example.com>"; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
}

// A patch set someone else uploaded in Gerrit to a GitHub-born change
// never reached the PR (markForeign): the author's next push builds on
// the patch set GitHub had, so it is theirs, with that patch set's
// message, and does not race Gerrit.
func TestPushAfterForeignPatchSetIsTheAuthors(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	st, _ := im.Store.Load(ctx, 1)
	st.State.Origin, st.State.HeadBranch = "github", "feature"
	if err := im.Store.Save(ctx, 1, st, "GitHub-born"); err != nil {
		t.Fatal(err)
	}
	ci := s.changes[1]
	ci.Owner = s.accounts["austin"]
	mal := s.commit(g[0], "one.txt", "mallory\n", "Mallory's message\n\nChange-Id: "+ids[0]+"\n")
	s.push("gerrit.git", mal, "refs/changes/01/1/2")
	ci.Revisions[mal] = gerrit.RevisionInfo{Number: 2, Ref: "refs/changes/01/1/2", Uploader: gerrit.AccountInfo{AccountID: 9, Username: "mallory"},
		Commit: gerrit.CommitInfo{Message: "Mallory's message\n\nChange-Id: " + ids[0] + "\n"}}
	ci.CurrentRevision = mal
	s.pulls[10].Head.Ref, s.pulls[10].User.Login = "feature", "austin"
	h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
	s.push("github.git", h, "refs/heads/feature")
	s.pulls[10].Head.SHA = h
	s.webhookPush(im.Pushers, 10, "austin")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], "austin ") {
		t.Fatalf("uploads = %v, want one as austin (and no restore of mallory's)", up.uploads)
	}
	if msg := s.git("m.git", "log", "-1", "--format=%B", ci.CurrentRevision); strings.Contains(msg, "Mallory") {
		t.Errorf("the author's patch set took mallory's message: %q", msg)
	}

	// Someone else's pure rebase of what GitHub had is the author's too.
	s2, im2, up2, m1b, g2, ids2 := gerritBornWorld(t, false)
	st2, _ := im2.Store.Load(ctx, 1)
	st2.State.Origin, st2.State.HeadBranch = "github", "feature"
	if err := im2.Store.Save(ctx, 1, st2, "GitHub-born"); err != nil {
		t.Fatal(err)
	}
	ci2 := s2.changes[1]
	mal2 := s2.commit(g2[0], "one.txt", "mallory\n", "Mallory's message\n\nChange-Id: "+ids2[0]+"\n")
	s2.push("gerrit.git", mal2, "refs/changes/01/1/2")
	ci2.Revisions[mal2] = gerrit.RevisionInfo{Number: 2, Ref: "refs/changes/01/1/2", Uploader: gerrit.AccountInfo{AccountID: 9, Username: "mallory"},
		Commit: gerrit.CommitInfo{Message: "Mallory's message\n\nChange-Id: " + ids2[0] + "\n"}}
	ci2.CurrentRevision, ci2.Owner = mal2, s2.accounts["austin"]
	s2.pulls[10].Head.Ref, s2.pulls[10].User.Login = "feature", "austin"
	r := s2.rebase(g2[0], m1b)
	s2.push("github.git", r, "refs/heads/feature")
	s2.pulls[10].Head.SHA = r
	s2.webhookPush(im2.Pushers, 10, "brt")
	if err := im2.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up2.uploads) != 1 || !strings.HasPrefix(up2.uploads[0], "austin ") {
		t.Errorf("rebase by brt: uploads = %v, want one on behalf of austin", up2.uploads)
	}
}

// A foreign patch set is never recorded as on GitHub, even where GitHub's
// head has its tree (a commit-message edit): /submit and the importer
// take recorded patch sets for ones GitHub had.
func TestForeignPatchSetIsNotRecorded(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	h1 := s.commit(m0, "b.txt", "b\n", "Mine")
	ps1 := s.commit(m0, "b.txt", "b\n", "Mine\n\nChange-Id: "+cid(1)+"\n")
	ps2 := s.commit(m0, "b.txt", "b\n", "Mallory's\n\nChange-Id: "+cid(1)+"\n")
	st := &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: ps1, GitHubSHA: h1}}}
	s.save(ctx, store, 1, st)
	st, _ = store.Load(ctx, 1)
	pr := s.pr(10, "feature", h1, "main", "brt")
	for _, foreign := range []bool{true, false} {
		snap := &Snapshot{Branch: "main", Changes: map[int]*GerritChange{1: {Number: 1, Status: "NEW", PatchSet: 2, Revision: ps2, Subject: "Mallory's"}},
			State: map[int]*state.Change{1: st}, PRs: map[int]*github.PullRequest{10: pr}, GitHubBranches: map[string]string{"feature": h1},
			SameTree: map[int]string{1: h1}, Foreign: map[int]bool{1: foreign}}
		p := &Plan{Desired: map[int]*Desired{1: {Change: 1, Head: "feature", PR: 10, Base: "main"}}}
		if err := c.record(ctx, snap, p); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Load(ctx, 1)
		if recorded := len(got.Revisions) == 2; recorded == foreign {
			t.Errorf("foreign %v: revisions %+v", foreign, got.Revisions)
		}
	}
}

// The bridge's own export of an older patch set (pushed before a newer
// one was uploaded, and not recorded yet) is Gerrit's commit, not anyone's
// push: it is recorded, never uploaded again over the newer one.
func TestOwnExportOfOlderPatchSetIsNotImported(t *testing.T) {
	s, im, up, _, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	st, _ := im.Store.Load(ctx, 1)
	st.Revisions = nil
	if err := im.Store.Save(ctx, 1, st, "export not recorded"); err != nil {
		t.Fatal(err)
	}
	ci := s.changes[1]
	ps2 := s.commit(g[0], "one.txt", "2\n", "One\n\nChange-Id: "+ids[0]+"\n")
	s.push("gerrit.git", ps2, "refs/changes/01/1/2")
	ci.Revisions[ps2] = gerrit.RevisionInfo{Number: 2, Ref: "refs/changes/01/1/2", Uploader: s.accounts["austin"], Commit: gerrit.CommitInfo{Message: "One\n\nChange-Id: " + ids[0] + "\n"}}
	ci.CurrentRevision = ps2
	s.webhookPush(im.Pushers, 10, "bot")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 0 || ci.CurrentRevision != ps2 {
		t.Fatalf("uploads = %v: the bridge's own export was uploaded over patch set 2", up.uploads)
	}
	st, _ = im.Store.Load(ctx, 1)
	if len(st.Revisions) != 1 || st.Revisions[0].PatchSet != 1 || st.Revisions[0].GitHubSHA != g[0] {
		t.Errorf("revisions = %+v, want patch set 1 recorded as on GitHub", st.Revisions)
	}
}

// Gerrit reads the Change-Id footer of the last paragraph. A commit with
// another Change-Id higher up must not get past the "belongs to another
// change" check on that one and land as a patch set of the footer's.
func TestImportRefusesChangeIDsGerritReadsDifferently(t *testing.T) {
	s, im, up, m1, _, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	// Committed as the bridge would commit an unmapped stranger's
	// squash, so the commit could go up unchanged.
	im.Bridge.Email = "austin@example.com"
	evil := s.commit(m1, "evil.txt", "x\n", "Evil\n\nChange-Id: "+cid(5)+"\n\nChange-Id: "+ids[0]+"\n")
	s.push("github.git", evil, "refs/heads/evil")
	s.pr(50, "evil", evil, "main", "stranger")
	before := s.changes[1].CurrentRevision
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if s.changes[1].CurrentRevision != before {
		t.Fatalf("the stranger's PR became a patch set of change 1 (uploads %v)", up.uploads)
	}
	if len(up.uploads) != 0 {
		t.Errorf("uploads = %v", up.uploads)
	}
	if sts := s.statuses[evil]; len(sts) != 1 || sts[0].State != "pending" || !strings.Contains(sts[0].Description, "Change-Id") {
		t.Errorf("gerrit/sync = %+v, want pending about the Change-Id", sts)
	}
}

// CI trusts the uploader, so a push is uploaded as its pusher only if they
// uploaded the current patch set too; anyone else's push that is more
// than a rebase (Commit suggestion, a fix-up, Update branch with
// conflicts resolved) is uploaded by the bridge, which CI does not trust.
func TestWhoUploadsAPush(t *testing.T) {
	for _, tc := range []struct {
		name, pusher, want string
		edit               bool
	}{
		{"uploader pushes", "austin", "austin", true},
		{"someone else adds code", "brt", "", true},
		{"unknown pusher adds code", "", "", true},
		{"unknown pusher rebases", "", "austin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, im, up, m1, g, ids := gerritBornWorld(t, false)
			ctx := context.Background()
			h := s.rebase(g[0], m1)
			if tc.edit {
				h = s.commit(h, "one.txt", "1 fixed\n", "Fix")
			}
			s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
			s.pulls[10].Head.SHA = h
			s.webhookPush(im.Pushers, 10, tc.pusher)
			if err := im.Import(ctx, "main"); err != nil {
				t.Fatal(err)
			}
			if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], tc.want+" ") {
				t.Fatalf("uploads = %v, want one as %q", up.uploads, tc.want)
			}
		})
	}
}

// Nothing is uploaded as an inactive account, whose name Gerrit may read
// as someone else's.
func TestNoUploadAsAnInactiveAccount(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	r := s.changes[1].Revisions[g[0]]
	r.Uploader.Inactive = true
	s.changes[1].Revisions[g[0]] = r
	h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
	s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
	s.pulls[10].Head.SHA = h
	s.webhookPush(im.Pushers, 10, "austin")
	if err := im.Import(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], " ") {
		t.Errorf("uploads = %v, want the bridge's", up.uploads)
	}
}

// A maintainer's "Update branch" on a stranger's PR (a merge of main the
// maintainer pushed) leaves the stranger's code uploaded as the stranger:
// it is a pure rebase.
func TestUpdateBranchOnSomeoneElsesPRStaysTheirs(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	s.git("work", "checkout", "-q", "--detach", g[0])
	s.git("work", "merge", "-q", "--no-edit", m1)
	h := s.git("work", "rev-parse", "HEAD")
	s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
	s.pulls[10].Head.SHA = h
	s.webhookPush(im.Pushers, 10, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], "austin ") {
		t.Fatalf("uploads = %v, want one on behalf of austin", up.uploads)
	}
}

// A PR cannot bring more than the bounds into Gerrit, which keeps every
// patch set's objects for good.
func TestImportRefusesTooBigPRs(t *testing.T) {
	defer func(f, total int64) { maxImportFile, maxImportTotal = f, total }(maxImportFile, maxImportTotal)
	maxImportFile, maxImportTotal = 1000, 3000
	for _, tc := range []struct {
		name  string
		files map[string]int
		want  string
	}{
		{"small", map[string]int{"a.bin": 500}, ""},
		{"a big file", map[string]int{"a.bin": 1500}, "A file is larger than"},
		{"too much in all", map[string]int{"a.bin": 900, "b.bin": 900, "c.bin": 900, "d.bin": 900}, "This PR adds more than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, im, up, m1, _, _ := gerritBornWorld(t, false)
			h := m1
			for name, n := range tc.files {
				h = s.commit(h, name, strings.Repeat(name[:1], n), "Add "+name)
			}
			s.push("github.git", h, "refs/heads/big")
			s.pr(50, "big", h, "main", "austin")
			if err := im.Import(context.Background(), "main"); err != nil {
				t.Fatal(err)
			}
			sts := s.statuses[h]
			if tc.want == "" {
				if len(up.uploads) != 1 {
					t.Errorf("uploads = %v, statuses %+v", up.uploads, sts)
				}
				return
			}
			if len(up.uploads) != 0 || len(sts) != 1 || sts[0].State != "failure" || !strings.Contains(sts[0].Description, tc.want) {
				t.Errorf("uploads = %v, statuses %+v", up.uploads, sts)
			}
		})
	}
}

// Objects from GitHub are checked as they are fetched: a malformed tree
// (here one with a ".." entry) never reaches the mirror, let alone Gerrit.
func TestImportFetchChecksObjects(t *testing.T) {
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	empty := s.git("work", "hash-object", "-w", "-t", "blob", "/dev/null")
	cmd := exec.Command("git", "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	cmd.Dir = filepath.Join(s.dir, "work")
	raw, _ := hex.DecodeString(empty)
	cmd.Stdin = bytes.NewReader(append([]byte("100644 ..\x00"), raw...))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	bad := s.git("work", "commit-tree", strings.TrimSpace(string(out)), "-p", m1, "-m", "Evil")
	s.push("github.git", bad, "refs/heads/evil")
	s.pr(50, "evil", bad, "main", "austin")
	good := s.commit(m1, "good.txt", "g\n", "Good")
	s.push("github.git", good, "refs/heads/good")
	s.pr(51, "good", good, "main", "austin")
	// One PR's bad objects do not stop the others.
	if err := im.Import(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 {
		t.Errorf("uploads %v, want the good PR's", up.uploads)
	}
	if _, err := im.Mirror.Git(context.Background(), nil, "cat-file", "-e", bad); err == nil {
		t.Error("the malformed commit reached the mirror")
	}
	if sts := s.statuses[bad]; len(sts) != 1 || sts[0].State != "failure" || !strings.Contains(sts[0].Description, "object checks") {
		t.Errorf("bad PR's gerrit/sync = %+v", sts)
	}

	// A fork PR listing someone else's head, whose real head is bad, gets
	// only itself refused.
	s2, im2, _, m1b, _, _ := gerritBornWorld(t, false)
	victim := s2.commit(m1b, "v.txt", "v\n", "Victim")
	s2.push("github.git", victim, "refs/heads/victim")
	s2.pr(61, "victim", victim, "main", "austin")
	s2.git("work", "hash-object", "-w", "-t", "blob", "/dev/null")
	cmd = exec.Command("git", "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	cmd.Dir = filepath.Join(s2.dir, "work")
	cmd.Stdin = bytes.NewReader(append([]byte("100644 ..\x00"), raw...))
	if out, err = cmd.Output(); err != nil {
		t.Fatal(err)
	}
	evil := s2.git("work", "commit-tree", strings.TrimSpace(string(out)), "-p", m1b, "-m", "Evil")
	s2.push("github.git", evil, "refs/pull/62/head")
	s2.pr(62, "main", victim, "main", "mallory").Head.Repo.FullName = "mallory/fork"
	for range 2 {
		im2.Import(context.Background(), "main")
	}
	if sts := s2.statuses[victim]; len(sts) == 0 || sts[len(sts)-1].State == "failure" {
		t.Errorf("victim's gerrit/sync = %+v", sts)
	}
}

// A head too slow to fetch is refused, not fetched over and over.
func TestImportRefusesHeadsTooSlowToFetch(t *testing.T) {
	defer func(a, o time.Duration) { fetchAllTimeout, fetchOneTimeout = a, o }(fetchAllTimeout, fetchOneTimeout)
	fetchAllTimeout, fetchOneTimeout = time.Nanosecond, time.Nanosecond
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	h := s.commit(m1, "big.txt", "big\n", "Big")
	s.push("github.git", h, "refs/heads/big")
	s.pr(70, "big", h, "main", "austin")
	if err := im.Import(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if sts := s.statuses[h]; len(up.uploads) != 0 || len(sts) != 1 || sts[0].State != "failure" || !strings.Contains(sts[0].Description, "too big") {
		t.Errorf("uploads %v, gerrit/sync %+v", up.uploads, sts)
	}
	if why := im.refusedWhy(70, h); why == "" {
		t.Error("not refused")
	}
}

// Closed PRs' heads do not stay in the mirror, and a same-repo branch
// named like a fork PR's ref does not collide with it.
func TestImportRefsDoNotPileUpOrCollide(t *testing.T) {
	s, im, _, m1, _, _ := gerritBornWorld(t, false)
	ctx := context.Background()
	if _, err := im.Mirror.Git(ctx, nil, "fetch", "-q", "origin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := im.Mirror.Git(ctx, nil, "update-ref", "refs/remotes/github/pull/77", m1); err != nil {
		t.Fatal(err)
	}
	h := s.commit(m1, "p.txt", "p\n", "Branch named pull/7")
	s.push("github.git", h, "refs/heads/pull/7")
	s.pr(8, "pull/7", h, "main", "austin")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := im.Mirror.Git(ctx, nil, "rev-parse", "--verify", "--quiet", "refs/remotes/github/pull/77"); err == nil {
		t.Error("a closed PR's head is still in the mirror")
	}
	if got, _ := im.Mirror.Git(ctx, nil, "rev-parse", "refs/remotes/github/heads/pull/7"); strings.TrimSpace(string(got)) != h {
		t.Errorf("branch pull/7 fetched to %q", got)
	}
}

// The bridge never pushes to a branch Gerrit has, whatever the plan says:
// replication owns those.
func TestApplyRefusesBranchesGerritHas(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/release")
	snap := &Snapshot{Branch: "main", DefaultBranch: "main"}
	err := c.applyOne(ctx, snap, Action{Kind: PushBranches, Refs: map[string]string{"release": m0}, Leases: map[string]string{"release": ""}})
	if err == nil || !strings.Contains(err.Error(), "which Gerrit owns") {
		t.Errorf("err = %v", err)
	}
	if out, _ := exec.Command("git", "-C", filepath.Join(s.dir, "github.git"), "rev-parse", "--verify", "--quiet", "refs/heads/release").Output(); len(out) != 0 {
		t.Error("pushed to release")
	}
}

// Who pushed comes from GitHub's activity log for the PR's branch, so a
// restart (no webhook remembered) loses nothing, and the log wins over a
// webhook remembered for the same commit elsewhere.
func TestPusherFromGitHubsActivityLog(t *testing.T) {
	for _, tc := range []struct {
		name, logged, webhook, want string
	}{
		{"after a restart", "austin", "", "austin"},
		{"the log wins", "brt", "austin", ""},
		{"not listed yet: the webhook", "", "austin", "austin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, im, up, m1, g, ids := gerritBornWorld(t, false)
			h := s.commit(s.rebase(g[0], m1), "one.txt", "1 fixed\n", "Fix")
			s.push("github.git", h, "refs/heads/gerrit/"+ids[0])
			s.pulls[10].Head.SHA = h
			if tc.logged != "" {
				s.pushed("o/r", "refs/heads/gerrit/"+ids[0], h, tc.logged)
			}
			s.webhookPush(im.Pushers, 10, tc.webhook)
			if err := im.Import(context.Background(), "main"); err != nil {
				t.Fatal(err)
			}
			if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], tc.want+" ") {
				t.Fatalf("uploads = %v, want one as %q", up.uploads, tc.want)
			}
		})
	}
}

// A PR's first upload is its author's only if they pushed its head: a
// maintainer opening a PR from a stranger's branch, or a branch someone
// else moved before the bridge saw the PR, is the bridge's to upload.
func TestFirstUploadNeedsTheAuthorsPush(t *testing.T) {
	for _, tc := range []struct {
		name, pusher, want string
		opened             bool // a webhook says the author opened it
	}{
		{"the author's push", "austin", "austin", false},
		{"someone else's branch", "brt", "", false},
		{"nobody says", "", "", false},
		// Opening a PR is not pushing its head (fourth audit).
		{"opened by the author", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, im, up, m1, _, _ := gerritBornWorld(t, false)
			s.commit(m1, "new.txt", "n\n", "New")
			s.git("work", "commit", "-q", "--amend", "--no-edit", "--author=Someone Else <else@example.com>")
			h := s.git("work", "rev-parse", "HEAD")
			s.push("github.git", h, "refs/heads/topic")
			s.pr(60, "topic", h, "main", "austin")
			if tc.pusher != "" {
				s.pushed("o/r", "refs/heads/topic", h, tc.pusher)
			}
			if tc.opened {
				im.Pushers.FromDelivery(&github.Delivery{Event: "pull_request", Action: "opened", PR: 60,
					Payload: []byte(`{"sender":{"login":"austin"},"pull_request":{"number":60,"head":{"ref":"topic","sha":"` + h + `","repo":{"full_name":"o/r"}}}}`)})
			}
			if err := im.Import(context.Background(), "main"); err != nil {
				t.Fatal(err)
			}
			if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], tc.want+" ") {
				t.Fatalf("uploads = %v, want one as %q", up.uploads, tc.want)
			}
			// Uploaded as austin, it is austin's; the bridge's keeps the
			// commit's author.
			want := map[string]string{"austin": "austin@example.com", "": "else@example.com"}[tc.want]
			if got := s.git("m.git", "log", "-1", "--format=%ae", strings.Fields(strings.TrimPrefix(up.uploads[0], tc.want+" "))[0]); got != want {
				t.Errorf("author %s, want %s", got, want)
			}
		})
	}
	// Just opened, and nobody says yet: wait (GitHub's log lags a little)
	// rather than make the change the bridge's for good.
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	h := s.commit(m1, "new.txt", "n\n", "New")
	s.push("github.git", h, "refs/heads/topic")
	s.pr(60, "topic", h, "main", "austin").CreatedAt = time.Now()
	if err := im.Import(context.Background(), "main"); err == nil || len(up.uploads) != 0 {
		t.Errorf("err = %v, uploads %v; want a retry and no upload yet", err, up.uploads)
	}
	if sts := s.statuses[h]; len(sts) != 1 || sts[0].State != "pending" {
		t.Errorf("gerrit/sync = %+v", sts)
	}
}

// An import that wants a retry (a fork PR's head not published yet) does
// not hold back exporting the branch's Gerrit changes.
func TestExportDespiteAnImportRetry(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	g := s.commit(m0, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), g, "One\n\nChange-Id: "+cid(1)+"\n", "NEW")
	r := ci.Revisions[g]
	r.Commit.Parents = append(r.Commit.Parents, struct {
		Commit string `json:"commit"`
	}{m0})
	r.Commit.Subject = "One"
	ci.Revisions[g] = r
	fork := s.commit(m0, "fork.txt", "f\n", "From a fork")
	pr := s.pr(52, "main", fork, "main", "brt")
	pr.Head.Repo.FullName = "brt/fork" // not at refs/pull/52/head yet
	c.Importer = &Importer{Project: "p", Gerrit: c.Gerrit, GitHub: c.GitHub, GitHubURL: c.GitHubURL, Mirror: c.Mirror, Store: store,
		Uploader: &fakeUploader{s: s, m: c.Mirror}, Identity: c.Identity, Bridge: gerrit.AccountInfo{AccountID: 1}, DefaultBranch: "main",
		Pushers: &Pushers{}, Logger: slog.Default()}
	err := c.Reconcile(ctx, Key{Branch: "main"})
	if err == nil || !strings.Contains(err.Error(), "importing") {
		t.Errorf("err = %v, want the import's retry", err)
	}
	if got := s.gitOut("github.git", "rev-parse", "refs/heads/gerrit/"+cid(1)); strings.TrimSpace(got) != g {
		t.Errorf("change 1 not exported (gerrit/%s = %q)", cid(1), got)
	}
}

// A change uploaded for a PR whose link was never saved (a refused state
// push, a crash) is adopted and its upload recorded, not refused forever
// as another PR's; one uploaded by someone else, or for another PR, is not.
func TestLostLinkIsAdopted(t *testing.T) {
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	ctx := context.Background()
	up.create = true
	h := s.commit(m1, "new.txt", "n\n", "New")
	s.push("github.git", h, "refs/heads/topic")
	pr := s.pr(60, "topic", h, "main", "austin")
	s.pushed("o/r", "refs/heads/topic", h, "austin")
	fail := true
	im.link = func(ctx context.Context, n int, origin, branch, changeID string, pr *github.PullRequest) error {
		if fail {
			fail = false
			return errors.New("state push refused")
		}
		return updateStateFn(ctx, im.Store, n, "Link", func(st *state.Change) {
			st.State.Change, st.State.ChangeID, st.State.Branch, st.State.Origin, st.State.PR = n, changeID, branch, origin, pr.Number
			st.State.HeadBranch = pr.HeadKey("o/r")
		})
	}
	for range 3 {
		im.Import(ctx, "main")
	}
	n := im.Store.ChangeForPR(60)
	st, _ := im.Store.Load(ctx, n)
	if len(up.uploads) != 1 || n == 0 || len(st.Revisions) != 1 || st.Revisions[0].GitHubSHA != h {
		t.Fatalf("uploads %v, change %d, revisions %+v: want the one upload adopted and recorded", up.uploads, n, st.Revisions)
	}

	// Someone else's change with the PR's Change-Id, one made for another
	// PR, or one no longer open, is not adopted. Nor is the author's own
	// upload when someone else pushed the PR's head (fourth audit): the
	// bridge would have uploaded that as itself.
	s.accounts["bridge"] = im.Bridge
	for _, tc := range []struct {
		uploader, url, status, pusher string
		alone                         bool // the bridge cannot act as anyone
		want                          bool
	}{
		{"austin", pr.HTMLURL, "NEW", "austin", false, true},
		{"bridge", pr.HTMLURL, "NEW", "brt", false, true},
		{"brt", pr.HTMLURL, "NEW", "austin", false, false},
		{"austin", "https://github.com/o/r/pull/61", "NEW", "austin", false, false},
		{"austin", pr.HTMLURL, "ABANDONED", "austin", false, false},
		{"austin", pr.HTMLURL, "NEW", "brt", false, false},
		{"austin", pr.HTMLURL, "NEW", "austin", true, false},
	} {
		h2 := s.commit(m1, "new.txt", fmt.Sprintf("%+v", tc), "New")
		pr2 := s.pr(62, "topic2", h2, "main", "austin")
		pr2.HTMLURL = pr.HTMLURL
		s.pushed("o/r", "refs/heads/topic2", h2, tc.pusher)
		up.alone = tc.alone
		ci := &gerrit.ChangeInfo{Number: 9, Status: tc.status, Revisions: map[string]gerrit.RevisionInfo{"x": {Number: 1, Uploader: s.accounts[tc.uploader],
			Commit: gerrit.CommitInfo{Message: "T\n\nGitHub-PR: " + tc.url + "\nChange-Id: I1\n"}}}}
		if got, err := im.ownUpload(ctx, ci, pr2, h2); err != nil || got != tc.want {
			t.Errorf("%+v: adopted %v (err %v)", tc, got, err)
		}
	}
	up.alone = false
	// Nor one another PR is linked to.
	s.save(ctx, im.Store, 9, &state.Change{State: state.ChangeState{Change: 9, PR: 61}})
	ci := &gerrit.ChangeInfo{Number: 9, Status: "NEW", Revisions: map[string]gerrit.RevisionInfo{"x": {Number: 1, Uploader: s.accounts["austin"],
		Commit: gerrit.CommitInfo{Message: "T\n\nGitHub-PR: " + pr.HTMLURL + "\nChange-Id: I1\n"}}}}
	if got, _ := im.ownUpload(ctx, ci, pr, h); got {
		t.Error("adopted a change another PR is linked to")
	}
}

// Draft, then ready again before the bridge recorded the first toggle as
// in sync: the second is a toggle too, not Gerrit's to undo.
func TestDraftThenReadyQuickly(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	ctx := context.Background()
	s.pulls[70].Draft = true
	s.activity[70] = github.PRActivity{DraftActor: "austin"}
	if changed, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("draft: changed=%v err=%v", changed, err)
	}
	// Re-observed: Gerrit is WIP now, and the PR is ready again.
	st, err := c.Store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	snap.State[7], snap.Changes[7].WIP = st, true
	s.pulls[70].Draft = false
	if changed, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("ready: changed=%v err=%v", changed, err)
	}
	if len(s.calls) != 2 || s.calls[1] != "ready 7 as austin" || s.changes[7].WorkInProgress {
		t.Errorf("calls = %v, wip = %v", s.calls, s.changes[7].WorkInProgress)
	}
}

// The same for two quick title edits.
func TestTwoQuickPREdits(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	ctx := context.Background()
	s.pulls[70].Title = "Seven, better"
	s.activity[70] = github.PRActivity{TextEditor: "austin"}
	if changed, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("first: changed=%v err=%v", changed, err)
	}
	st, err := c.Store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	ci := s.changes[7]
	snap.State[7] = st
	snap.Changes[7].Message = ci.Revisions[ci.CurrentRevision].Commit.Message
	s.pulls[70].Title = "Seven, best"
	if changed, err := c.mirrorPREdits(ctx, snap, map[int]bool{}); err != nil || !changed {
		t.Fatalf("second: changed=%v err=%v comments=%v", changed, err, s.comments[70])
	}
	if got := ci.Revisions[ci.CurrentRevision].Commit.Message; !strings.HasPrefix(got, "Seven, best\n") {
		t.Errorf("message %q", got)
	}
}

// Merged in Gerrit (rebased on submit, so a new patch set): the merged key
// swaps the PR's history head for the merged commit and bases it on main
// at once, so replication's push shows it merged.
func TestMergedKeyReadiesPRsForReplication(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	m1 := s.commit(m0, "b.txt", "b\n", "main moves")
	s.push("gerrit.git", m1, "refs/heads/main")
	g1 := s.commit(m0, "one.txt", "1\n", "One\n\nChange-Id: "+cid(1)+"\n")
	h1 := s.commit(g1, "one.txt", "1b\n", "Patch set 2 from Gerrit") // GitHub history
	merged := s.commit(m1, "one.txt", "1b\n", "One\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), merged, "One\n\nChange-Id: "+cid(1)+"\n", "MERGED")
	ci.Revisions[merged] = gerritRev(ci.Revisions[merged], 3)
	s.push("gerrit.git", merged, ci.Revisions[merged].Ref)
	s.push("github.git", h1, "refs/heads/gerrit/"+cid(1))
	s.pr(10, "gerrit/"+cid(1), h1, "gerrit/somewhere", "bot")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "gerrit", HeadBranch: "gerrit/" + cid(1), Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 2, GerritSHA: "ps2", GitHubSHA: h1}}})
	d := &Dispatcher{Code: c, Store: store}
	if err := d.Reconcile(ctx, Key{Merged: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.ref("github.git", "refs/heads/gerrit/"+cid(1)); got != merged {
		t.Errorf("head = %s, want the merged commit %s", short(got), short(merged))
	}
	if s.pulls[10].Base.Ref != "main" {
		t.Errorf("base = %s", s.pulls[10].Base.Ref)
	}
}

// The same with a Link footer, which Gerrit also reads as a Change-Id.
func TestImportRefusesLinkFooterToAnotherChange(t *testing.T) {
	s, im, up, m1, _, ids := gerritBornWorld(t, false)
	ctx := context.Background()
	im.Bridge.Email = "austin@example.com"
	evil := s.commit(m1, "evil.txt", "x\n", "Evil\n\nChange-Id: "+cid(5)+"\nLink: http://127.0.0.1:18080/gerrit/id/"+ids[0]+"\n")
	s.push("github.git", evil, "refs/heads/evil")
	s.pr(50, "evil", evil, "main", "stranger")
	before := s.changes[1].CurrentRevision
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if s.changes[1].CurrentRevision != before || len(up.uploads) != 0 {
		t.Errorf("uploads %v; change 1 moved: %v", up.uploads, s.changes[1].CurrentRevision != before)
	}
}

// A pure rebase borrows the uploader's trust only onto a parent that is
// the branch's or theirs: here brt uploads the parent change, then
// restacks austin's change onto it, and that layer is the bridge's.
func TestPureRebaseOntoSomeoneElsesParentIsTheBridges(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, true)
	ctx := context.Background()
	r := s.changes[1].Revisions[g[0]]
	r.Uploader = s.accounts["brt"]
	s.changes[1].Revisions[g[0]] = r
	r1 := s.rebase(g[0], m1)
	r2 := s.rebase(g[1], r1)
	s.push("github.git", r1, "refs/heads/gerrit/"+ids[0])
	s.push("github.git", r2, "refs/heads/gerrit/"+ids[1])
	s.pulls[10].Head.SHA, s.pulls[20].Head.SHA = r1, r2
	s.webhookPush(im.Pushers, 10, "brt")
	s.webhookPush(im.Pushers, 20, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 2 || !strings.HasPrefix(up.uploads[0], "brt ") || !strings.HasPrefix(up.uploads[1], " ") {
		t.Errorf("uploads = %v, want brt's own layer as brt and austin's as the bridge", up.uploads)
	}
}

// A parent someone else uploaded on the uploader's behalf (Gerrit's
// rebase) is not the uploader's to vouch for.
func TestPureRebaseOntoParentUploadedOnBehalfIsTheBridges(t *testing.T) {
	s, im, up, m1, g, ids := gerritBornWorld(t, true)
	ctx := context.Background()
	r1 := s.rebase(g[0], m1)
	s.push("gerrit.git", r1, "refs/changes/01/1/2")
	brt := s.accounts["brt"]
	s.changes[1].Revisions[r1] = gerrit.RevisionInfo{Number: 2, Ref: "refs/changes/01/1/2", Uploader: s.accounts["austin"], RealUploader: &brt,
		Commit: gerrit.CommitInfo{Message: "One\n\nChange-Id: " + ids[0] + "\n"}}
	s.changes[1].CurrentRevision = r1
	st, _ := im.Store.Load(ctx, 1)
	st.Revisions = append(st.Revisions, state.Revision{PatchSet: 2, GerritSHA: r1, GitHubSHA: r1})
	if err := im.Store.Save(ctx, 1, st, "exported"); err != nil {
		t.Fatal(err)
	}
	s.push("github.git", r1, "refs/heads/gerrit/"+ids[0])
	s.pulls[10].Head.SHA = r1
	r2 := s.rebase(g[1], r1)
	s.push("github.git", r2, "refs/heads/gerrit/"+ids[1])
	s.pulls[20].Head.SHA = r2
	s.webhookPush(im.Pushers, 20, "brt")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 || !strings.HasPrefix(up.uploads[0], " ") {
		t.Errorf("uploads = %v, want austin's layer rebased onto brt's rebase uploaded by the bridge", up.uploads)
	}
}

// A change touching .github/ (itself or through a parent change) is
// withheld from GitHub, with one notice in Gerrit per patch set.
func TestChangesTouchingGitHubConfigAreWithheld(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	os.MkdirAll(filepath.Join(s.dir, "work", ".github", "workflows"), 0o755)
	wf := s.commit(m0, ".github/workflows/x.yml", "on: push\n", "Workflow\n\nChange-Id: "+cid(1)+"\n")
	above := s.commit(wf, "b.txt", "b\n", "Above\n\nChange-Id: "+cid(2)+"\n")
	plain := s.commit(m0, "c.txt", "c\n", "Plain\n\nChange-Id: "+cid(3)+"\n")
	s.change(1, cid(1), wf, "Workflow", "NEW")
	s.change(2, cid(2), above, "Above", "NEW")
	s.change(3, cid(3), plain, "Plain", "NEW")
	for _, ref := range []string{"refs/changes/01/1/1", "refs/changes/02/2/1", "refs/changes/03/3/1", "refs/heads/main"} {
		s.git("m.git", "fetch", "-q", "origin", "+"+ref+":"+strings.Replace(ref, "refs/", "refs/remotes/gerrit/", 1))
	}
	snap := &Snapshot{Changes: map[int]*GerritChange{
		1: {Number: 1, Status: "NEW", Revision: wf, PatchSet: 1},
		2: {Number: 2, Status: "NEW", Revision: above, PatchSet: 1},
		3: {Number: 3, Status: "NEW", Revision: plain, PatchSet: 1},
	}, State: map[int]*state.Change{}}
	for range 2 {
		if err := c.withhold(ctx, snap, "main", nil); err != nil {
			t.Fatal(err)
		}
		for n := range snap.Changes {
			snap.State[n], _ = store.Load(ctx, n)
		}
	}
	if !snap.Withheld[1] || !snap.Withheld[2] || snap.Withheld[3] {
		t.Errorf("withheld = %v, want 1 and 2", snap.Withheld)
	}
	if strings.Join(s.calls, ",") != "review 1,review 2" {
		t.Errorf("notices = %v, want one each for 1 and 2", s.calls)
	}

	// CODEOWNERS too; a change below that adds config and a change that
	// takes it out again; an old base that would bring back config the
	// branch has since changed; and history unrelated to the branch.
	os.MkdirAll(filepath.Join(s.dir, "work", "docs"), 0o755)
	owners := s.commit(m0, "CODEOWNERS", "* @someone\n", "Owners")
	docsOwners := s.commit(m0, "docs/CODEOWNERS", "* @someone\n", "Owners")
	s.git("work", "checkout", "-q", "--detach", wf)
	s.git("work", "rm", "-q", ".github/workflows/x.yml")
	s.git("work", "commit", "-q", "-m", "Undo")
	undo := s.git("work", "rev-parse", "HEAD")
	s.git("work", "checkout", "-q", "--detach", m0)
	os.MkdirAll(filepath.Join(s.dir, "work", ".github", "workflows"), 0o755)
	m1 := s.commit(m0, ".github/workflows/y.yml", "on: [push]\n", "Main's own workflow")
	oldBase := s.commit(m1, "e.txt", "e\n", "On main before its workflow changed")
	m2 := s.commit(m1, ".github/workflows/y.yml", "on: {push: {branches: [main]}}\n", "Main's workflow only on main")
	s.push("gerrit.git", m2, "refs/heads/main")
	s.git("m.git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/gerrit/heads/main")
	s.git("work", "checkout", "-q", "--orphan", "lonely")
	s.git("work", "rm", "-rq", "--cached", ".")
	s.git("work", "clean", "-fdq")
	orphan := s.commit("", "z.txt", "z\n", "Unrelated")
	onM2 := s.commit(m2, "d.txt", "d\n", "On the new main")
	for _, ref := range []string{owners, docsOwners, undo, orphan, onM2, plain, oldBase} {
		s.push("gerrit.git", ref, "refs/heads/tmp")
		s.git("m.git", "fetch", "-q", "origin", "+refs/heads/tmp:refs/remotes/tmp/"+ref)
	}
	for _, tc := range []struct {
		name, rev string
		want      bool
	}{
		{"root CODEOWNERS", owners, true}, {"docs/CODEOWNERS", docsOwners, true}, {"added below, taken out", undo, true},
		{"based on main before its workflow changed", oldBase, true}, {"based on main before it had one", plain, false},
		{"on the new main", onM2, false}, {"unrelated history", orphan, false},
	} {
		if w, err := c.changesConfig(ctx, "main", tc.rev); err != nil || w != tc.want {
			t.Errorf("%s: withheld %v, err %v; want %v", tc.name, w, err, tc.want)
		}
	}
	// A change that cannot be checked is withheld; a notice Gerrit will
	// not take is not retried, and fails nothing.
	s.calls = nil
	s.refuseReview = map[int]int{6: 400, 7: 503}
	snap = &Snapshot{Changes: map[int]*GerritChange{
		5: {Number: 5, Status: "NEW", Revision: strings.Repeat("0", 40), PatchSet: 1},
		6: {Number: 6, Status: "NEW", Revision: owners, PatchSet: 1},
		7: {Number: 7, Status: "NEW", Revision: docsOwners, PatchSet: 1},
	}, State: map[int]*state.Change{}}
	for range 2 {
		if err := c.withhold(ctx, snap, "main", nil); err != nil {
			t.Fatal(err)
		}
		for n := range snap.Changes {
			snap.State[n], _ = store.Load(ctx, n)
		}
	}
	if !snap.Withheld[5] || !snap.Withheld[6] || !snap.Withheld[7] {
		t.Errorf("withheld = %v, want 5, 6 and 7", snap.Withheld)
	}
	if got := strings.Join(s.calls, ","); got != "review 6,review 7,review 7" {
		t.Errorf("notices = %v: want 6's refusal taken as final, 7's retried", got)
	}
	// And whatever a plan says, the push is refused.
	err := c.applyOne(ctx, &Snapshot{Branch: "main", DefaultBranch: "main"},
		Action{Kind: PushBranches, Refs: map[string]string{"gerrit/" + cid(1): wf}, Leases: map[string]string{"gerrit/" + cid(1): ""}})
	if err == nil || !strings.Contains(err.Error(), ".github/") {
		t.Errorf("push: %v", err)
	}
}

// A change whose push would carry GitHub history that changed .github/
// (a PR's commits below it that added a workflow and took it out again,
// which its Gerrit squash does not have) is withheld, not pushed; the
// rest of the branch is exported (fourth audit: the guard used to stop
// the whole branch, every reconcile).
func TestRefusedPushWithholdsOnlyItsChange(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	os.MkdirAll(filepath.Join(s.dir, "work", ".github", "workflows"), 0o755)
	h1 := s.commit(m0, ".github/workflows/x.yml", "on: push\n", "Workflow")
	s.git("work", "rm", "-q", ".github/workflows/x.yml")
	h2 := s.commit(h1, "f.txt", "f\n", "Feature, workflow gone")
	s.push("github.git", h2, "refs/heads/feature")
	s.pr(10, "feature", h2, "main", "brt")
	change := func(n int, parent, file, subject string) string {
		msg := subject + "\n\nChange-Id: " + cid(n) + "\n"
		rev := s.commit(parent, file, file+"\n", msg)
		ci := s.change(n, cid(n), rev, msg, "NEW")
		r := ci.Revisions[rev]
		r.Commit.Parents = append(r.Commit.Parents, struct {
			Commit string `json:"commit"`
		}{parent})
		r.Commit.Subject = subject
		ci.Revisions[rev] = r
		return rev
	}
	sq := change(1, m0, "f.txt", "Feature") // the PR's squash: no workflow
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, ChangeID: cid(1), PR: 10, Branch: "main", Origin: "github",
		HeadBranch: "feature", Lifecycle: "NEW"}, Revisions: []state.Revision{{PatchSet: 1, GerritSHA: sq, GitHubSHA: h2}}})
	above := change(2, sq, "c.txt", "Above")
	plain := change(3, m0, "p.txt", "Plain")
	if err := c.Reconcile(ctx, Key{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(s.gitOut("github.git", "rev-parse", "refs/heads/gerrit/"+cid(3))); got != plain {
		t.Errorf("change 3 not exported (gerrit/%s = %q)", cid(3), got)
	}
	if got := strings.TrimSpace(s.gitOut("github.git", "for-each-ref", "--format=%(refname)", "--contains", h1)); got != "refs/heads/feature" {
		t.Errorf("the workflow's history is on %s, want only the PR's own branch", got)
	}
	if got := s.gitOut("github.git", "rev-parse", "--verify", "-q", "refs/heads/gerrit/"+cid(2)); got != "" {
		t.Errorf("change 2 (%s) exported as %s", short(above), got)
	}
	if !slices.Contains(s.calls, "review 2") {
		t.Errorf("calls = %v, want a withheld notice on change 2", s.calls)
	}
	// The same for a frozen base; a refused push that is no one change's
	// is left for applyOne to refuse.
	p := &Plan{Desired: map[int]*Desired{4: {Change: 4, Head: "gerrit/" + cid(4), Frozen: "gerrit-base/" + cid(4)}},
		Actions: []Action{{Kind: PushBranches, Refs: map[string]string{"gerrit-base/" + cid(4): h2, "gerrit/" + cid(4): plain, "elsewhere": h2}}}}
	if got, err := c.refusedPushes(ctx, &Snapshot{Branch: "main"}, p); err != nil || !reflect.DeepEqual(got, map[int]bool{4: true}) {
		t.Errorf("refused %v, err %v; want change 4", got, err)
	}
}

// New PRs beyond an owner's budget wait; begun ones, PRs already made and
// trusted owners' do not; creating a PR spends from the budget.
func TestNewPRsWithinTheOwnersBudget(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, _ := s.code(ctx)
	c.PRBudget = &Budget{N: 2, Per: time.Hour, Trusted: func(_ context.Context, a int) (bool, bool) { return a == 7, true }}
	snap := &Snapshot{Changes: map[int]*GerritChange{}, State: map[int]*state.Change{}, GitHubBranches: map[string]string{"gerrit/" + cid(4): "x"}}
	for n, owner := range map[int]int{1: 9, 2: 9, 3: 9, 4: 9, 5: 9, 6: 7, 7: 7, 8: 7} {
		snap.Changes[n] = &GerritChange{Number: n, ChangeID: cid(n), Status: "NEW", OwnerID: owner}
	}
	snap.State[5] = &state.Change{State: state.ChangeState{PR: 50}}
	c.deferOverBudget(ctx, snap)
	if want := map[int]bool{3: true}; !reflect.DeepEqual(snap.Deferred, want) {
		t.Errorf("deferred = %v, want %v", snap.Deferred, want)
	}
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("github.git", m0, "refs/heads/gerrit/"+cid(1))
	snap.Changes[1].Revision = m0
	snap.Branch, snap.Repo = "main", "o/r"
	if err := c.applyOne(ctx, snap, Action{Kind: CreatePR, Change: 1, New: &github.NewPull{Title: "One", Head: "gerrit/" + cid(1), Base: "main"}}); err != nil {
		t.Fatal(err)
	}
	if left := c.PRBudget.Left(ctx, 9); left != 1 {
		t.Errorf("budget left %d after a PR, want 1", left)
	}
}

// Gerrit lets anyone add a patch set to any change; only the owner's, the
// bridge's and trusted members' reach a PR author's branch.
func TestForeignPatchSetsAreMarked(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	c.BridgeAccount = 1
	unknown := false
	c.Trusted = func(_ context.Context, account int) (bool, bool) { return account == 7, !unknown }
	snap := &Snapshot{Changes: map[int]*GerritChange{}, State: map[int]*state.Change{}}
	for n, ids := range map[int][3]int{ // owner, uploader, real uploader
		1: {5, 5, 0}, // the owner's own
		2: {5, 9, 0}, // someone else's
		3: {5, 7, 0}, // a trusted member's
		4: {5, 5, 9}, // someone else's, on the owner's behalf
		5: {5, 9, 0}, // someone else's, on a Gerrit-born change
		6: {5, 1, 0}, // the bridge's (an import)
	} {
		snap.Changes[n] = &GerritChange{Number: n, Status: "NEW", Revision: fmt.Sprintf("r%d", n), PatchSet: 2,
			OwnerID: ids[0], UploaderID: ids[1], RealUploaderID: ids[2]}
		origin := "github"
		if n == 5 {
			origin = "gerrit"
		}
		snap.State[n] = &state.Change{State: state.ChangeState{Change: n, Origin: origin}}
		s.save(ctx, store, n, snap.State[n])
	}
	for range 2 {
		if err := c.markForeign(ctx, snap); err != nil {
			t.Fatal(err)
		}
		for n := range snap.Changes {
			snap.State[n], _ = store.Load(ctx, n)
		}
	}
	if want := map[int]bool{2: true, 4: true}; !reflect.DeepEqual(snap.Foreign, want) {
		t.Errorf("foreign = %v, want %v", snap.Foreign, want)
	}
	if strings.Join(s.calls, ",") != "review 2,review 4" {
		t.Errorf("notices = %v, want one each for 2 and 4", s.calls)
	}
	// If the trusted group cannot be read, nobody but the owner and the
	// bridge is trusted, and nothing is claimed in a notice.
	unknown, s.calls = true, nil
	snap.Changes[3].PatchSet = 3
	if err := c.markForeign(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Foreign[3] || len(s.calls) != 0 {
		t.Errorf("group unreadable: foreign = %v, notices %v", snap.Foreign, s.calls)
	}
}

// Who edited is asked after the edit was seen. If the PR was edited again
// in between, the editor named may not be this edit's: nothing is done
// this round.
func TestPREditedAgainWhileMirroring(t *testing.T) {
	s, c, snap := editsWorld(t, "gerrit")
	s.may[7] = append(s.may[7], "austin")
	s.pulls[70].Title = "Seven, by someone"
	snapTitle := *s.pulls[70]
	snap.PRs[70] = &snapTitle // what observe saw
	s.activity[70] = github.PRActivity{TextEditor: "austin"}
	s.onActivity = func() { s.pulls[70].Title = "Seven, by austin" } // austin edits again
	changed, err := c.mirrorPREdits(context.Background(), snap, map[int]bool{})
	if err != nil || !changed || len(s.calls) != 0 {
		t.Errorf("changed=%v err=%v calls=%v; want nothing sent, and a re-observe", changed, err, s.calls)
	}
}

// GitHub publishes a new PR's refs/pull/N/head moments after the PR. A
// head that cannot be fetched for such a reason is not refused: the
// others import, and a retry imports it.
func TestImportRetriesAHeadNotPublishedYet(t *testing.T) {
	s, im, up, m1, _, _ := gerritBornWorld(t, false)
	ctx := context.Background()
	fork := s.commit(m1, "fork.txt", "f\n", "From a fork")
	pr := s.pr(52, "main", fork, "main", "brt")
	pr.Head.Repo.FullName = "brt/fork" // its head is only at refs/pull/52/head, not there yet
	s.pushed("brt/fork", "refs/heads/main", fork, "brt")
	good := s.commit(m1, "good.txt", "g\n", "Good")
	s.push("github.git", good, "refs/heads/good")
	s.pushed("o/r", "refs/heads/good", good, "austin")
	s.pr(51, "good", good, "main", "austin")
	if err := im.Import(ctx, "main"); err == nil {
		t.Error("no retry asked for")
	}
	if len(up.uploads) != 1 {
		t.Errorf("uploads %v, want the good PR's", up.uploads)
	}
	if sts := s.statuses[fork]; len(sts) != 0 {
		t.Errorf("fork PR refused: %+v", sts)
	}
	s.push("github.git", fork, "refs/pull/52/head")
	if err := im.Import(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	byBRT := 0
	for _, u := range up.uploads {
		if strings.HasPrefix(u, "brt ") {
			byBRT++
		}
	}
	if byBRT != 1 {
		t.Errorf("uploads %v, want the fork PR's (as brt) once", up.uploads)
	}
}

// What observe says about a GitHub-born change whose patch set someone
// else uploaded in Gerrit.
func TestObserveMarksForeignPatchSets(t *testing.T) {
	s := newSites(t)
	ctx := context.Background()
	c, store := s.code(ctx)
	c.Trusted = func(context.Context, int) (bool, bool) { return false, true }
	m0 := s.commit("", "a.txt", "a\n", "base")
	s.push("gerrit.git", m0, "refs/heads/main")
	s.push("github.git", m0, "refs/heads/main")
	h1 := s.commit(m0, "b.txt", "b\n", "Mine")
	s.push("github.git", h1, "refs/heads/feature")
	s.pr(10, "feature", h1, "main", "brt")
	ps1 := s.commit(m0, "b.txt", "b\n", "Mine\n\nChange-Id: "+cid(1)+"\n")
	ps2 := s.commit(m0, "b.txt", "theirs\n", "Mine\n\nChange-Id: "+cid(1)+"\n")
	ci := s.change(1, cid(1), ps1, "Mine", "NEW")
	ci.Owner = gerrit.AccountInfo{AccountID: 5, Username: "brt"}
	ps2Rev := gerrit.RevisionInfo{Number: 2, Ref: "refs/changes/01/1/2", Uploader: gerrit.AccountInfo{AccountID: 9, Username: "mallory"}}
	ps2Rev.Commit.Parents = append(ps2Rev.Commit.Parents, struct {
		Commit string `json:"commit"`
	}{m0})
	ci.Revisions[ps2] = ps2Rev
	ci.CurrentRevision = ps2
	s.push("gerrit.git", ps2, "refs/changes/01/1/2")
	s.save(ctx, store, 1, &state.Change{State: state.ChangeState{Change: 1, PR: 10, Branch: "main", Origin: "github", HeadBranch: "feature", Lifecycle: "NEW"},
		Revisions: []state.Revision{{PatchSet: 1, GerritSHA: ps1, GitHubSHA: h1}}})
	snap, err := c.observe(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Foreign[1] {
		t.Errorf("foreign = %v; mallory's patch set 2 is not the author's", snap.Foreign)
	}
	// The owner's own is theirs; on their behalf by mallory, it is not.
	for _, real := range []*gerrit.AccountInfo{nil, {AccountID: 9, Username: "mallory"}} {
		ps2Rev.Uploader, ps2Rev.RealUploader = ci.Owner, real
		ci.Revisions[ps2] = ps2Rev
		if snap, err = c.observe(ctx, "main", nil); err != nil {
			t.Fatal(err)
		}
		if snap.Foreign[1] != (real != nil) {
			t.Errorf("real uploader %v: foreign = %v", real, snap.Foreign)
		}
	}
}
