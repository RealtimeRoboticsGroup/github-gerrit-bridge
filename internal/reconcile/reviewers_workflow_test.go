package reconcile

// A harness for whole review lifecycles under the reviewer sync (design
// §6.6, "Reviewer added / attention"): people act on either side, as
// Gerrit 3.14 and GitHub record it, and the comment sync runs after
// every step, every few, or once at the end, with failures injected.
//
// Synced after every step, each step's want is checked: the sides as the
// design says they are then. However batched, every sync must keep:
//   - the hard rule: while nobody has acted in Gerrit, the bridge never
//     writes to GitHub for reviewers;
//   - every write the bridge makes has a cause among what people did
//     since (each cause used at most once: no repeated asks for one
//     event), and none is made twice in one sync (a withdrawal and an ask
//     again for a new patch set may both be);
//   - the bridge never undoes what a person did last about someone, on
//     the side they did it, unless people acted on both sides about them
//     since the last sync (which the bridge cannot order);
//   - at quiescence, a second sync writes nothing, and the sides agree,
//     but where the design says they may not (a turn given by someone
//     the bridge does not carry, Gerrit's ready mirrored from GitHub; a
//     merged or closed PR).
//
// Each workflow (reviewers_workflows_test.go) runs in every wfVariant; a
// variant a known bug fails is skipped, naming its repro in
// reviewers_workflow_bugs_test.go, until the bug is fixed.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// wfPerson is someone in a workflow: zero g or gh, no account there.
type wfPerson struct {
	g  gerrit.AccountInfo
	gh github.User
}

var wfPeople = map[string]wfPerson{
	"mike":     {mike, mikeGH},
	"jane":     {jane, janeGH},
	"owen":     {ownerGA, ownerGH},
	"austin":   {austin, github.User{Login: "austin-gh", ID: 80, Type: "User"}},
	"stranger": {stranger, github.User{Login: "stranger-gh", ID: 83, Type: "User"}},
	"new":      {gh: github.User{Login: "new-gh", ID: 81, Type: "User"}}, // no Gerrit account
	"nogh":     {g: gerrit.AccountInfo{AccountID: 27, Username: "nogh"}}, // no GitHub account
	"Copilot":  {gh: github.User{Login: "Copilot", ID: 82, Type: "Bot"}},
}

// wfTrusted are those in CI's trusted group.
var wfTrusted = map[string]bool{"austin": true, "owen": true}

type wfStep struct {
	name string
	do   func(w *wf)
	// want is the sides after a sync right after this step, synced after
	// every step: "G[...] H[...]", whose turn it is in Gerrit (not the
	// owner's) and who is asked on GitHub, by name.
	want string
	// fail is the failures of the sync that follows this step, whenever
	// it comes (comma-separated withFailure kinds).
	fail string
	// check, if set, is more to check after the sync, every step synced.
	check func(w *wf) []string
}

// wf is a workflow's world: a comment sync where GitHub owns the
// branches, and what the harness knows of what people did.
type wf struct {
	t      *testing.T
	e      *commentsEnv
	owner  string // the Gerrit change's owner
	author string // the PR's author on GitHub
	ps     int    // the newest patch set
	// writes are the bridge's writes in this sync ("gh+login" asks, "gh-"
	// withdraws, "g+" gives the turn or makes a reviewer, "g-" takes it,
	// "note:" says someone has no Gerrit account), by name.
	writes []string
	// allow counts the writes people's doings since may cause.
	allow map[string]int
	// last is the last state a person set about each, as "g+", "g-",
	// "gh+", "gh-"; touched the sides people acted on about each since
	// the last clean sync.
	last    map[string]string
	touched map[string]map[string]bool
	// uncarried: their Gerrit turn is one the design says is not carried
	// (given or taken by someone untrusted, or by Gerrit for a ready
	// mirrored from GitHub).
	uncarried map[string]bool
	// gitHubOnly: nobody has acted in Gerrit yet.
	gitHubOnly bool
	syncs      int
	// pending are patch sets uploaded in Gerrit the bridge has yet to
	// record exporting.
	pending map[int]wfPending
	errs    []string
}

// wfGerrit records the bridge's Gerrit writes for the reviewer sync.
type wfGerrit struct {
	*fakeGerrit
	w *wf
}

