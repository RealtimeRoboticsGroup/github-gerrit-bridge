package reconcile

// Random review lifecycles for the reviewer sync, on the workflow harness
// (reviewers_workflow_test.go): seeded, so each seed is the same run every
// time. Each seed draws 10–30 plausible steps (GitHub requests,
// withdrawals and reviews; Gerrit hand-offs, turns taken, replies,
// reviewers added and removed, by trusted people and not; uploads, %r=
// included, recorded at once, later or never; imports of GitHub pushes;
// WIP and ready, abandon and restore, from either side; failures), runs
// them in every wfVariant and checks what must hold however batched (the
// harness's sync and settle), and, synced after every step, that the
// sides agree after each sync that failed nothing but where the design
// says they may not. A quarter of the seeds are worked on GitHub alone
// (the hard rule: the bridge writes nothing to GitHub then, comments
// included).
//
// A failing seed is cut down to the fewest steps that fail the same way,
// and printed as steps to paste into a repro.
//
//	REVIEWERS_FUZZ_SEEDS=3000 go test -run TestReviewerFuzz ./internal/reconcile
//	bazel test //internal/reconcile:all --test_filter=TestReviewerFuzz --test_env=REVIEWERS_FUZZ_SEEDS=3000
//
// REVIEWERS_FUZZ_FIRST is the first seed (default 1); REVIEWERS_FUZZ_SEED
// runs one seed alone.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fzOp is one step, as data: replayed as it says when plausible then,
// skipped when not (the sides differ across variants, as the bridge's
// writes come at other times, and steps are dropped when cutting down).
type fzOp struct {
	kind  string
	by    string
	ns    []string
	state string // a review's
	fail  string // the following sync's failures
	// skipIf skips it when it holds (fzKnown, taking away what a known
	// bug needs): "draft", while a draft; "abandonedOut", its person's
	// last out of the attention set by an abandon; "outChangeRequester",
	// its person out of it, their review that counts a change request.
	skipIf string
}

func (o fzOp) String() string {
	s := o.kind + "(" + o.by
	if len(o.ns) > 0 {
		s += " " + strings.Join(o.ns, ",")
	}
	if o.state != "" {
		s += " " + o.state
	}
	s += ")"
	if o.fail != "" {
		s += " fail=" + o.fail
	}
	return s
}

// code is o as harness calls, to paste into a wfStep's do.
func (o fzOp) code() string {
	q := func(ss ...string) string {
		var out []string
		for _, s := range ss {
			out = append(out, strconv.Quote(s))
		}
		return strings.Join(out, ", ")
	}
	switch o.kind {
	case "request", "withdraw":
		return fmt.Sprintf("w.%s(%s)", o.kind, q(o.ns...))
	case "review":
		return fmt.Sprintf("w.review(%s)", q(o.by, o.state))
	case "handOff", "takeTurn":
		return fmt.Sprintf("w.%s(%s)", o.kind, q(append([]string{o.by}, o.ns...)...))
	case "replyG":
		return fmt.Sprintf("w.replyG(%s)", q(o.by))
	case "addReviewer", "removeReviewer":
		return fmt.Sprintf("w.%s(%s)", o.kind, q(o.by, o.ns[0]))
	case "upload", "uploadUnrecorded":
		rs := ""
		if len(o.ns) > 0 {
			rs = ", " + q(o.ns...)
		}
		return fmt.Sprintf("w.%s(%q, w.ps+1%s)", o.kind, o.by, rs)
	case "record":
		return "w.record(w.ps)"
	case "push":
		return fmt.Sprintf("w.push(%q, w.ps+1)", o.by)
	case "wipG", "readyG", "abandonG", "restoreG":
		return fmt.Sprintf("w.%s(%q, nil)", strings.TrimSuffix(o.kind, "G"), o.by)
	case "wipH", "readyH", "abandonH", "restoreH":
		return fmt.Sprintf("w.%s(%q, &bridgeGA)", strings.TrimSuffix(o.kind, "H"), o.by)
	case "submit":
		return "w.submit()"
	}
	return "// nothing"
}

