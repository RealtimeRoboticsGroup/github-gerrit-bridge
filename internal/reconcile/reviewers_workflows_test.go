package reconcile

import (
	"fmt"
	"testing"
)

// Review lifecycles for the reviewer sync (reviewers_workflow_test.go).
// A step's want is the sides after it, synced after every step: "G[...]"
// whose turn it is in Gerrit (but the owner's), "H[...]" who is asked on
// GitHub.

func wfNotes(n string, want int) func(*wf) []string {
	return func(w *wf) []string {
		if got := w.notes(n); got != want {
			return []string{fmt.Sprintf("%d notes that %s has no Gerrit account, want %d", got, n, want)}
		}
		return nil
	}
}

// austin's change, born in Gerrit: %r=mike, a change request, his fix
// and hand-off, jane asked on GitHub, WIP and ready, submitted.
func TestReviewerWorkflowGerritOwner(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "austin uploads PS3 %r=mike", do: func(w *wf) { w.upload("austin", 3, "mike") }, want: "G[mike] H[mike]"},
			{name: "mike requests changes on GitHub", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin replies in Gerrit, hands mike the turn", do: func(w *wf) { w.handOff("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[mike] H[mike]"},
			{name: "mike asks jane on GitHub", do: func(w *wf) { w.request("jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "mike approves on GitHub", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[jane] H[jane]"},
			{name: "austin marks it WIP", do: func(w *wf) { w.wip("austin", nil) }, want: "G[] H[jane]"},
			// Gerrit's ready gives every reviewer the turn, austin's
			// doing: mike, who approved, is asked again (design: "again,
			// if they have").
			{name: "austin marks it ready", do: func(w *wf) { w.ready("austin", nil) }, want: "G[jane mike] H[jane mike]"},
			{name: "jane approves on GitHub", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[mike] H[mike]"},
			// Gerrit clears the set; GitHub's requests stand (a limit).
			{name: "austin submits", do: func(w *wf) { w.submit() }, want: "G[] H[mike]"},
		}
	})
}

// owen's PR, worked on GitHub alone (the hard rule), until austin joins
// in Gerrit: his hand-off and upload ask mike.
func TestReviewerWorkflowGitHubPRJoinedInGerrit(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike on GitHub", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "mike requests changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen pushes PS3", do: func(w *wf) { w.push("owen", 3) }, want: "G[] H[]"},
			{name: "owen asks mike again", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "mike comments", do: func(w *wf) { w.review("mike", "COMMENTED") }, want: "G[] H[]"},
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
			{name: "jane approves", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[] H[]"},
			{name: "owen pushes PS4", do: func(w *wf) { w.push("owen", 4) }, want: "G[] H[]"},
			{name: "austin, in Gerrit, hands mike the turn", do: func(w *wf) { w.handOff("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "owen pushes PS5", do: func(w *wf) { w.push("owen", 5) }, want: "G[mike] H[mike]"},
			{name: "mike requests changes again", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS6 in Gerrit", do: func(w *wf) { w.upload("austin", 6) }, want: "G[mike] H[mike]"},
			{name: "mike approves", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[] H[]"},
		}
	})
}

// owen's PR worked on GitHub alone throughout, drafts, closing and
// reopening included: the bridge never writes to GitHub (the hard rule).
func TestReviewerWorkflowGitHubOnly(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike on GitHub", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "mike requests changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen pushes PS3", do: func(w *wf) { w.push("owen", 3) }, want: "G[] H[]"},
			{name: "owen asks mike again, and new", do: func(w *wf) { w.request("mike", "new") }, want: "G[mike] H[mike new]"},
			{name: "mike comments", do: func(w *wf) { w.review("mike", "COMMENTED") }, want: "G[] H[new]"},
			{name: "owen asks jane, Copilot", do: func(w *wf) { w.request("jane", "Copilot") }, want: "G[jane] H[Copilot jane new]"},
			{name: "jane approves, Copilot comments", do: func(w *wf) { w.review("jane", "APPROVED"); w.review("Copilot", "COMMENTED") }, want: "G[] H[new]"},
			{name: "owen makes it a draft", do: func(w *wf) { w.wip("owen", &bridgeGA) }, want: "G[] H[new]"},
			{name: "owen asks jane while a draft", do: func(w *wf) { w.request("jane") }, want: "G[] H[jane new]"},
			// Gerrit gives every reviewer the turn on the bridge's ready:
			// mike's is one nobody asks for on GitHub (design).
			{name: "owen marks it ready", do: func(w *wf) { w.ready("owen", &bridgeGA) }, want: "G[jane mike] H[jane new]"},
			{name: "owen pushes PS4", do: func(w *wf) { w.push("owen", 4) }, want: "G[jane mike] H[jane new]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[jane new]"},
			{name: "owen reopens it", do: func(w *wf) { w.restore("owen", &bridgeGA) }, want: "G[jane] H[jane new]"},
			{name: "owen withdraws new and jane", do: func(w *wf) { w.withdraw("new", "jane") }, want: "G[] H[]"},
		}
	})
}