func (g wfGerrit) AddToAttentionSet(ctx context.Context, n int, user, reason string) error {
	err := g.fakeGerrit.AddToAttentionSet(ctx, n, user, reason)
	if err == nil {
		g.w.writes = append(g.w.writes, "g+"+g.w.byUser(user))
	}
	return err
}

func (g wfGerrit) RemoveFromAttentionSet(ctx context.Context, n int, account int, reason string) error {
	_, in := g.attention[account]
	err := g.fakeGerrit.RemoveFromAttentionSet(ctx, n, account, reason)
	if err == nil && in {
		g.w.writes = append(g.w.writes, "g-"+g.w.byID(account))
	}
	return err
}

func (g wfGerrit) SetReview(ctx context.Context, n int, rev string, in gerrit.ReviewInput) error {
	err := g.fakeGerrit.SetReview(ctx, n, rev, in)
	if err == nil {
		for _, r := range in.Reviewers {
			g.w.writes = append(g.w.writes, "g+"+g.w.byUser(r.Reviewer))
		}
		if l, ok := strings.CutPrefix(in.Message, "@"); ok && strings.Contains(in.Message, "was asked to review on GitHub") {
			l, _, _ = strings.Cut(l, " ")
			g.w.writes = append(g.w.writes, "note:"+g.w.byLogin(l))
		}
	}
	return err
}

func (w *wf) byUser(user string) string {
	for n, p := range wfPeople {
		if p.g.AccountID != 0 && strings.EqualFold(p.g.Username, user) {
			return n
		}
	}
	return "?" + user
}

func (w *wf) byID(id int) string {
	for n, p := range wfPeople {
		if p.g.AccountID != 0 && p.g.AccountID == id {
			return n
		}
	}
	return fmt.Sprintf("?%d", id)
}

func (w *wf) byLogin(login string) string {
	for n, p := range wfPeople {
		if p.gh.ID != 0 && strings.EqualFold(p.gh.Login, login) {
			return n
		}
	}
	return "?" + login
}

// newWF is a workflow on owner's change, whose PR author is author
// ("owen": opened on GitHub; "austin": born in Gerrit, opened as him).
func newWF(t *testing.T, owner string) *wf {
	e := newCommentsEnv(t)
	e.c.GitHubOwned = true
	e.c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &fakeGitHubAccounts{m: map[string]github.User{}}
	users := &Users{Overrides: map[string]string{}}
	e.g.accounts = map[string]gerrit.AccountInfo{}
	e.h.ids = map[string]int64{}
	for _, p := range wfPeople {
		if p.gh.ID != 0 {
			e.h.ids[p.gh.Login] = p.gh.ID
		}
		if p.g.AccountID != 0 {
			e.g.accounts[p.g.Username] = p.g
		}
		if p.g.AccountID != 0 && p.gh.ID != 0 {
			a.m[p.g.Username] = p.gh
			users.Overrides[p.gh.Login] = p.g.Username
		}
	}
	e.c.Reviewers = a
	e.c.Users = users
	e.c.Trusted = func(_ context.Context, id int) (bool, bool) {
		return id == austin.AccountID || id == ownerGA.AccountID, true
	}
	e.g.owner = wfPeople[owner].g
	e.h.author = wfPeople[owner].gh
	e.g.reviewers = map[string][]gerrit.AccountInfo{}
	e.g.attention, e.g.removed = map[int]gerrit.AttentionSetInfo{}, map[int]gerrit.AttentionSetInfo{}
	w := &wf{t: t, e: e, owner: owner, author: owner, ps: 2, allow: map[string]int{}, last: map[string]string{},
		touched: map[string]map[string]bool{}, uncarried: map[string]bool{}, gitHubOnly: true}
	e.c.Gerrit = wfGerrit{e.g, w}
	return w
}

// ---- state ----

func (w *wf) turn(n string) bool {
	_, in := w.e.g.attention[wfPeople[n].g.AccountID]
	return wfPeople[n].g.AccountID != 0 && in
}

func (w *wf) requested(n string) bool {
	id := wfPeople[n].gh.ID
	return id != 0 && slices.ContainsFunc(w.e.h.requested, func(u github.User) bool { return u.ID == id })
}