// fzUploaders may upload patch sets in Gerrit; fzPushers push to the PR.
var (
	fzUploaders = []string{"austin", "owen", "jane", "stranger"}
	fzActors    = []string{"austin", "owen", "jane", "mike", "stranger", "nogh"}
)

func (w *wf) hasG(n string) bool  { return wfPeople[n].g.AccountID != 0 }
func (w *wf) hasGH(n string) bool { return wfPeople[n].gh.ID != 0 }
func (w *wf) merged() bool        { return w.e.h.merged || w.e.g.status == "MERGED" }
func (w *wf) abandoned() bool     { return w.e.h.closed && !w.merged() }

// plausible is o's people as they may be now (a hand-off's to reviewers
// whose turn it is not, say); false: o cannot be done now.
func (o fzOp) plausible(w *wf) (fzOp, bool) {
	keep := func(f func(n string) bool) bool {
		o.ns = slices.DeleteFunc(slices.Clone(o.ns), func(n string) bool { return !f(n) })
		return len(o.ns) > 0
	}
	open := w.open()
	switch o.skipIf {
	case "draft":
		if w.e.g.wip {
			return o, false
		}
	case "outChangeRequester":
		for _, n := range o.ns {
			if !w.turn(n) && slices.Contains(w.changeRequesters(), n) {
				return o, false
			}
		}
	case "abandonedOut":
		for _, n := range o.ns {
			if w.e.g.removed[wfPeople[n].g.AccountID].Reason == "Change was abandoned" {
				return o, false
			}
		}
	}
	switch o.kind {
	case "nothing":
		return o, true
	case "request":
		// GitHub: not the PR's author; anyone else, a draft's or a closed
		// PR's too.
		return o, !w.merged() && keep(func(n string) bool { return w.hasGH(n) && n != w.author && !w.requested(n) })
	case "withdraw":
		return o, !w.merged() && keep(w.requested)
	case "review":
		if !open || !w.hasGH(o.by) {
			return o, false
		}
		if o.by == "Copilot" {
			return o, w.requested(o.by) && o.state == "COMMENTED"
		}
		// The author may only comment on their own PR.
		return o, o.by != w.author || o.state == "COMMENTED"
	case "handOff":
		// Gerrit's reply hands the turn to reviewers (or CCs) only.
		return o, open && w.hasG(o.by) && keep(func(n string) bool { return n != o.by && n != w.owner && w.reviewer(n) && !w.turn(n) })
	case "takeTurn":
		return o, open && w.hasG(o.by) && keep(func(n string) bool { return n != o.by && n != w.owner && w.turn(n) })
	case "replyG":
		return o, open && (w.reviewer(o.by) || o.by == w.owner || o.by == "austin")
	case "addReviewer":
		return o, open && w.hasG(o.by) && keep(func(n string) bool { return w.hasG(n) && n != w.owner && !w.reviewer(n) })
	case "removeReviewer":
		// Gerrit: the owner, the reviewer, or someone allowed to (austin).
		return o, open && keep(w.reviewer) && (o.by == w.owner || o.by == o.ns[0] || o.by == "austin")
	case "upload", "uploadUnrecorded":
		// %r= for those not yet reviewers, not the owner or uploader.
		keep(func(n string) bool { return w.hasG(n) && n != w.owner && n != o.by && !w.reviewer(n) })
		return o, open
	case "record":
		_, p := w.pending[w.ps]
		return o, p && !w.merged()
	case "push":
		_, p := w.pending[w.ps]
		return o, open && !p
	case "wipG", "wipH":
		return o, open && !w.e.g.wip
	case "readyG", "readyH":
		return o, open && w.e.g.wip
	case "abandonG", "abandonH":
		return o, open
	case "restoreG", "restoreH":
		return o, w.abandoned()
	case "submit":
		return o, open && !w.e.g.wip
	}
	return o, false
}

