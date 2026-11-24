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