func (w *wf) reviewer(n string) bool {
	id := wfPeople[n].g.AccountID
	return id != 0 && slices.ContainsFunc(w.e.g.reviewers["REVIEWER"], func(a gerrit.AccountInfo) bool { return a.AccountID == id })
}

func (w *wf) open() bool {
	return !w.e.h.closed && !w.e.h.merged && (w.e.g.status == "" || w.e.g.status == "NEW")
}

// view is "G[...] H[...]": whose turn it is in Gerrit (but the owner's),
// and who is asked on GitHub.
func (w *wf) view() string {
	var g, h []string
	for _, n := range wfNames() {
		if n != w.owner && w.turn(n) {
			g = append(g, n)
		}
		if w.requested(n) {
			h = append(h, n)
		}
	}
	return fmt.Sprintf("G%v H%v", g, h)
}

// wfNames are the people's names, sorted.
func wfNames() []string { return slices.Sorted(maps.Keys(wfPeople)) }

func (w *wf) notes(n string) int {
	c := 0
	for _, m := range w.e.g.messages {
		if strings.HasPrefix(m.Message, "Patch Set") && strings.Contains(m.Message, "@"+wfPeople[n].gh.Login+" was asked to review on GitHub") {
			c++
		}
	}
	return c
}

// ---- what people do ----

func (w *wf) may(keys ...string) {
	for _, k := range keys {
		w.allow[k]++
	}
}

// did is a person setting n's state on side ("g" or "gh") to in.
func (w *wf) did(n, side string, in bool) {
	w.last[n] = side + map[bool]string{true: "+", false: "-"}[in]
	if w.touched[n] == nil {
		w.touched[n] = map[string]bool{}
	}
	w.touched[n][side] = true
	w.uncarried[n] = false
}

// mayAsk is n being asked on GitHub, if the bridge carries what by did.
func (w *wf) mayAsk(by, n string) {
	p := wfPeople[n]
	if (wfTrusted[by] || by == "") && n != w.owner && n != w.author && p.gh.ID != 0 && p.gh.Type != "Bot" {
		w.may("gh+" + n)
	} else {
		w.uncarried[n] = true
	}
}

// mayWithdraw is n's request withdrawn, if the bridge carries what by did.
func (w *wf) mayWithdraw(by, n string) {
	if wfTrusted[by] || by == n {
		w.may("gh-" + n)
	} else {
		w.uncarried[n] = true
	}
}

// inGerrit marks a person acting in Gerrit.
func (w *wf) inGerrit() { w.gitHubOnly = false }

// setTurn is Gerrit putting n in (or out of) the attention set at ts;
// false: no change (Gerrit skips who is in already, or out).
func (w *wf) setTurn(n, ts, reason string, in bool) bool {
	if w.turn(n) == in {
		return false // Gerrit: no change
	}
	w.e.g.setAttention(wfPeople[n].g, ts, reason, in)
	return true
}

// replied is Gerrit's own rules for by's reply at ts: by has had their
// turn, and the owner, if someone else replied, has it.
func (w *wf) replied(by, ts string) {
	if by != w.owner && w.setTurn(by, ts, "removed on reply", false) {
		w.did(by, "g", false)
		w.mayWithdraw(by, by)
	}
	if by != w.owner && w.e.g.owner.AccountID != 0 {
		w.e.g.setAttention(w.e.g.owner, ts, "Someone else replied", true)
	}
}

func (w *wf) message(by, ts, tag, msg string, real *gerrit.AccountInfo) {
	w.e.g.nextID++
	w.e.g.messages = append(w.e.g.messages, gerrit.ChangeMessage{ID: fmt.Sprintf("wf%d", w.e.g.nextID), Author: wfPeople[by].g, RealAuthor: real,
		Date: ts, Tag: tag, Message: msg, RevisionNumber: w.ps})
}

// handOff is by replying in Gerrit, handing ns the turn.
func (w *wf) handOff(by string, ns ...string) {
	w.inGerrit()
	ts := w.e.g.tick()
	w.message(by, ts, "", fmt.Sprintf("Patch Set %d:\n\nPTAL", w.ps), nil)
	for _, n := range ns {
		if w.setTurn(n, ts, "Someone replied", true) {
			w.did(n, "g", true)
			w.mayAsk(by, n)
		}
	}
	w.replied(by, ts)
}