func (o fzOp) do(w *wf) {
	switch o.kind {
	case "request":
		w.request(o.ns...)
	case "withdraw":
		w.withdraw(o.ns...)
	case "review":
		w.review(o.by, o.state)
	case "handOff":
		w.handOff(o.by, o.ns...)
	case "takeTurn":
		w.takeTurn(o.by, o.ns...)
	case "replyG":
		w.replyG(o.by)
	case "addReviewer":
		w.addReviewer(o.by, o.ns[0])
	case "removeReviewer":
		w.removeReviewer(o.by, o.ns[0])
	case "upload":
		w.upload(o.by, w.ps+1, o.ns...)
	case "uploadUnrecorded":
		w.uploadUnrecorded(o.by, w.ps+1, o.ns...)
	case "record":
		w.record(w.ps)
	case "push":
		w.push(o.by, w.ps+1)
	case "wipG":
		w.wip(o.by, nil)
	case "wipH":
		w.wip(o.by, &bridgeGA)
	case "readyG":
		w.ready(o.by, nil)
	case "readyH":
		w.ready(o.by, &bridgeGA)
	case "abandonG":
		w.abandon(o.by, nil)
	case "abandonH":
		w.abandon(o.by, &bridgeGA)
	case "restoreG":
		w.restore(o.by, nil)
	case "restoreH":
		w.restore(o.by, &bridgeGA)
	case "submit":
		w.submit()
	}
}

// fzGen draws plausible steps.
type fzGen struct {
	r          *rand.Rand
	gitHubOnly bool
}

func (g *fzGen) pick(ss []string) string { return ss[g.r.IntN(len(ss))] }

func (g *fzGen) some(ss []string) []string {
	ss = slices.Clone(ss)
	g.r.Shuffle(len(ss), func(i, j int) { ss[i], ss[j] = ss[j], ss[i] })
	k := 1
	for k < len(ss) && g.r.IntN(4) == 0 {
		k++
	}
	return slices.Sorted(slices.Values(ss[:k]))
}

var fzFailures = []string{"gh502", "gh502:mike-gh", "gh502:jane-gh", "gerrit500", "trust", "lookup"}

// next is a plausible step for w now.
func (g *fzGen) next(w *wf, last bool) fzOp {
	type choice struct {
		weight int
		kind   string
	}
	cs := []choice{{14, "request"}, {6, "withdraw"}, {16, "review"}, {5, "push"}, {2, "wipH"}, {3, "readyH"},
		{2, "abandonH"}, {4, "restoreH"}, {2, "nothing"}}
	if !g.gitHubOnly {
		cs = append(cs, []choice{{9, "handOff"}, {5, "takeTurn"}, {6, "replyG"}, {7, "addReviewer"}, {3, "removeReviewer"},
			{8, "upload"}, {2, "uploadUnrecorded"}, {6, "record"}, {2, "wipG"}, {3, "readyG"}, {1, "abandonG"}, {4, "restoreG"}}...)
		if last {
			cs = append(cs, choice{3, "submit"})
		}
	}
	total := 0
	for _, c := range cs {
		total += c.weight
	}
	everyone := wfNames()
	for {
		k := g.r.IntN(total)
		var kind string
		for _, c := range cs {
			if k < c.weight {
				kind = c.kind
				break
			}
			k -= c.weight
		}
		o := fzOp{kind: kind}
		switch kind {
		case "request", "withdraw", "handOff", "takeTurn":
			o.ns = g.some(everyone)
		case "addReviewer", "removeReviewer":
			o.ns = []string{g.pick(everyone)}
		case "upload", "uploadUnrecorded":
			if g.r.IntN(3) == 0 {
				o.ns = g.some(everyone)
			}
		}
		switch kind {
		case "review":
			o.by = g.pick(everyone)
			o.state = g.pick([]string{"APPROVED", "CHANGES_REQUESTED", "CHANGES_REQUESTED", "COMMENTED", "COMMENTED"})
		case "handOff", "takeTurn", "addReviewer", "removeReviewer", "replyG":
			// Mostly trusted people, who the bridge carries.
			if g.r.IntN(3) > 0 {
				o.by = g.pick([]string{"austin", "owen"})
			} else {
				o.by = g.pick(fzActors)
			}
			if kind == "replyG" && g.r.IntN(2) == 0 {
				o.by = g.pick(everyone)
			}
		case "upload", "uploadUnrecorded":
			o.by = g.pick(fzUploaders)
		case "push":
			o.by = g.pick([]string{w.author, w.author, "austin", "jane"})
		case "wipG", "readyG", "abandonG", "restoreG":
			o.by = g.pick([]string{w.owner, "austin"})
		case "wipH", "readyH", "abandonH", "restoreH":
			o.by = w.owner // the bridge mirrors GitHub's, as the owner
		}
		o, ok := o.plausible(w)
		if !ok {
			continue
		}
		if g.r.IntN(8) == 0 {
			o.fail = g.pick(fzFailures)
		}
		return o
	}
}