// Many reviewers on owen's PR: newcomers with no Gerrit account, a bot,
// withdrawals and requests again, a draft, closing and reopening.
func TestReviewerWorkflowManyReviewers(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike, new, Copilot", do: func(w *wf) { w.request("mike", "new", "Copilot") }, want: "G[mike] H[Copilot mike new]", check: wfNotes("new", 1)},
			{name: "Copilot reviews", do: func(w *wf) { w.review("Copilot", "COMMENTED") }, want: "G[mike] H[mike new]"},
			{name: "austin adds jane in Gerrit", do: func(w *wf) { w.addReviewer("austin", "jane") }, want: "G[jane mike] H[jane mike new]"},
			{name: "owen withdraws new", do: func(w *wf) { w.withdraw("new") }, want: "G[jane mike] H[jane mike]"},
			{name: "owen asks new again", do: func(w *wf) { w.request("new") }, want: "G[jane mike] H[jane mike new]", check: wfNotes("new", 2)},
			{name: "austin removes jane in Gerrit", do: func(w *wf) { w.removeReviewer("austin", "jane") }, want: "G[mike] H[mike new]"},
			{name: "owen asks jane again", do: func(w *wf) { w.request("jane") }, want: "G[jane mike] H[jane mike new]"},
			{name: "austin takes mike's turn in Gerrit", do: func(w *wf) { w.takeTurn("austin", "mike") }, want: "G[jane] H[jane new]"},
			{name: "owen asks mike again", do: func(w *wf) { w.request("mike") }, want: "G[jane mike] H[jane mike new]"},
			{name: "owen makes it a draft", do: func(w *wf) { w.wip("owen", &bridgeGA) }, want: "G[] H[jane mike new]"},
			{name: "jane comments while a draft", do: func(w *wf) { w.review("jane", "COMMENTED") }, want: "G[] H[mike new]"},
			// jane's turn, from Gerrit for the bridge's ready, nobody asks
			// for on GitHub (design).
			{name: "owen marks it ready", do: func(w *wf) { w.ready("owen", &bridgeGA) }, want: "G[jane mike] H[mike new]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[mike new]"},
			{name: "owen reopens it", do: func(w *wf) { w.restore("owen", &bridgeGA) }, want: "G[mike] H[mike new]", check: wfNotes("new", 2)},
			{name: "mike approves", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[] H[new]"},
		}
	})
}

// Patch sets uploaded in Gerrit with GitHub pushes imported between:
// each of austin's asks mike, who last asked for changes, once; an import
// never does; a withdrawal stands for that patch set only.
func TestReviewerWorkflowPatchSetsAndImports(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "mike requests changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen pushes PS3", do: func(w *wf) { w.push("owen", 3) }, want: "G[] H[]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[mike] H[mike]"},
			{name: "owen pushes PS5", do: func(w *wf) { w.push("owen", 5) }, want: "G[mike] H[mike]"},
			{name: "mike requests changes on PS5", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen pushes PS6", do: func(w *wf) { w.push("owen", 6) }, want: "G[] H[]"},
			{name: "austin uploads PS7", do: func(w *wf) { w.upload("austin", 7) }, want: "G[mike] H[mike]"},
			{name: "owen withdraws mike", do: func(w *wf) { w.withdraw("mike") }, want: "G[] H[]"},
			{name: "owen pushes PS8", do: func(w *wf) { w.push("owen", 8) }, want: "G[] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[] H[]"},
			{name: "austin uploads PS9", do: func(w *wf) { w.upload("austin", 9) }, want: "G[mike] H[mike]"},
			{name: "mike approves", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[] H[]"},
			{name: "austin uploads PS10", do: func(w *wf) { w.upload("austin", 10) }, want: "G[] H[]"},
		}
	})
}