// takeTurn is by replying in Gerrit, taking ns out of the attention set.
func (w *wf) takeTurn(by string, ns ...string) {
	w.inGerrit()
	ts := w.e.g.tick()
	w.message(by, ts, "", fmt.Sprintf("Patch Set %d:\n\nnot yet", w.ps), nil)
	for _, n := range ns {
		if w.setTurn(n, ts, "Someone replied", false) {
			w.did(n, "g", false)
			w.mayWithdraw(by, n)
		}
	}
	w.replied(by, ts)
}

// replyG is n replying in Gerrit (a comment): their turn ends.
func (w *wf) replyG(n string) {
	w.inGerrit()
	ts := w.e.g.tick()
	w.message(n, ts, "", fmt.Sprintf("Patch Set %d:\n\n(1 comment)", w.ps), nil)
	w.replied(n, ts)
}

func (w *wf) addReviewerAt(ts, by, n string) {
	w.e.g.addReviewerAt(ts, wfPeople[by].g, wfPeople[n].g)
	if !w.e.g.wip {
		w.did(n, "g", true)
		w.mayAsk(by, n)
	}
}

// addReviewer is by adding n as a reviewer in Gerrit.
func (w *wf) addReviewer(by, n string) {
	w.inGerrit()
	w.addReviewerAt(w.e.g.tick(), by, n)
}

// removeReviewer is by removing reviewer n in Gerrit.
func (w *wf) removeReviewer(by, n string) {
	w.inGerrit()
	was := w.turn(n)
	w.e.g.removeReviewer(wfPeople[by].g, wfPeople[n].g)
	w.did(n, "g", false)
	if was {
		w.mayWithdraw(by, n)
	}
}

// changeRequesters are those whose GitHub review that counts asks for
// changes.
func (w *wf) changeRequesters() []string {
	var out []string
	for _, r := range latestReviews(w.e.h.reviews) {
		if r.State == "CHANGES_REQUESTED" && r.User.Type != "Bot" {
			out = append(out, w.byLogin(r.User.Login))
		}
	}
	return out
}

// upload is by uploading patch set ps in Gerrit (exported to the PR),
// with %r= for rs: whoever last asked for changes may be asked again.
func (w *wf) upload(by string, ps int, rs ...string) {
	w.inGerrit()
	sha := fmt.Sprintf("sha%d", ps)
	ts := w.e.exported(w.t, wfPeople[by].g, ps, sha)
	w.e.h.head, w.ps = sha, ps
	for _, r := range rs {
		w.addReviewerAt(ts, by, r)
	}
	if wfTrusted[by] {
		for _, n := range w.changeRequesters() {
			if n != by {
				w.may("gh+"+n, "g+"+n)
				delete(w.last, n)
				w.uncarried[n] = false
			}
		}
	}
}

// uploadUnrecorded is by uploading patch set ps in Gerrit, with %r= for
// rs, that the bridge has yet to record exporting (record), or never
// will (withheld from GitHub): its turns wait for the record, and are
// nobody's once a later patch set is current.
func (w *wf) uploadUnrecorded(by string, ps int, rs ...string) {
	w.inGerrit()
	ts := w.e.g.upload(wfPeople[by].g, ps, fmt.Sprintf("sha%d", ps))
	w.ps = ps
	for _, r := range rs {
		w.e.g.addReviewerAt(ts, wfPeople[by].g, wfPeople[r].g)
		w.did(r, "g", true)
		w.uncarried[r] = true // until recorded
	}
	if w.pending == nil {
		w.pending = map[int]wfPending{}
	}
	w.pending[ps] = wfPending{by, rs}
}

// wfPending is a patch set uploaded in Gerrit but not yet recorded.
type wfPending struct {
	by string
	rs []string
}

// record is the bridge recording that it exported patch set ps (pushed
// to the PR), as the code sync does.
func (w *wf) record(ps int) {
	sha := fmt.Sprintf("sha%d", ps)
	if err := updateStateFn(context.Background(), w.e.st, 5, "export", func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: sha, GitHubSHA: sha, Exported: true})
	}); err != nil {
		w.t.Fatal(err)
	}
	w.e.h.head = sha
	p := w.pending[ps]
	delete(w.pending, ps)
	if ps != w.ps {
		return // superseded: its turns are nobody's
	}
	for _, r := range p.rs {
		w.uncarried[r] = false
		w.mayAsk(p.by, r)
	}
	if wfTrusted[p.by] {
		for _, n := range w.changeRequesters() {
			if n != p.by {
				w.may("gh+"+n, "g+"+n)
				delete(w.last, n)
				w.uncarried[n] = false
			}
		}
	}
}