// fzResult is a run of ops in one variant: its failures, and its log.
type fzResult struct {
	errs, log []string
	ops       []fzOp // the steps run (drawn, or as given)
}

// fzRun runs ops (or, gen set, n drawn steps) on owner's change in v.
func fzRun(t *testing.T, owner string, v wfVariant, ops []fzOp, gen *fzGen, n int) fzResult {
	w := newWF(t, owner)
	var res fzResult
	strong := v.every == 1
	var fail []string
	gitHub := func() int { return w.e.h.posts + len(w.e.h.dismissed) + len(w.e.h.edits) + len(w.e.h.reviewEdits) }
	if gen == nil {
		n = len(ops)
	}
	for i := range n {
		var o fzOp
		if gen != nil {
			o = gen.next(w, i == n-1)
			res.ops = append(res.ops, o)
		} else {
			o = ops[i]
			res.ops = append(res.ops, o)
			var ok bool
			if o, ok = o.plausible(w); !ok {
				o = fzOp{kind: "nothing", fail: o.fail}
			}
		}
		o.do(w)
		if o.fail != "" {
			fail = append(fail, o.fail)
		}
		if (i+1)%v.every != 0 && i != n-1 {
			res.log = append(res.log, fmt.Sprintf("%2d %s", i+1, o))
			continue
		}
		if f := v.fail[w.syncs+1]; f != "" {
			fail = append(fail, f)
		}
		before, only := gitHub(), w.gitHubOnly
		w.sync(fmt.Sprintf("after step %d %s", i+1, o), strings.Join(fail, ","))
		if only && gitHub() != before {
			w.errorf("sync %d: HARD RULE: posted to GitHub, nobody having acted in Gerrit", w.syncs)
		}
		res.log = append(res.log, fmt.Sprintf("%2d %-60s %s %v %s", i+1, o.String(), w.view(), w.writes, strings.Join(fail, ",")))
		clean := len(fail) == 0
		fail = nil
		if strong && clean {
			for _, b := range w.disagree() {
				w.errorf("step %d %s: %s", i+1, o, b)
			}
		}
	}
	w.settle()
	res.errs = w.errs
	return res
}

// disagree is how the sides differ, but where the design says they may
// (as settle checks).
func (w *wf) disagree() []string {
	if !w.open() || w.e.h.draft {
		return nil
	}
	var out []string
	for _, n := range wfNames() {
		p := wfPeople[n]
		if n == w.owner || n == w.author || p.g.AccountID == 0 || p.gh.ID == 0 || w.uncarried[n] {
			continue
		}
		if w.requested(n) && !w.reviewer(n) {
			out = append(out, fmt.Sprintf("disagree: %s asked on GitHub, not a Gerrit reviewer: %s", n, w.view()))
		}
		if w.reviewer(n) && w.turn(n) != w.requested(n) {
			out = append(out, fmt.Sprintf("disagree: %s's turn in Gerrit %v, asked on GitHub %v: %s", n, w.turn(n), w.requested(n), w.view()))
		}
	}
	return out
}

// fzClass is what kind of failure err is, to cut down to the same one.
func fzClass(err string) string {
	for _, c := range []string{"HARD RULE", "twice in one sync", "which nobody's doing", "undid", "quiet one wrote", "settled:", "disagree"} {
		if strings.Contains(err, c) {
			return c
		}
	}
	return err
}