// Asks again for new patch sets that fail, for one person or for the
// trusted group, and a withdrawal after.
func TestReviewerWorkflowReAsksWithFailures(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike and jane", do: func(w *wf) { w.addReviewer("austin", "mike"); w.addReviewer("austin", "jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "both request changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED"); w.review("jane", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS3 (asking jane fails)", do: func(w *wf) { w.upload("austin", 3) }, fail: "gh502:jane-gh", want: "G[mike] H[mike]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane mike] H[jane mike]"},
			{name: "owen withdraws jane", do: func(w *wf) { w.withdraw("jane") }, want: "G[mike] H[mike]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[mike] H[mike]"},
			{name: "mike comments on PS3", do: func(w *wf) { w.review("mike", "COMMENTED") }, want: "G[] H[]"},
			{name: "austin uploads PS4 (trust unreadable)", do: func(w *wf) { w.upload("austin", 4) }, fail: "trust", want: "G[] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane mike] H[jane mike]"},
			{name: "jane approves", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[mike] H[mike]"},
			{name: "mike requests changes on PS4", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS5 (GitHub down)", do: func(w *wf) { w.upload("austin", 5) }, fail: "gh502", want: "G[] H[]"},
			{name: "austin uploads PS6 (Gerrit refuses)", do: func(w *wf) { w.upload("austin", 6) }, fail: "gerrit500", want: "G[] H[mike]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[mike] H[mike]"},
		}
	})
}

// Withdrawals after asks, and turns taken: by austin, by the reviewers
// themselves (carried, trusted or not), by someone untrusted (not).
func TestReviewerWorkflowWithdrawals(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "austin uploads PS3 %r=mike,jane", do: func(w *wf) { w.upload("austin", 3, "mike", "jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "mike withdraws jane on GitHub", do: func(w *wf) { w.withdraw("jane") }, want: "G[mike] H[mike]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[mike] H[mike]"},
			{name: "austin hands jane the turn", do: func(w *wf) { w.handOff("austin", "jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "austin takes mike's turn", do: func(w *wf) { w.takeTurn("austin", "mike") }, want: "G[jane] H[jane]"},
			{name: "jane asks mike again on GitHub", do: func(w *wf) { w.request("mike") }, want: "G[jane mike] H[jane mike]"},
			{name: "jane replies in Gerrit", do: func(w *wf) { w.replyG("jane") }, want: "G[mike] H[mike]"},
			{name: "stranger hands jane the turn", do: func(w *wf) { w.handOff("stranger", "jane") }, want: "G[jane mike] H[mike]"},
			{name: "mike replies in Gerrit", do: func(w *wf) { w.replyG("mike") }, want: "G[jane] H[]"},
			{name: "austin removes jane", do: func(w *wf) { w.removeReviewer("austin", "jane") }, want: "G[] H[]"},
			{name: "austin adds stranger", do: func(w *wf) { w.addReviewer("austin", "stranger") }, want: "G[stranger] H[stranger]"},
			{name: "stranger replies in Gerrit", do: func(w *wf) { w.replyG("stranger") }, want: "G[] H[]"},
			{name: "stranger takes mike's... nobody's turn; asks mike on GitHub", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "stranger takes mike's turn", do: func(w *wf) { w.takeTurn("stranger", "mike") }, want: "G[] H[mike]"},
		}
	})
}

// Reviewers who comment, ask for changes or approve: only those whose
// review that counts asks for changes are asked again for a new patch set.
func TestReviewerWorkflowReviewKinds(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks mike, jane, stranger", do: func(w *wf) { w.request("mike", "jane", "stranger") }, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "mike asks for changes, jane comments, stranger approves", do: func(w *wf) {
				w.review("mike", "CHANGES_REQUESTED")
				w.review("jane", "COMMENTED")
				w.review("stranger", "APPROVED")
			}, want: "G[] H[]"},
			{name: "austin uploads PS3", do: func(w *wf) { w.upload("austin", 3) }, want: "G[mike] H[mike]"},
			{name: "mike approves", do: func(w *wf) { w.review("mike", "APPROVED") }, want: "G[] H[]"},
			{name: "jane asks for changes", do: func(w *wf) { w.review("jane", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[jane] H[jane]"},
			{name: "stranger asks for changes", do: func(w *wf) { w.review("stranger", "CHANGES_REQUESTED") }, want: "G[jane] H[jane]"},
			{name: "owen pushes PS5", do: func(w *wf) { w.push("owen", 5) }, want: "G[jane] H[jane]"},
			{name: "austin uploads PS6", do: func(w *wf) { w.upload("austin", 6) }, want: "G[jane stranger] H[jane stranger]"},
			{name: "jane comments on PS6", do: func(w *wf) { w.review("jane", "COMMENTED") }, want: "G[stranger] H[stranger]"},
			{name: "austin uploads PS7", do: func(w *wf) { w.upload("austin", 7) }, want: "G[jane stranger] H[jane stranger]"},
		}
	})
}