// push is by pushing patch set ps to the PR, which the bridge imports.
func (w *wf) push(by string, ps int) {
	sha := fmt.Sprintf("sha%d", ps)
	w.e.imported(w.t, wfPeople[by].g, ps, sha)
	w.e.h.head, w.ps = sha, ps
}

// clear is Gerrit clearing the attention set at ts.
func (w *wf) clear(ts, reason string) {
	for _, a := range w.e.g.attention {
		w.e.g.setAttention(a.Account, ts, reason, false)
		delete(w.last, w.byID(a.Account.AccountID))
	}
}

// wip is by marking the change WIP: in Gerrit (real nil), or on GitHub
// as the PR's draft, mirrored by the bridge as the owner.
func (w *wf) wip(by string, real *gerrit.AccountInfo) {
	if real == nil {
		w.inGerrit()
	}
	ts := w.e.g.tick()
	w.message(by, ts, "autogenerated:gerrit:setWorkInProgress", "Set Work In Progress", real)
	w.clear(ts, "Change was marked work in progress")
	w.e.g.wip, w.e.h.draft = true, true
}

// ready is by marking the change ready, likewise: Gerrit gives every
// reviewer not in the attention set the turn.
func (w *wf) ready(by string, real *gerrit.AccountInfo) {
	if real == nil {
		w.inGerrit()
	}
	ts := w.e.g.tick()
	w.message(by, ts, "autogenerated:gerrit:setReadyForReview", "Set Ready For Review", real)
	for _, r := range w.e.g.reviewers["REVIEWER"] {
		n := w.byID(r.AccountID)
		if n == w.owner || !w.setTurn(n, ts, "Change was marked ready for review", true) {
			continue
		}
		if real == nil {
			w.did(n, "g", true)
			w.mayAsk(by, n)
		} else {
			// The bridge's doing for GitHub's ready: not carried (design).
			delete(w.last, n)
			w.uncarried[n] = !w.requested(n)
		}
	}
	w.e.g.wip, w.e.h.draft = false, false
}

// abandon is by abandoning the change, likewise (GitHub: closing the PR).
func (w *wf) abandon(by string, real *gerrit.AccountInfo) {
	if real == nil {
		w.inGerrit()
	}
	ts := w.e.g.tick()
	w.message(by, ts, "", "Abandoned", real)
	w.clear(ts, "Change was abandoned")
	w.e.g.status, w.e.h.closed = "ABANDONED", true
}

// restore is by restoring it, likewise (GitHub: reopening the PR): whoever
// is still asked on GitHub may be given back their turn.
func (w *wf) restore(by string, real *gerrit.AccountInfo) {
	if real == nil {
		w.inGerrit()
	}
	ts := w.e.g.tick()
	w.message(by, ts, "", "Restored", real)
	w.e.g.status, w.e.h.closed = "NEW", false
	for n := range wfPeople {
		if w.requested(n) {
			w.may("g+" + n)
		}
	}
}

func (w *wf) submit() {
	w.inGerrit()
	ts := w.e.g.tick()
	w.clear(ts, "Change was submitted")
	w.e.g.status, w.e.h.merged = "MERGED", true
}

// request is someone asking ns to review on GitHub.
func (w *wf) request(ns ...string) {
	for _, n := range ns {
		if w.requested(n) {
			continue
		}
		p := wfPeople[n]
		w.e.h.requested = append(w.e.h.requested, p.gh)
		w.did(n, "gh", true)
		if p.g.AccountID != 0 {
			w.may("g+" + n)
		} else if p.gh.Type != "Bot" {
			w.may("note:" + n)
		}
	}
}

// withdraw is someone withdrawing ns's requests on GitHub.
func (w *wf) withdraw(ns ...string) {
	for _, n := range ns {
		if !w.requested(n) {
			continue
		}
		id := wfPeople[n].gh.ID
		w.e.h.requested = slices.DeleteFunc(w.e.h.requested, func(u github.User) bool { return u.ID == id })
		w.did(n, "gh", false)
		w.may("g-" + n)
	}
}

