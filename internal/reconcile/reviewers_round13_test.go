package reconcile

import (
	"errors"
	"testing"
)

// owen withdraws jane while lookups fail (the end kept, Ending). jane
// replies, and austin hands her the turn again, after the withdrawal,
// while the trusted group cannot be read (and lookups still fail). Once
// all is well, austin's hand-off, made after the end, must be carried
// (asked), not taken by the old request's end.
func TestR13EndingTakesALaterUnseenTurn(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[jane] H[jane]"},
			{name: "jane replies", do: func(w *wf) { w.replyG("jane") }, want: "G[] H[]"},
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
			{name: "owen withdraws jane (lookups fail)", do: func(w *wf) { w.withdraw("jane") }, fail: "lookup", want: "G[jane] H[]"},
			{name: "jane replies; austin hands her the turn (lookups, trust fail)", do: func(w *wf) {
				w.replyG("jane")
				w.handOff("austin", "jane")
			}, fail: "lookup,trust", want: "G[jane] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane] H[jane]"},
		}
	})
}

// The same with the directory down as in production: GitHubFor fails
// with the lookups, so no trust failure is needed.
func TestR13EndingTakesALaterTurnDirectoryDown(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		down := func(w *wf, on bool) {
			var err error
			if on {
				err = &IdentityUnknown{errors.New("directory down")}
			}
			w.e.c.Reviewers.(*fakeGitHubAccounts).unknown = err
		}
		return []wfStep{
			{name: "austin adds jane", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[jane] H[jane]"},
			{name: "jane replies", do: func(w *wf) { w.replyG("jane") }, want: "G[] H[]"},
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
			{name: "owen withdraws jane (directory down)", do: func(w *wf) { down(w, true); w.withdraw("jane") }, fail: "lookup", want: "G[jane] H[]"},
			{name: "jane replies; austin hands her the turn (directory down)", do: func(w *wf) {
				w.replyG("jane")
				w.handOff("austin", "jane")
			}, fail: "lookup", want: "G[jane] H[]"},
			{name: "directory back", do: func(w *wf) { down(w, false) }, want: "G[jane] H[jane]"},
		}
	})
}

// Fuzz seed 300245: austin's request, its Gerrit write failing, ends
// while lookups fail; asked again, the new request must still be carried.
func TestR13FuzzF300245(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, fail: "gh502", want: "G[] H[]"},
			{name: "austin comments", do: func(w *wf) { w.review("austin", "COMMENTED") }, want: "G[] H[]"},
			{name: "austin asked (gerrit fails)", do: func(w *wf) { w.request("austin") }, fail: "gerrit500", want: "G[] H[austin]"},
			{name: "austin comments (lookups fail)", do: func(w *wf) { w.review("austin", "COMMENTED") }, fail: "lookup", want: "G[] H[]"},
			{name: "austin asked again (trust fails)", do: func(w *wf) { w.request("austin") }, fail: "trust", want: "G[austin] H[austin]"},
			{name: "jane approves", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[austin] H[austin]"},
		}
	})
}