// A reviewer who also uploads: austin, asking for changes on GitHub, is
// not asked for his own patch set, but is for owen's after it.
func TestReviewerWorkflowReviewerUploads(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks austin and mike", do: func(w *wf) { w.request("austin", "mike") }, want: "G[austin mike] H[austin mike]"},
			{name: "both ask for changes", do: func(w *wf) { w.review("austin", "CHANGES_REQUESTED"); w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS3", do: func(w *wf) { w.upload("austin", 3) }, want: "G[mike] H[mike]"},
			{name: "owen uploads PS4 in Gerrit", do: func(w *wf) { w.upload("owen", 4) }, want: "G[austin mike] H[austin mike]"},
			{name: "austin approves", do: func(w *wf) { w.review("austin", "APPROVED") }, want: "G[mike] H[mike]"},
			{name: "austin uploads PS5 %r=jane", do: func(w *wf) { w.upload("austin", 5, "jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "jane asks for changes", do: func(w *wf) { w.review("jane", "CHANGES_REQUESTED") }, want: "G[mike] H[mike]"},
			{name: "austin takes mike's turn", do: func(w *wf) { w.takeTurn("austin", "mike") }, want: "G[] H[]"},
			{name: "jane uploads PS6 (untrusted)", do: func(w *wf) { w.upload("jane", 6) }, want: "G[] H[]"},
			{name: "owen uploads PS7", do: func(w *wf) { w.upload("owen", 7) }, want: "G[jane mike] H[jane mike]"},
		}
	})
}

// Abandoned and restored, in Gerrit or by closing the PR, with and
// without a sync between: whoever is still asked on GitHub is given back
// the turn, once for each abandon.
func TestReviewerWorkflowAbandonRestore(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks jane and mike", do: func(w *wf) { w.request("jane", "mike") }, want: "G[jane mike] H[jane mike]"},
			{name: "austin abandons in Gerrit", do: func(w *wf) { w.abandon("austin", nil) }, want: "G[] H[jane mike]"},
			{name: "austin restores in Gerrit", do: func(w *wf) { w.restore("austin", nil) }, want: "G[jane mike] H[jane mike]"},
			{name: "mike comments", do: func(w *wf) { w.review("mike", "COMMENTED") }, want: "G[jane] H[jane]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[jane]"},
			{name: "owen reopens it", do: func(w *wf) { w.restore("owen", &bridgeGA) }, want: "G[jane] H[jane]"},
			{name: "owen closes and reopens it at once", do: func(w *wf) { w.abandon("owen", &bridgeGA); w.restore("owen", &bridgeGA) }, want: "G[jane] H[jane]"},
			{name: "austin takes jane's turn", do: func(w *wf) { w.takeTurn("austin", "jane") }, want: "G[] H[]"},
			{name: "austin abandons and restores at once", do: func(w *wf) { w.abandon("austin", nil); w.restore("austin", nil) }, want: "G[] H[]"},
			{name: "owen asks mike", do: func(w *wf) { w.request("mike") }, want: "G[mike] H[mike]"},
			{name: "austin abandons", do: func(w *wf) { w.abandon("austin", nil) }, want: "G[] H[mike]"},
			{name: "owen withdraws mike while closed", do: func(w *wf) { w.withdraw("mike") }, want: "G[] H[]"},
			{name: "austin restores", do: func(w *wf) { w.restore("austin", nil) }, want: "G[] H[]"},
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, want: "G[jane] H[jane]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[jane]"},
			{name: "owen reopens it", do: func(w *wf) { w.restore("owen", &bridgeGA) }, want: "G[jane] H[jane]"},
			{name: "jane replies in Gerrit", do: func(w *wf) { w.replyG("jane") }, want: "G[] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[] H[]"},
		}
	})
}