func fzFails(res fzResult, class string) bool {
	return slices.ContainsFunc(res.errs, func(e string) bool { return fzClass(e) == class })
}

// fzMinimize cuts ops down to the fewest that fail class in some variant
// (v, or one synced more often, which shows it more plainly): steps
// dropped, in runs then one by one, people dropped from steps, failures
// dropped. It returns the steps and the variant they fail in.
func fzMinimize(t *testing.T, owner string, v wfVariant, ops []fzOp, class string) ([]fzOp, wfVariant) {
	// A known bug cut down is that bug; another is kept from becoming one.
	known := fzKnown(t, owner, v, ops, class) != ""
	vs := []wfVariant{wfVariants[0]}
	if v.name != vs[0].name {
		vs = append(vs, v)
	}
	if v.every > 1 && v.fail == nil {
		vs = append(vs, fzVariant("every2"), fzVariant("every3"), fzVariant("once"))
	}
	fails := func(ops []fzOp) bool {
		for i, u := range vs {
			if fzFails(fzRun(t, owner, u, ops, nil, 0), class) && (known || fzKnown(t, owner, u, ops, class) == "") {
				// Try it first next time.
				vs[0], vs[i] = vs[i], vs[0]
				return true
			}
		}
		return false
	}
	for size := len(ops) / 2; size > 1; size /= 2 {
		for i := 0; i+size <= len(ops); {
			if try := slices.Delete(slices.Clone(ops), i, i+size); fails(try) {
				ops = try
			} else {
				i += size
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(ops); i++ {
			try := slices.Delete(slices.Clone(ops), i, i+1)
			if fails(try) {
				ops, changed = try, true
				i--
			}
		}
		for i := range ops {
			if ops[i].fail != "" {
				try := slices.Clone(ops)
				try[i].fail = ""
				if fails(try) {
					ops, changed = try, true
				}
			}
			for j := 0; len(ops[i].ns) > 1 && j < len(ops[i].ns); j++ {
				try := slices.Clone(ops)
				try[i].ns = slices.Delete(slices.Clone(ops[i].ns), j, j+1)
				if fails(try) {
					ops, changed = try, true
					j--
				}
			}
		}
	}
	for _, u := range vs {
		if fzFails(fzRun(t, owner, u, ops, nil, 0), class) {
			return ops, u
		}
	}
	return ops, v
}

// fzVariant is a wfVariant by name.
func fzVariant(name string) wfVariant {
	for _, v := range wfVariants {
		if v.name == name {
			return v
		}
	}
	panic(name)
}

func fzEnvInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			panic(fmt.Sprintf("%s: %v", name, err))
		}
		return n
	}
	return def
}

// fzKnownBugs are the known bugs (their repros, in
// reviewers_fuzz_bugs_test.go), each with what takes away what it needs:
// a failure kind (steps' and the variant's), or steps of a kind skipped
// when skipIf holds (fzOp.skipIf). Remove an entry once its bug is fixed.
var fzKnownBugs = []struct{ bug, fail, kind, skipIf string }{
	{bug: "TestReviewerBugRemovedWhileADraftAfterAHandOff, TestReviewerBugRepliedAfterReadyAfterADraftHandOff", kind: "handOff", skipIf: "draft"},
	{bug: "TestReviewerBugGiveBackUndoesARemoval", kind: "removeReviewer", skipIf: "abandonedOut"},
}

// fzKnown is the known bug a failure is, "" if none: one that goes away
// once what the bug needs is taken out.
func fzKnown(t *testing.T, owner string, v wfVariant, ops []fzOp, class string) string {
	for _, k := range fzKnownBugs {
		drop := func(fail string) string {
			return strings.Join(slices.DeleteFunc(strings.Split(fail, ","), func(f string) bool { return f == k.fail }), ",")
		}
		u := wfVariant{name: v.name, every: v.every, fail: map[int]string{}}
		for n, f := range v.fail {
			u.fail[n] = drop(f)
		}
		without := slices.Clone(ops)
		changed := false
		for i := range without {
			if f := drop(without[i].fail); f != without[i].fail {
				without[i].fail, changed = f, true
			}
			if k.kind != "" && without[i].kind == k.kind {
				without[i].skipIf, changed = k.skipIf, true
			}
		}
		for n, f := range v.fail {
			changed = changed || u.fail[n] != f
		}
		if changed && !fzFails(fzRun(t, owner, u, without, nil, 0), class) {
			return k.bug
		}
	}
	return ""
}

