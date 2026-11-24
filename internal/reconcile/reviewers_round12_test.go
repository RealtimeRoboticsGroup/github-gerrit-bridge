package reconcile

// Bugs the twelfth review found in the reviewer sync, each a workflow.

import "testing"

// new (no Gerrit account) is asked: noted. The request is withdrawn in a
// sync whose lookups fail (its end kept, Ending); new is asked again: a
// new request, which the design says is noted again.
func TestR12NoteAfterEndingRequest(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks new", do: func(w *wf) { w.request("new") }, want: "G[] H[new]", check: wfNotes("new", 1)},
			{name: "owen withdraws new (lookups fail)", do: func(w *wf) { w.withdraw("new") }, fail: "lookup", want: "G[] H[]"},
			{name: "owen asks new again", do: func(w *wf) { w.request("new") }, want: "G[] H[new]", check: wfNotes("new", 2)},
		}
	})
}

// austin takes jane's turn while the trusted group cannot be read (the
// withdrawal waits); owen withdraws her request on GitHub (still unread),
// then asks her again: a new request, which the waiting withdrawal must
// not withdraw.
func TestR12WaitingWithdrawalTakesANewRequest(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
			{name: "austin takes jane's turn (trust unknown)", do: func(w *wf) { w.takeTurn("austin", "jane") }, fail: "trust", want: "G[] H[jane]"},
			{name: "owen withdraws jane (trust unknown)", do: func(w *wf) { w.withdraw("jane") }, fail: "trust", want: "G[] H[]"},
			{name: "owen asks jane again", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
		}
	})
}

// jane, a reviewer out of the attention set, is asked on GitHub by owen
// while austin hands her the turn in Gerrit; the sync's lookups fail, so
// the request is left to look new though her turn was seen (asked
// already). owen then withdraws her: its end never takes her turn.
func TestR12SeenTurnRequestLostToALookupFailure(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[jane] H[jane]"},
			{name: "jane approves", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[] H[]"},
			{name: "owen asks jane; austin hands her the turn (lookups fail)", do: func(w *wf) {
				w.request("jane")
				w.handOff("austin", "jane")
			}, fail: "lookup", want: "G[jane] H[jane]"},
			{name: "owen withdraws jane", do: func(w *wf) { w.withdraw("jane") }, want: "G[] H[]"},
		}
	})
}

// austin uploads PS3, not yet on GitHub when mike asks for changes on
// PS2's commit (the bridge then takes his turn); once PS3 is recorded,
// mike is not asked for it: the bridge's own removal, after PS3's upload,
// is taken for someone taking him out.
func TestR12BridgesOwnRemovalSkipsReAsk(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "austin uploads PS3 (unrecorded); mike asks for changes on PS2", do: func(w *wf) {
				w.uploadUnrecorded("austin", 3)
				w.review("mike", "CHANGES_REQUESTED")
			}, want: "G[] H[]"},
			{name: "PS3 recorded", do: func(w *wf) { w.record(3) }, want: "G[mike] H[mike]"},
		}
	})
}

// austin uploads PS3 in Gerrit; owen pushes to the PR, its import not yet
// recorded when mike, asked on GitHub, requests changes on that push. The
// ask again for PS3 must not ask mike: he reviewed a later commit (design:
// "reviewed it or a later patch set's commit on GitHub").
func TestR12ReAskForAReviewOfAPushNotYetImported(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin uploads PS3", do: func(w *wf) { w.upload("austin", 3) }, want: "G[] H[]"},
			{name: "owen asks mike", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "owen pushes sha4 (import pending); mike asks for changes on it", do: func(w *wf) {
				w.e.h.head = "sha4"
				w.review("mike", "CHANGES_REQUESTED")
			}, want: "G[] H[]"},
		}
	})
}

// mike, a reviewer out of the attention set, is asked on GitHub in a sync
// whose Gerrit writes fail (the request left to look new); austin then
// removes him as a reviewer. Retrying the request must not make him a
// reviewer again (as TestReviewerBugFailedGiveBackUndoesARemoval).
func TestR12FailedRequestRetryUndoesARemoval(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen asks mike (Gerrit down)", do: func(w *wf) { w.request("mike") }, fail: "gerrit500", want: "G[] H[mike]"},
			{name: "austin removes mike", do: func(w *wf) { w.removeReviewer("austin", "mike") }, want: "G[] H[mike]", check: func(w *wf) []string {
				if w.reviewer("mike") {
					return []string{"mike made a reviewer again"}
				}
				return nil
			}},
		}
	})
}

// The same, the request made while a draft (left to look new once ready).
func TestR12DraftRequestUndoesARemoval(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "mike approves", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[] H[]"},
			{name: "owen makes it a draft", do: func(w *wf) { w.wip("owen", &bridgeGA) }, want: "G[] H[]"},
			{name: "owen asks mike", do: func(w *wf) { w.request("mike") }, want: "G[] H[mike]"},
			{name: "austin removes mike", do: func(w *wf) { w.removeReviewer("austin", "mike") }, want: "G[] H[mike]"},
			{name: "owen marks it ready", do: func(w *wf) { w.ready("owen", &bridgeGA) }, want: "G[] H[mike]", check: func(w *wf) []string {
				if w.reviewer("mike") {
					return []string{"mike made a reviewer again"}
				}
				return nil
			}},
		}
	})
}