// The trusted group unreadable, and identity lookups failing, at the
// sync after each of austin's doings: each waits, and is carried once.
func TestReviewerWorkflowTrustUnreadable(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, fail: "trust", want: "G[mike] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[mike] H[mike]"},
			{name: "austin takes mike's turn", do: func(w *wf) { w.takeTurn("austin", "mike") }, fail: "trust", want: "G[] H[mike]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[] H[]"},
			{name: "austin hands mike the turn; mike approves", do: func(w *wf) { w.handOff("austin", "mike"); w.review("mike", "APPROVED") }, fail: "trust", want: "G[mike] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[] H[]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS3", do: func(w *wf) { w.upload("austin", 3) }, fail: "trust", want: "G[] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[mike] H[mike]"},
			{name: "mike asks for changes again", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "owen uploads PS4 in Gerrit", do: func(w *wf) { w.upload("owen", 4) }, fail: "lookup", want: "G[] H[]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[mike] H[mike]"},
			{name: "owen asks jane", do: func(w *wf) { w.request("jane") }, fail: "lookup", want: "G[mike] H[jane mike]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane mike] H[jane mike]"},
		}
	})
}

// A hand-off while a draft, in Gerrit or on GitHub, is carried once it
// is ready; a withdrawal is carried at once.
func TestReviewerWorkflowDrafts(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds jane and mike", do: func(w *wf) { w.addReviewer("austin", "jane"); w.addReviewer("austin", "mike") }, want: "G[jane mike] H[jane mike]"},
			{name: "owen makes it a draft", do: func(w *wf) { w.wip("owen", &bridgeGA) }, want: "G[] H[jane mike]"},
			{name: "jane comments", do: func(w *wf) { w.review("jane", "COMMENTED") }, want: "G[] H[mike]"},
			{name: "austin hands jane the turn", do: func(w *wf) { w.handOff("austin", "jane") }, want: "G[jane] H[mike]"},
			{name: "owen withdraws mike", do: func(w *wf) { w.withdraw("mike") }, want: "G[jane] H[]"},
			{name: "owen asks stranger", do: func(w *wf) { w.request("stranger") }, want: "G[jane] H[stranger]"},
			{name: "owen marks it ready", do: func(w *wf) { w.ready("owen", &bridgeGA) }, want: "G[jane mike stranger] H[jane stranger]"},
			{name: "austin marks it WIP", do: func(w *wf) { w.wip("austin", nil) }, want: "G[] H[jane stranger]"},
			{name: "austin uploads PS3", do: func(w *wf) { w.upload("austin", 3) }, want: "G[] H[jane stranger]"},
			{name: "stranger asks for changes", do: func(w *wf) { w.review("stranger", "CHANGES_REQUESTED") }, want: "G[] H[jane]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[] H[jane]"},
			{name: "austin marks it ready", do: func(w *wf) { w.ready("austin", nil) }, want: "G[jane mike stranger] H[jane mike stranger]"},
		}
	})
}

// Patch sets whose export the bridge records after the sync that sees
// them, or never (withheld from GitHub): their turns and asks wait for
// the record, and a withheld one's are nobody's.
func TestReviewerWorkflowUnrecordedPatchSets(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "austin adds mike", do: func(w *wf) { w.addReviewer("austin", "mike") }, want: "G[mike] H[mike]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin uploads PS3 %r=jane, not yet recorded", do: func(w *wf) { w.uploadUnrecorded("austin", 3, "jane") }, want: "G[jane] H[]"},
			{name: "the bridge records PS3", do: func(w *wf) { w.record(3) }, want: "G[jane mike] H[jane mike]"},
			{name: "austin uploads PS4 %r=stranger, withheld", do: func(w *wf) { w.uploadUnrecorded("austin", 4, "stranger") }, want: "G[jane mike stranger] H[jane mike]"},
			{name: "austin uploads PS5", do: func(w *wf) { w.upload("austin", 5) }, want: "G[jane mike stranger] H[jane mike]"},
			{name: "mike asks for changes on PS5", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[jane stranger] H[jane]"},
			{name: "austin uploads PS6, not yet recorded", do: func(w *wf) { w.uploadUnrecorded("austin", 6) }, want: "G[jane stranger] H[jane]"},
			{name: "the bridge records PS6", do: func(w *wf) { w.record(6) }, want: "G[jane mike stranger] H[jane mike]"},
		}
	})
}