func TestReviewerFuzz(t *testing.T) {
	first, seeds := fzEnvInt("REVIEWERS_FUZZ_FIRST", 1), fzEnvInt("REVIEWERS_FUZZ_SEEDS", 8)
	if s := fzEnvInt("REVIEWERS_FUZZ_SEED", 0); s != 0 {
		first, seeds = s, 1
	}
	// Distinct failures, reported once each (cut down from the first seed
	// that shows it).
	var list []int
	for seed := first; seed < first+seeds; seed++ {
		list = append(list, seed)
	}
	if s := os.Getenv("REVIEWERS_FUZZ_LIST"); s != "" { // seeds, comma-separated
		list = nil
		for _, f := range strings.Split(s, ",") {
			n, err := strconv.Atoi(f)
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, n)
		}
	}
	var mu sync.Mutex
	reported := map[string]bool{}
	for _, seed := range list {
		t.Run(strconv.Itoa(seed), func(t *testing.T) {
			t.Parallel()
			r := rand.New(rand.NewPCG(uint64(seed), 0x5eed))
			g := &fzGen{r: r, gitHubOnly: r.IntN(4) == 0}
			owner := "owen"
			if !g.gitHubOnly && r.IntN(3) == 0 {
				owner = "austin"
			}
			n := 10 + r.IntN(21)
			strong := fzRun(t, owner, wfVariants[0], nil, g, n)
			ops := strong.ops
			results := map[string]fzResult{wfVariants[0].name: strong}
			var wg sync.WaitGroup
			var rmu sync.Mutex
			for _, v := range wfVariants[1:] {
				wg.Go(func() {
					res := fzRun(t, owner, v, ops, nil, 0)
					rmu.Lock()
					results[v.name] = res
					rmu.Unlock()
				})
			}
			wg.Wait()
			// The first variant failing, cut down; the others named.
			var failing []string
			for _, v := range wfVariants {
				if res := results[v.name]; len(res.errs) > 0 {
					failing = append(failing, v.name)
				}
			}
			for _, name := range failing {
				v, res := fzVariant(name), results[name]
				class := fzClass(res.errs[0])
				key := fmt.Sprintf("%s/%s/%v", class, owner, g.gitHubOnly)
				if bug := fzKnown(t, owner, v, ops, class); bug != "" && os.Getenv("REVIEWERS_FUZZ_BUGS") == "" {
					t.Logf("known bug %s: %s", bug, res.errs[0])
					continue
				}
				mu.Lock()
				dup := reported[key]
				reported[key] = true
				mu.Unlock()
				if dup && os.Getenv("REVIEWERS_FUZZ_ALL") == "" {
					t.Errorf("seed %d, variants %v: %s (as reported)", seed, failing, res.errs[0])
					break
				}
				cut, cv := fzMinimize(t, owner, v, ops, class)
				mres := fzRun(t, owner, cv, cut, nil, 0)
				var code []string
				for _, o := range cut {
					c := o.code()
					if o.fail != "" {
						c += fmt.Sprintf(" // fail: %q", o.fail)
					}
					code = append(code, c)
				}
				t.Errorf("seed %d, %s's change (GitHub alone %v), variant %s (of %v): %s\ncut down to %d steps, in %s:\n  %s\nerrors:\n  %s\nlog:\n  %s",
					seed, owner, g.gitHubOnly, v.name, failing, class, len(cut), cv.name, strings.Join(code, "\n  "),
					strings.Join(mres.errs, "\n  "), strings.Join(mres.log, "\n  "))
				break
			}
		})
	}
}