// review is n reviewing the PR's head on GitHub (state APPROVED,
// CHANGES_REQUESTED or COMMENTED), which ends their request.
func (w *wf) review(n, state string) {
	p := wfPeople[n]
	// Asked or not, their answer is about their turn: theirs no longer.
	w.e.h.requested = slices.DeleteFunc(w.e.h.requested, func(u github.User) bool { return u.ID == p.gh.ID })
	w.did(n, "gh", false)
	w.e.h.reviews = append(w.e.h.reviews, github.Review{ID: w.e.h.id(), State: state, Body: strings.ToLower(state) + " by " + n,
		User: p.gh, CommitID: cmpOr(w.e.h.head, "sha2"), SubmittedAt: w.e.g.now()})
	w.may("g-" + n)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- failures ----

// withFailure makes the next sync fail, for now, in kinds' ways:
// "gh502" (GitHub refuses requests), "gh502:<login>" (for one login),
// "gerrit500" (Gerrit refuses attention, reviewer and note changes),
// "trust" (the trusted group cannot be read), "lookup" (identity lookups
// cannot answer).
func (w *wf) withFailure(kinds string) func() {
	var undo []func()
	for _, kind := range strings.Split(kinds, ",") {
		e := w.e
		switch {
		case kind == "":
		case kind == "gh502":
			e.h.refuseAsk = &github.APIError{Status: 502, Message: "Bad Gateway"}
			undo = append(undo, func() { e.h.refuseAsk = nil })
		case strings.HasPrefix(kind, "gh502:"):
			e.c.GitHub = &refuseFor{e.h, strings.TrimPrefix(kind, "gh502:")}
			undo = append(undo, func() { e.c.GitHub = e.h })
		case kind == "gerrit500":
			e.g.refuseAttention, e.g.refuseReviewers, e.g.refuseNotes = true, true, true
			undo = append(undo, func() { e.g.refuseAttention, e.g.refuseReviewers, e.g.refuseNotes = false, false, false })
		case kind == "trust":
			old := e.c.Trusted
			e.c.Trusted = func(context.Context, int) (bool, bool) { return false, false }
			undo = append(undo, func() { e.c.Trusted = old })
		case kind == "lookup":
			old := e.c.Users
			e.c.Users = failingLookups{old}
			undo = append(undo, func() { e.c.Users = old })
		default:
			w.t.Fatalf("unknown failure %q", kind)
		}
	}
	return func() {
		for i := len(undo) - 1; i >= 0; i-- { // in reverse: a kind given twice is undone
			undo[i]()
		}
	}
}

// ---- syncing and checking ----

func (w *wf) errorf(format string, args ...any) {
	w.errs = append(w.errs, fmt.Sprintf(format, args...))
}

// sync is one comment sync, with fail's failures, checking what must
// hold however batched; at names what it follows.
func (w *wf) sync(at, fail string) {
	w.t.Helper()
	w.syncs++
	undo := w.withFailure(fail)
	w.writes = nil
	asks := len(w.e.h.asks)
	w.e.sync(w.t)
	undo()
	for _, a := range w.e.h.asks[asks:] {
		w.writes = append(w.writes, "gh"+a[:1]+w.byLogin(a[1:]))
	}
	at = fmt.Sprintf("sync %d (%s)", w.syncs, at)
	seen := map[string]bool{}
	for _, wr := range w.writes {
		if strings.HasPrefix(wr, "gh") && w.gitHubOnly {
			w.errorf("%s: HARD RULE: wrote %s to GitHub, nobody having acted in Gerrit", at, wr)
		}
		if seen[wr] {
			w.errorf("%s: %s twice in one sync (%v)", at, wr, w.writes)
		}
		seen[wr] = true
		if w.allow[wr] == 0 {
			w.errorf("%s: %s, which nobody's doing since asked for (%v)", at, wr, w.writes)
		} else {
			w.allow[wr]--
		}
	}
	for n, l := range w.last {
		if len(w.touched[n]) > 1 || n == w.owner {
			continue // both sides: the bridge cannot tell which came last
		}
		side, in := strings.TrimRight(l, "+-"), strings.HasSuffix(l, "+")
		now := w.turn(n)
		if side == "gh" {
			now = w.requested(n)
		}
		if now != in && w.open() {
			w.errorf("%s: undid what a person did last about %s (%s): %s", at, n, l, w.view())
		}
	}
	// A clean sync carries everything it can: what it did not, it will
	// not (but what waits for a draft's ready, a closed PR's reopening,
	// or a failure).
	if fail == "" && w.open() && !w.e.h.draft {
		clear(w.allow)
		clear(w.touched)
	}
}

// settle syncs until quiet: the second sync writes nothing, and the
// sides agree but as the design allows.
func (w *wf) settle() {
	w.t.Helper()
	w.sync("settle", "")
	w.sync("settled", "")
	if len(w.writes) != 0 {
		w.errorf("a sync after a quiet one wrote %v", w.writes)
	}
	if !w.open() || w.e.h.draft {
		return
	}
	for _, n := range wfNames() {
		p := wfPeople[n]
		if n == w.owner || n == w.author || p.g.AccountID == 0 || p.gh.ID == 0 || w.uncarried[n] {
			continue
		}
		if w.requested(n) && !w.reviewer(n) {
			w.errorf("settled: %s asked on GitHub, not a Gerrit reviewer", n)
		}
		if w.reviewer(n) && w.turn(n) != w.requested(n) {
			w.errorf("settled: %s's turn in Gerrit %v, asked on GitHub %v", n, w.turn(n), w.requested(n))
		}
	}
}

// wfVariant is how a workflow is synced: after every every steps, with
// fail's failures for each sync (by its 1-based number).
type wfVariant struct {
	name  string
	every int
	fail  map[int]string
}

var wfVariants = []wfVariant{
	{"each", 1, nil},
	{"every2", 2, nil},
	{"every3", 3, nil},
	{"once", 1 << 10, nil},
	{"each-failing", 1, map[int]string{1: "gh502", 3: "gerrit500", 5: "trust", 7: "lookup", 9: "gh502", 11: "gerrit500", 13: "trust", 15: "lookup"}},
	{"each-failing2", 1, map[int]string{2: "trust", 4: "gh502", 6: "gerrit500", 8: "lookup", 10: "trust", 12: "gh502", 14: "gerrit500"}},
	{"every2-failing", 2, map[int]string{1: "trust", 2: "gh502", 3: "gerrit500", 4: "lookup", 5: "trust"}},
}

// wfKnown is a variant a known bug fails, by its repro's name
// (reviewers_workflow_bugs_test.go): skipped until it is fixed.
type wfKnown map[string]string

// runWF runs steps on owner's change in every variant (in parallel), but
// those known to fail.
func runWF(t *testing.T, owner string, known wfKnown, steps func() []wfStep) {
	t.Parallel()
	for _, v := range wfVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			if bug := known[v.name]; bug != "" {
				t.Skipf("known bug: %s", bug)
			}
			w := newWF(t, owner)
			ss := steps()
			strong := v.name == "each"
			var fail []string
			var log []string
			for i, s := range ss {
				s.do(w)
				if s.fail != "" {
					fail = append(fail, s.fail)
				}
				if (i+1)%v.every != 0 && i != len(ss)-1 {
					continue
				}
				if f := v.fail[w.syncs+1]; f != "" {
					fail = append(fail, f)
				}
				w.sync(fmt.Sprintf("after step %d %q", i+1, s.name), strings.Join(fail, ","))
				log = append(log, fmt.Sprintf("%2d %-55s %s %v %s", i+1, s.name, w.view(), w.writes, strings.Join(fail, ",")))
				fail = nil
				if strong && s.want != "" && w.view() != s.want {
					w.errorf("step %d %q: %s, want %s (%v)", i+1, s.name, w.view(), s.want, w.writes)
				}
				if strong && s.check != nil {
					for _, b := range s.check(w) {
						w.errorf("step %d %q: %s", i+1, s.name, b)
					}
				}
			}
			w.settle()
			if len(w.errs) > 0 {
				t.Errorf("%s:\n  %s\nlog:\n  %s", v.name, strings.Join(w.errs, "\n  "), strings.Join(log, "\n  "))
			}
		})
	}
}
