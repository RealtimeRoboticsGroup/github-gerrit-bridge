package reconcile

// Bugs the reviewer fuzzer (reviewers_fuzz_test.go) found, each cut down
// to a workflow. A new one found is added here skipped (and to
// fzKnownBugs) until fixed.

import "testing"

// The bridge asks jane on GitHub for austin's adding her, in a sync whose
// identity lookups fail for now: the request it made is not recorded as
// seen (the GitHub → Gerrit pass could not tell whose it is, so left it
// to look new), so her review ending it never takes her turn in Gerrit.
func TestReviewerBugOwnAskLostToALookupFailureReview(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane (lookups fail)", do: func(w *wf) { w.addReviewer("austin", "jane") }, fail: "lookup", want: "G[jane] H[jane]"},
			{name: "jane asks for changes", do: func(w *wf) { w.review("jane", "CHANGES_REQUESTED") }, want: "G[] H[]"},
		}
	})
}

// Likewise, and austin then takes jane's turn while GitHub refuses the
// withdrawal: the bridge's own request, looking new, gives jane back the
// turn austin took (undoing a trusted person's last doing).
func TestReviewerBugOwnAskLostToALookupFailureTurnGivenBack(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane (lookups fail)", do: func(w *wf) { w.addReviewer("austin", "jane") }, fail: "lookup", want: "G[jane] H[jane]"},
			{name: "austin takes jane's turn (GitHub down)", do: func(w *wf) { w.takeTurn("austin", "jane") }, fail: "gh502", want: "G[] H[jane]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[] H[]"},
		}
	})
}

// mike is asked on GitHub; owen closes the PR (Gerrit's abandon takes
// mike's turn), then reopens it and removes mike as a reviewer in Gerrit
// before the next sync: giving back the turn the abandon took, the bridge
// makes mike a reviewer again, undoing owen's removal.
func TestReviewerBugGiveBackUndoesARemoval(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[mike]"},
			{name: "owen reopens it and removes mike in Gerrit", do: func(w *wf) {
				w.restore("owen", &bridgeGA)
				w.removeReviewer("owen", "mike")
			}, want: "G[] H[mike]", check: func(w *wf) []string {
				if w.reviewer("mike") {
					return []string{"mike made a reviewer again"}
				}
				return nil
			}},
		}
	})
}

// mike asks for changes on GitHub (his turn over); austin uploads PS3,
// and owen then removes mike as a reviewer in Gerrit, before a sync: the
// ask again for PS3 skips whoever was taken out of the attention set
// after it, but mike, out already, was not, so he is asked on GitHub and
// made a reviewer in Gerrit again, undoing owen's removal.
func TestReviewerBugReAskUndoesARemoval(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS3; owen removes mike", do: func(w *wf) {
				w.upload("austin", 3)
				w.removeReviewer("owen", "mike")
			}, want: "G[] H[]", check: func(w *wf) []string {
				if w.reviewer("mike") {
					return []string{"mike made a reviewer again"}
				}
				return nil
			}},
		}
	})
}

// owen adds mike, who is asked on GitHub; austin marks the change WIP,
// hands mike the turn (not carried while a draft), then removes him as a
// reviewer: a withdrawal, which the design carries while a draft, but the
// bridge never took that turn as seen, so never sees it end. mike stays
// asked on GitHub for good, no longer a reviewer in Gerrit.
func TestReviewerBugRemovedWhileADraftAfterAHandOff(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "owen adds mike", do: func(w *wf) { w.addReviewer("owen", "mike") }, want: "G[mike] H[mike]"},
			{name: "austin marks it WIP", do: func(w *wf) { w.wip("austin", nil) }, want: "G[] H[mike]"},
			{name: "austin hands mike the turn", do: func(w *wf) { w.handOff("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "austin removes mike", do: func(w *wf) { w.removeReviewer("austin", "mike") }, want: "G[] H[]"},
			{name: "austin marks it ready", do: func(w *wf) { w.ready("austin", nil) }, want: "G[] H[]"},
		}
	})
}

// The same turn, handed while a draft, ended by jane's own reply once
// owen marks the PR ready, before a sync sees it ready: her request
// stays on GitHub, her turn in Gerrit gone.
func TestReviewerBugRepliedAfterReadyAfterADraftHandOff(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[jane] H[jane]"},
			{name: "owen makes it a draft", do: func(w *wf) { w.wip("owen", &bridgeGA) }, want: "G[] H[jane]"},
			{name: "austin hands jane the turn", do: func(w *wf) { w.handOff("austin", "jane") }, want: "G[jane] H[jane]"},
			{name: "owen marks it ready; jane replies in Gerrit", do: func(w *wf) {
				w.ready("owen", &bridgeGA)
				w.replyG("jane")
			}, want: "G[] H[]"},
		}
	})
}

// austin removes owen as a reviewer in Gerrit; the bridge withdraws his
// GitHub request in a sync whose lookups fail, so cannot settle its end
// in Gerrit; owen is asked again before the next sync. That request is
// new, and gives him the turn, not taken for the one that ended.
func TestReviewerBugAskedAgainWhileAnEndWaits(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "owen is asked", do: func(w *wf) { w.request("owen") }, want: "G[owen] H[owen]"},
			{name: "austin removes owen (lookups fail)", do: func(w *wf) { w.removeReviewer("austin", "owen") }, fail: "lookup", want: "G[] H[]"},
			{name: "owen is asked again", do: func(w *wf) { w.request("owen") }, want: "G[owen] H[owen]"},
		}
	})
}

// jane is asked on GitHub while the change is WIP (a draft), then handed
// the turn in Gerrit; her review ends the request before it is ready. A
// request made while a draft is not recorded, so neither is that turn:
// once ready it is carried, and her review since answers it.
func TestReviewerBugHandOffWhileADraftToSomeoneAskedWhileADraft(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin marks it WIP", do: func(w *wf) { w.wip("austin", nil) }, want: "G[] H[]"},
			{name: "jane is asked", do: func(w *wf) { w.request("jane") }, want: "G[] H[jane]"},
			{name: "austin adds jane", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[] H[jane]"},
			{name: "owen hands jane the turn", do: func(w *wf) { w.handOff("owen", "jane") }, want: "G[jane] H[jane]"},
			{name: "jane asks for changes", do: func(w *wf) { w.review("jane", "CHANGES_REQUESTED") }, want: "G[jane] H[]"},
			{name: "owen marks it ready", do: func(w *wf) { w.ready("owen", nil) }, want: "G[] H[]"},
		}
	})
}