// A Gerrit reviewer with no GitHub account: never asked, the bridge
// writing to GitHub only for the others.
func TestReviewerWorkflowNoGitHubAccount(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "austin uploads PS3 %r=nogh,mike", do: func(w *wf) { w.upload("austin", 3, "nogh", "mike") }, want: "G[mike nogh] H[mike]"},
			{name: "nogh replies", do: func(w *wf) { w.replyG("nogh") }, want: "G[mike] H[mike]"},
			{name: "austin hands nogh the turn", do: func(w *wf) { w.handOff("austin", "nogh") }, want: "G[mike nogh] H[mike]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[nogh] H[]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[mike nogh] H[mike]"},
			{name: "austin removes nogh", do: func(w *wf) { w.removeReviewer("austin", "nogh") }, want: "G[mike] H[mike]"},
		}
	})
}

// austin's change abandoned and restored in Gerrit, with and without a
// sync between, and a change request while abandoned.
func TestReviewerWorkflowGerritOwnerAbandonRestore(t *testing.T) {
	runWF(t, "austin", nil, func() []wfStep {
		return []wfStep{
			{name: "austin uploads PS3 %r=mike,jane", do: func(w *wf) { w.upload("austin", 3, "mike", "jane") }, want: "G[jane mike] H[jane mike]"},
			{name: "jane approves", do: func(w *wf) { w.review("jane", "APPROVED") }, want: "G[mike] H[mike]"},
			{name: "austin abandons", do: func(w *wf) { w.abandon("austin", nil) }, want: "G[] H[mike]"},
			{name: "austin restores", do: func(w *wf) { w.restore("austin", nil) }, want: "G[mike] H[mike]"},
			{name: "austin abandons and restores at once", do: func(w *wf) { w.abandon("austin", nil); w.restore("austin", nil) }, want: "G[mike] H[mike]"},
			{name: "austin abandons", do: func(w *wf) { w.abandon("austin", nil) }, want: "G[] H[mike]"},
			{name: "mike asks for changes while abandoned", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[] H[]"},
			{name: "austin restores", do: func(w *wf) { w.restore("austin", nil) }, want: "G[] H[]"},
			{name: "austin uploads PS4", do: func(w *wf) { w.upload("austin", 4) }, want: "G[mike] H[mike]"},
		}
	})
}

// Giving turns back after a reopen when it fails for now, someone asked
// while closed, and asks again around a taken turn.
func TestReviewerWorkflowGiveBackAndReAskEdges(t *testing.T) {
	runWF(t, "owen", nil, func() []wfStep {
		return []wfStep{
			{name: "owen asks jane and mike", do: func(w *wf) { w.request("jane", "mike") }, want: "G[jane mike] H[jane mike]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[jane mike]"},
			{name: "owen asks stranger while closed", do: func(w *wf) { w.request("stranger") }, want: "G[] H[jane mike stranger]"},
			{name: "owen reopens it (Gerrit refuses)", do: func(w *wf) { w.restore("owen", &bridgeGA) }, fail: "gerrit500", want: "G[] H[jane mike stranger]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "owen closes the PR", do: func(w *wf) { w.abandon("owen", &bridgeGA) }, want: "G[] H[jane mike stranger]"},
			{name: "owen reopens it (lookups fail)", do: func(w *wf) { w.restore("owen", &bridgeGA) }, fail: "lookup", want: "G[] H[jane mike stranger]"},
			{name: "nothing", do: func(w *wf) {}, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "all three ask for changes", do: func(w *wf) {
				w.review("jane", "CHANGES_REQUESTED")
				w.review("mike", "CHANGES_REQUESTED")
				w.review("stranger", "CHANGES_REQUESTED")
			}, want: "G[] H[]"},
			{name: "austin uploads PS3 and owen PS4", do: func(w *wf) { w.upload("austin", 3); w.upload("owen", 4) }, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "jane uploads PS5 (untrusted)", do: func(w *wf) { w.upload("jane", 5) }, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "austin takes mike's turn, then uploads PS6", do: func(w *wf) { w.takeTurn("austin", "mike"); w.upload("austin", 6) }, want: "G[jane mike stranger] H[jane mike stranger]"},
			{name: "mike asks for changes", do: func(w *wf) { w.review("mike", "CHANGES_REQUESTED") }, want: "G[jane stranger] H[jane stranger]"},
			{name: "nogh (untrusted) hands mike the turn", do: func(w *wf) { w.handOff("nogh", "mike") }, want: "G[jane mike stranger] H[jane stranger]"},
			{name: "austin uploads PS7, then takes mike's turn", do: func(w *wf) { w.upload("austin", 7); w.takeTurn("austin", "mike") }, want: "G[jane stranger] H[jane stranger]"},
		}
	})
}
