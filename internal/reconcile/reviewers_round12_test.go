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
