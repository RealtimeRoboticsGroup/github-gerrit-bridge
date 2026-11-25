package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/ldap"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// GitHubAccounts gives Gerrit accounts' GitHub accounts, for review
// requests (design §6.6).
type GitHubAccounts interface {
	// GitHubFor is a's GitHub account, under its login now. err: who is
	// who is unknown for now.
	GitHubFor(ctx context.Context, a gerrit.AccountInfo) (github.User, bool, error)
}

// ReviewerAccounts is GitHubAccounts: the GitHub account a Gerrit account
// linked, else the one the directory gives it, else (without a directory)
// a login an override gives it or, matching usernames, its username; each
// only while the identity mapping maps it back to that account (as
// Users.Lookup decides whose a GitHub account's comments and votes are).
type ReviewerAccounts struct {
	// Linked is the GitHub account a Gerrit account linked (Tokens.Linked).
	Linked    func(ctx context.Context, account int) (github.User, bool, error)
	Directory interface {
		ByGerritUser(user string) (ldap.Account, bool, error)
	}
	// Overrides are identity.github_to_gerrit (login -> Gerrit user), and
	// MatchUsernames identity.match_usernames.
	Overrides      map[string]string
	MatchUsernames bool
	Users          Identity
	GitHub         interface {
		UserByID(ctx context.Context, id int64) (github.User, error)
		UserID(ctx context.Context, login string) (int64, time.Time, error)
	}
}

// GitHubFor implements GitHubAccounts.
func (r *ReviewerAccounts) GitHubFor(ctx context.Context, a gerrit.AccountInfo) (github.User, bool, error) {
	var id int64
	if r.Linked != nil && a.AccountID != 0 {
		u, ok, err := r.Linked(ctx, a.AccountID)
		if err != nil {
			return github.User{}, false, err
		}
		if ok {
			id = u.ID
		}
	}
	if id == 0 && r.Directory != nil {
		d, found, err := r.Directory.ByGerritUser(a.Username)
		if err != nil {
			return github.User{}, false, &IdentityUnknown{err}
		}
		if found {
			id = d.GitHubID
		}
	}
	if id == 0 && r.Directory == nil {
		var logins []string
		for _, l := range slices.Sorted(maps.Keys(r.Overrides)) {
			if strings.EqualFold(r.Overrides[l], a.Username) {
				logins = append(logins, l)
			}
		}
		if r.MatchUsernames {
			logins = append(logins, a.Username)
		}
		for _, l := range logins {
			n, _, err := r.GitHub.UserID(ctx, l)
			if github.IsNotFound(err) {
				continue
			}
			if err != nil {
				return github.User{}, false, err
			}
			if gh, ok, err := r.check(ctx, a, github.User{Login: l, ID: n}); err != nil || ok {
				return gh, ok, err
			}
		}
		return github.User{}, false, nil
	}
	if id == 0 {
		return github.User{}, false, nil
	}
	// Requests go by login, which a rename frees for anyone to take: the
	// account's login now, by its id.
	gh, err := r.GitHub.UserByID(ctx, id)
	if github.IsNotFound(err) {
		return github.User{}, false, nil
	}
	if err != nil {
		return github.User{}, false, err
	}
	return r.check(ctx, a, gh)
}

// check is gh if the mapping maps it to a.
func (r *ReviewerAccounts) check(ctx context.Context, a gerrit.AccountInfo, gh github.User) (github.User, bool, error) {
	user, ok, err := askOnly(ctx, r.Users, gh)
	if err != nil || !ok || !strings.EqualFold(user, a.Username) {
		return github.User{}, false, err
	}
	return gh, true, nil
}

// reviewerReason begins the reasons the bridge gives Gerrit for attention
// set changes.
const reviewerReason = "GitHub: "

// syncReviewers carries reviewers, and whose turn it is, over both ways
// (design §6.6), from what changed on each side since the last sync saw
// them (prev; nil: never):
//
//   - Gerrit → GitHub: a reviewer a trusted person puts in the attention
//     set is asked to review the PR (again, if they have reviewed); one a
//     trusted person takes out of it, or who replies, is no longer asked.
//     A patch set a trusted person uploads asks again whoever last
//     requested changes on GitHub, as Gerrit hands nobody the turn then.
//   - GitHub → Gerrit: someone asked to review becomes a Gerrit reviewer,
//     or is put in the attention set if they are one; a request that ends
//     (withdrawn, or by their review) takes them out of it. Someone with no
//     Gerrit account is said so on the change, once per request.
//
// What the bridge did is not carried back, nor anything Gerrit cannot say
// who did: a PR nobody touches in Gerrit never has the bridge write to
// GitHub for its reviewers. A PR from before the bridge started is taken
// as it is the first time. What fails is logged and tried again by the
// next sync; it never holds up the comments.
func (c *Comments) syncReviewers(ctx context.Context, n, pr int, s *commentSnapshot, prev *state.Reviewers) *state.Reviewers {
	if c.Reviewers == nil {
		return prev
	}
	if s.pull == nil || s.pull.State != "open" || s.status != "NEW" {
		return prev // closed: nothing to carry; reopened, what changed since
	}
	attention := map[string]gerrit.AttentionSetInfo{} // by lower-case username
	for _, a := range s.attention {
		if a.Account.Username != "" {
			attention[strings.ToLower(a.Account.Username)] = a
		}
	}
	removed := map[string]gerrit.AttentionSetInfo{}
	for _, a := range s.removedAttention {
		if a.Account.Username != "" {
			removed[strings.ToLower(a.Account.Username)] = a
		}
	}
	requested := map[int64]github.User{}
	for _, u := range s.pull.RequestedReviewers {
		requested[u.ID] = u
	}
	attentionNow := slices.Sorted(maps.Keys(attention))
	if prev != nil && prev.PR != pr {
		prev = nil // linked to another PR since: what was seen was of that one (fourth review)
	}
	if prev == nil {
		if s.pull.CreatedAt.Before(c.Since) {
			// From before the bridge started: as it is.
			reqs := map[int64]state.Request{}
			for id, u := range requested {
				reqs[id] = state.Request{Login: u.Login, Recorded: true}
			}
			return &state.Reviewers{At: s.at, PR: pr, PatchSet: s.current, Attention: attentionNow, AttentionAt: attentionAt(attention), Requests: reqs}
		}
		prev = &state.Reviewers{} // new: everything on it is
	}
	reviewer := map[string]bool{}
	for _, a := range s.reviewers["REVIEWER"] {
		reviewer[strings.ToLower(a.Username)] = true
	}
	wasAttention := map[string]bool{}
	for _, u := range prev.Attention {
		wasAttention[u] = true
	}
	// sameTurn reports whether a is the turn seen before: one taken back
	// and given again since (or a stale one kept, waiting) is new (sixth
	// review: a removal kept waiting for good hid a later turn).
	// The bridge's own ("*") is the same while Gerrit still gives its
	// reason: an entry Gerrit wrote again since is new (seventh review:
	// one a trusted person handed again was taken for the bridge's).
	sameTurn := func(key string, a gerrit.AttentionSetInfo) bool {
		at, ok := prev.AttentionAt[key]
		bridges := strings.HasPrefix(at, "*") && (strings.HasPrefix(a.Reason, reviewerReason) || a.Reason == "Reviewer was added")
		return wasAttention[key] && (!ok || bridges || at == a.LastUpdate)
	}
	// What this look saw: the newest time Gerrit gave for anything.
	gerritAt := ""
	for _, u := range s.reviewerUpdates {
		gerritAt = max(gerritAt, u.Updated)
	}
	for _, m := range s.allMessages {
		gerritAt = max(gerritAt, m.Date)
	}
	for _, a := range attention {
		gerritAt = max(gerritAt, a.LastUpdate)
	}
	for _, a := range removed {
		gerritAt = max(gerritAt, a.LastUpdate)
	}
	// The requests seen before, as this look finds them: one no longer
	// asked has ended, now if not before. One asked again since it ended
	// (its turn not settled) is a new request (eleventh review: taken for
	// the old one, never carried; twelfth: said again).
	reqs := map[int64]state.Request{}
	for id, r := range prev.Requests {
		_, asked := requested[id]
		if asked && r.Ended {
			continue
		}
		if !asked && !r.Ended {
			r.Ended, r.EndedAt = true, gerritAt
		}
		reqs[id] = r
	}
	// was reports whether id's request was carried before, and goes on.
	was := func(id int64) bool {
		r, ok := reqs[id]
		return ok && r.Recorded && !r.Ended
	}
	noted := map[int64]bool{}
	for id, r := range reqs {
		if r.Noted && !r.Ended {
			noted[id] = true
		}
	}
	// Retries: what failed is left to look new (or gone) next time; and
	// whom the bridge took out of the attention set is out.
	// The bridge's own turns, handed out for GitHub requests, are seen as
	// they are made: Gerrit names nobody for one, and a person's post in
	// the same second would be taken for its maker (third review).
	// stillSeen is when each was taken out, as Gerrit said ("*": put in
	// by the bridge; "?": in, its lookup failed, so new again next time).
	notSeen, stillSeen := map[string]bool{}, map[string]string{}
	// notRequested: requests left unrecorded; taken, turns an ended
	// request's end took (or Gerrit will not let it).
	notRequested, taken := map[int64]bool{}, map[string]bool{}
	// Whether a is trusted; known false: the group could not be read, and
	// what a did waits for the next sync, unseen.
	// Asked once a sync for each (the group is read afresh each time).
	type trust struct{ trusted, known bool }
	trustOf := map[int]trust{}
	trusted := func(a gerrit.AccountInfo) (bool, bool) {
		if c.Trusted == nil || c.CIAccount != "" && strings.EqualFold(a.Username, c.CIAccount) {
			return false, true // fifth review
		}
		t, ok := trustOf[a.AccountID]
		if !ok {
			t.trusted, t.known = c.Trusted(ctx, a.AccountID)
			trustOf[a.AccountID] = t
		}
		return t.trusted, t.known
	}
	askedFor := maps.Clone(prev.AskedFor)
	if askedFor == nil {
		askedFor = map[int64]int{}
	}
	githubFor := func(a gerrit.AccountInfo) (github.User, bool, error) {
		gh, ok, err := c.Reviewers.GitHubFor(ctx, a)
		if err != nil {
			c.Logger.Warn("finding a Gerrit reviewer's GitHub account", "change", n, "gerrit_user", a.Username, "err", err)
		}
		return gh, ok, err
	}
	// askedNow are those the bridge asked on GitHub this sync; handedOn,
	// those asked (by it or anyone) whose Gerrit turn is theirs already.
	askedNow, handedOn := map[int64]bool{}, map[int64]bool{}
	ask := func(gh github.User) error {
		if err := c.GitHub.RequestReviewers(ctx, pr, []string{gh.Login}); err != nil {
			c.Logger.Warn("asking someone to review on GitHub", "change", n, "pr", pr, "github_login", gh.Login, "err", err)
			return err
		}
		requested[gh.ID] = gh
		askedNow[gh.ID] = true
		return nil
	}

	// Gerrit → GitHub. Not while a draft: it is their turn once ready.
	for _, key := range attentionNow {
		a := attention[key]
		if sameTurn(key, a) || sameAccount(a.Account, s.owner) {
			continue
		}
		if !reviewer[key] {
			// A CC's turn is not seen: made a reviewer while it is still
			// theirs, they are asked then (third review).
			notSeen[key] = true
			continue
		}
		if s.pull.Draft {
			// Unless asked on GitHub already: nothing to carry once
			// ready, and its end is (eleventh review: a turn handed
			// while a draft, never seen, never withdrew it).
			// Asked before it was a draft: a request made while one is
			// not recorded, to be carried once ready.
			if gh, ok, err := githubFor(a.Account); err != nil || !ok {
				notSeen[key] = true
			} else if _, asked := requested[gh.ID]; !asked {
				notSeen[key] = true
			} else if !was(gh.ID) {
				notSeen[key] = true
			}
			continue
		}
		gh, linked, err := githubFor(a.Account)
		if err != nil {
			// Looked at again next time, but there: taken out meanwhile,
			// by someone trusted, their request is withdrawn (thirteenth
			// review: never seen, its taking never was).
			notSeen[key], stillSeen[key] = true, "?"+a.LastUpdate
			continue
		}
		_, asked := requested[gh.ID]
		if r, ok := reqs[gh.ID]; linked && !asked && ok && r.Ended && a.LastUpdate <= r.Until {
			// Given by a look that saw them asked: their request's end
			// takes it, whoever gave it (twelfth review: an unseen
			// hand-off undid a withdrawal).
			if err := c.Gerrit.RemoveFromAttentionSet(ctx, n, a.Account.AccountID, reviewerReason+"review request ended"); err != nil {
				c.Logger.Warn("taking someone no longer asked on GitHub out of the attention set", "change", n, "gerrit_user", a.Account.Username, "err", err)
				if !refused(err) {
					notSeen[key] = true
					continue
				}
			} else {
				notSeen[key] = true
			}
			taken[key] = true
			continue
		}
		if asked && linked {
			// Asked already: nothing to carry, whoever gave the turn
			// (twelfth review: a lookup failing later left the request
			// to look new, so its end never took the turn; and a turn
			// left unseen, waiting on its patch set's record, was not
			// seen to end, by their own reply say).
			handedOn[gh.ID] = true
			continue
		}
		who, ok, wait := c.actorAt(s, a)
		if wait {
			notSeen[key] = true
			continue
		}
		if !ok {
			continue
		}
		if t, known := trusted(who); !known {
			notSeen[key] = true
			continue
		} else if !t {
			continue
		}
		if !linked || gh.ID == s.pull.User.ID {
			continue
		}
		if reviewedAfter(s, gh.ID, a.LastUpdate) {
			// Answered on GitHub since (ninth review: asked again, and
			// handed the turn back): theirs no longer.
			if err := c.Gerrit.RemoveFromAttentionSet(ctx, n, a.Account.AccountID, reviewerReason+"reviewed on GitHub"); err != nil {
				c.Logger.Warn("taking someone who reviewed on GitHub out of the attention set", "change", n, "gerrit_user", a.Account.Username, "err", err)
				if !refused(err) {
					notSeen[key] = true
				}
				continue
			}
			// Out now: asked again below (a new patch set), they are
			// given it back (tenth review: the add was skipped, the
			// attention set as read still having them).
			delete(attention, key)
			notSeen[key] = true
			continue
		}
		if err := ask(gh); err != nil && !githubRefusedForGood(err) {
			notSeen[key] = true
		} else if err == nil {
			handedOn[gh.ID] = true
		}
	}
	for _, key := range prev.Attention {
		r := removed[key] // none: nobody Gerrit names took them out
		if _, in := attention[key]; in || clearedReasons[r.Reason] || prev.AttentionAt[key] == "*"+r.LastUpdate {
			// Or Gerrit cleared the set (WIP, abandoned, submitted): not
			// anyone ending their turn, so their request stands (seventh
			// review: a WIP withdrew it, and ready never asked again).
			continue
		}
		who, ok, wait := c.actorAt(s, r)
		if wait {
			stillSeen[key] = r.LastUpdate
			continue
		}
		if !ok {
			continue
		}
		if !sameAccount(who, r.Account) {
			if t, known := trusted(who); !known {
				stillSeen[key] = r.LastUpdate
				continue
			} else if !t {
				continue
			}
		}
		gh, ok, err := githubFor(r.Account)
		if err != nil {
			stillSeen[key] = r.LastUpdate
			continue
		}
		u, asked := requested[gh.ID]
		if !ok || !asked || reviewedAfter(s, gh.ID, r.LastUpdate) {
			// Or asked again since: a review after the removal ended the
			// request it was about (ninth review: an older removal
			// withdrew a newer request).
			continue
		}
		if err := c.GitHub.RemoveRequestedReviewers(ctx, pr, []string{u.Login}); err != nil {
			c.Logger.Warn("withdrawing a review request on GitHub", "change", n, "pr", pr, "github_login", u.Login, "err", err)
			if !githubRefusedForGood(err) {
				stillSeen[key] = r.LastUpdate
			}
			continue
		}
		delete(requested, gh.ID)
	}
	if !s.pull.Draft {
		c.askAgain(ctx, s, prev, removed, requested, askedFor, trusted, ask)
	}

	// Gerrit gave nobody back the turn its abandon took, on restore:
	// whoever is still asked on GitHub is given it back, once for each
	// abandon (seventh and ninth reviews).
	givenBack := map[string]string{}
	giveBack := map[int64]string{} // whose turn, by GitHub id
	pending := map[string]string{}
	for key, r := range removed {
		if r.Reason != "Change was abandoned" {
			continue
		}
		if _, in := attention[key]; in {
			continue
		}
		if prev.GivenBack[key] == r.LastUpdate {
			givenBack[key] = r.LastUpdate
		} else {
			pending[key] = r.LastUpdate
		}
	}
	if len(pending) > 0 {
		known := true
		for id, u := range requested {
			if !was(id) {
				continue // new: added as any request is
			}
			user, ok, err := askOnly(ctx, c.Users, u)
			if err != nil {
				known = false
				continue
			}
			// Not if removed as a reviewer since (eleventh review: given
			// back, they were made one again).
			if at, out := pending[strings.ToLower(user)]; ok && out && !removedSince(s, user, at) {
				giveBack[id] = strings.ToLower(user)
			}
		}
		if known {
			maps.Copy(givenBack, pending)
		}
	}

	// GitHub → Gerrit. Not while a draft (a WIP change in Gerrit, where
	// it is nobody's turn): once ready (third review).
	for _, id := range slices.Sorted(maps.Keys(requested)) {
		u := requested[id]
		// Asked again by the bridge (their request had ended since last
		// seen): their turn in Gerrit too (fifth review).
		was := was(id) && !askedNow[id]
		if was && !noted[id] && giveBack[id] == "" || handedOn[id] {
			// Or asked for the Gerrit turn they have: nothing to carry,
			// and a lookup failing must not leave the request unseen
			// (eleventh review: its end then never took their turn).
			continue
		}
		if s.pull.Draft {
			if !was {
				notRequested[id] = true
			}
			continue
		}
		user, ok, err := askOnly(ctx, c.Users, u)
		if err != nil {
			c.Logger.Warn("finding the Gerrit account of someone asked to review on GitHub", "change", n, "github_login", u.Login, "err", err)
			if !was {
				notRequested[id] = true
			}
			delete(givenBack, giveBack[id]) // given back next time
			continue
		}
		if !ok {
			if noted[id] || u.Type == "Bot" {
				continue
			}
			msg := fmt.Sprintf("@%s was asked to review on GitHub (%s), but has no Gerrit account the bridge knows, so is not a reviewer here. "+
				"They are added once they have one (by signing in to Gerrit).", u.Login, s.pull.HTMLURL)
			if err := c.Gerrit.SetReview(ctx, n, s.revision, gerrit.ReviewInput{Message: msg, Tag: commentTag, Notify: "OWNER",
				// A note: nobody's turn (third review).
				IgnoreAutomaticAttentionSetRules: true}); err != nil {
				c.Logger.Warn("saying in Gerrit that someone asked to review on GitHub has no Gerrit account", "change", n, "github_login", u.Login, "err", err)
				if !refused(err) {
					notRequested[id] = true
				}
				continue
			}
			noted[id] = true
			continue
		}
		delete(noted, id)
		key := strings.ToLower(user)
		if strings.EqualFold(user, s.owner.Username) {
			continue
		}
		_, in := attention[key]
		if r, ok := reqs[id]; ok && !r.Recorded {
			at := r.Seen
			// Removed as a reviewer, or taken out of the attention set
			// by someone (not Gerrit clearing it, nor the bridge), after
			// the request was first seen: that stands (twelfth review:
			// carried late, it undid the removal, or their own reply).
			// An empty time is before anything in Gerrit (thirteenth
			// review: taken for none, a removal since was undone).
			since := at
			if at != "" {
				since += "~"
			}
			r, out := removed[key]
			if !reviewer[key] && removedSince(s, user, since) ||
				out && r.LastUpdate > at && !clearedReasons[r.Reason] && !strings.HasPrefix(r.Reason, reviewerReason) {
				continue
			}
		}
		if !reviewer[key] {
			err = c.Gerrit.SetReview(ctx, n, s.revision, gerrit.ReviewInput{Tag: commentTag, Reviewers: []gerrit.ReviewerInput{{Reviewer: user}}})
		} else if !in {
			err = c.Gerrit.AddToAttentionSet(ctx, n, user, reviewerReason+"asked to review on GitHub")
		}
		if err == nil && (!in || !reviewer[key]) {
			// Made a reviewer while their turn as a CC was not seen:
			// Gerrit writes it again, as the bridge's (tenth review).
			// With their removal as it was: Gerrit may give no turn
			// (a WIP change), and that removal is not a new one then
			// (eighth review: it withdrew the request just made).
			stillSeen[key] = "*" + removed[key].LastUpdate
		}
		if err != nil && !refused(err) {
			c.Logger.Warn("making someone asked to review on GitHub a Gerrit reviewer", "change", n, "gerrit_user", user, "github_login", u.Login, "err", err)
			if giveBack[id] != "" {
				// Given back next time, still asked (eleventh review:
				// left to look new, a removal since was undone).
				delete(givenBack, giveBack[id])
			} else {
				notRequested[id] = true
			}
		} else if err != nil {
			c.Logger.Info("Gerrit will not make someone asked to review on GitHub a reviewer", "change", n, "gerrit_user", user, "github_login", u.Login, "err", err)
		}
	}
	// Ended requests: each takes the turn it was for, seen before its end
	// was, and is kept until that turn is settled (twelfth review: a
	// draft's, withdrawn, left the turn it was for).
	kept := map[int64]state.Request{}
	for _, id := range slices.Sorted(maps.Keys(reqs)) {
		r := reqs[id]
		if _, asked := requested[id]; asked || !r.Ended {
			continue // or asked again by the bridge: a new request
		}
		if r.Login == "" {
			kept[id] = r // from an earlier build: whose is not known
			continue
		}
		user, _, err := askOnly(ctx, c.Users, github.User{ID: id, Login: r.Login})
		if err != nil {
			kept[id] = r
			continue
		}
		key := strings.ToLower(user)
		// A withdrawal waiting for this request: it has ended (twelfth
		// review: kept, it withdrew a request made again later).
		delete(stillSeen, key)
		a, in := attention[key]
		if !in || taken[key] || a.LastUpdate > r.EndedAt {
			// Settled: or given after the end was seen, not the turn the
			// request was for (tenth review: an old request's end took a
			// new turn; twelfth: one given while its end waited).
			continue
		}
		if !sameTurn(key, a) {
			if notSeen[key] && a.LastUpdate <= r.Until {
				kept[id] = r // taken once seen (a CC's, a draft's)
			}
			continue
		}
		// Their review ends it too (mirrored as them, it usually has).
		if err := c.Gerrit.RemoveFromAttentionSet(ctx, n, a.Account.AccountID, reviewerReason+"review request ended"); err != nil {
			c.Logger.Warn("taking someone no longer asked on GitHub out of the attention set", "change", n, "gerrit_user", user, "err", err)
			if !refused(err) {
				kept[id] = r
			}
			continue
		}
		notSeen[key] = true
	}

	next := &state.Reviewers{At: s.at, PR: pr, PatchSet: prev.PatchSet}
	if len(askedFor) > 0 {
		next.AskedFor = askedFor
	}
	if len(givenBack) > 0 {
		next.GivenBack = givenBack
	}
	next.AttentionAt = map[string]string{}
	for _, key := range attentionNow {
		if !notSeen[key] {
			next.Attention = append(next.Attention, key)
			next.AttentionAt[key] = attention[key].LastUpdate
		}
	}
	for key, at := range stillSeen { // none of whose turn it is now
		next.Attention = append(next.Attention, key)
		next.AttentionAt[key] = at
	}
	slices.Sort(next.Attention)
	for id, u := range requested {
		r := state.Request{Login: u.Login, Seen: gerritAt, Until: gerritAt, Recorded: !notRequested[id], Noted: noted[id] && !notRequested[id]}
		if p, ok := reqs[id]; ok && !p.Ended && (r.Recorded || !p.Recorded) {
			r.Seen = p.Seen // when first seen (as such, if unrecorded)
		}
		kept[id] = r
	}
	if len(kept) > 0 {
		next.Requests = kept
	}
	return next
}

// actorAt is who put a in (or out of) the attention set, as far as Gerrit
// says: the author of the message or reviewer update made with it. ok
// false: nobody Gerrit names, or the bridge on someone's behalf (a
// mirrored post, Run-As: tagged as the bridge's, or with a real author,
// though Gerrit records the reviewer it makes of them as their doing).
// The bridge's own account is never trusted (Checks.CheckUntrusted), so
// what it does as itself is not carried either. wait: made with a patch
// set the bridge has yet to record as exported or imported, so whose it
// is cannot be told yet (fourth review: a push with %r= the comment sync
// got to first was taken as seen, and its reviewer never asked).
func (c *Comments) actorAt(s *commentSnapshot, a gerrit.AttentionSetInfo) (who gerrit.AccountInfo, ok, wait bool) {
	// Gerrit's times are to the second: two people's doings in one second
	// cannot be told apart, so neither is taken (review of the redesign).
	var named []gerrit.AccountInfo
	mirrored := false
	for sha, r := range s.revInfos {
		if r.Created != a.LastUpdate {
			continue
		}
		switch {
		case s.imported(sha):
			mirrored = true // Gerrit says its author uploaded it
		case !s.recorded(sha) && sha == s.revision:
			wait = true // for the record, which may come
		case !s.exported(sha):
			// Recorded neither way, or never (withheld from GitHub, and a
			// later one current): not known as anyone's, and never will
			// be (sixth review: it waited for good).
			mirrored = true
		}
	}
	for _, m := range s.allMessages {
		if m.Date == a.LastUpdate {
			if m.RealAuthor != nil || isBridgeTag(m.Tag) {
				mirrored = true
			} else {
				named = append(named, m.Author)
			}
		}
	}
	for _, u := range s.reviewerUpdates {
		if u.Updated == a.LastUpdate && sameAccount(u.Reviewer, a.Account) {
			named = append(named, u.UpdatedBy)
		}
	}
	if mirrored {
		return gerrit.AccountInfo{}, false, false
	}
	if wait {
		return gerrit.AccountInfo{}, false, true
	}
	if len(named) == 0 {
		return gerrit.AccountInfo{}, false, false
	}
	for _, w := range named[1:] {
		if !sameAccount(w, named[0]) {
			return gerrit.AccountInfo{}, false, false
		}
	}
	return named[0], true, false
}

// exported reports whether the bridge recorded Gerrit commit sha as
// exported from Gerrit (state.Revision.Exported), and never as imported. Only then is a patch
// set its uploader's: an import, which Gerrit says its author uploaded,
// is not theirs, nor is one not yet recorded either way (an import whose
// state the bridge has yet to save). Each export queues a comment sync
// once recorded (Code.OnExported).
func (s *commentSnapshot) exported(sha string) bool {
	return slices.ContainsFunc(s.revisions, func(r state.Revision) bool { return r.GerritSHA == sha && r.Exported }) && !s.imported(sha)
}

// imported reports whether the bridge recorded Gerrit commit sha as
// imported from GitHub.
func (s *commentSnapshot) imported(sha string) bool {
	return slices.ContainsFunc(s.revisions, func(r state.Revision) bool { return r.GerritSHA == sha && r.Imported })
}

// recorded reports whether the bridge recorded Gerrit commit sha, as
// imported or exported.
func (s *commentSnapshot) recorded(sha string) bool {
	return slices.ContainsFunc(s.revisions, func(r state.Revision) bool { return r.GerritSHA == sha })
}

// uploader is who really uploaded r: for an import, the bridge.
func uploader(r gerrit.RevisionInfo) gerrit.AccountInfo {
	if r.RealUploader != nil {
		return *r.RealUploader
	}
	return r.Uploader
}

// askAgain asks again, once for each, whoever last requested changes
// on GitHub, for the newest patch set a trusted person uploaded since
// the PR was first seen (Gerrit hands nobody the turn on an upload).
// askedFor records each asked, or found to have answered it; what fails
// for now is not, and is tried again next sync. Not:
//   - whoever uploaded it;
//   - whoever reviewed it, or a later patch set's commit, on GitHub;
//   - whoever was taken out of the attention set at or after it, but by
//     Gerrit clearing the set (fifth review) or by the bridge (twelfth:
//     its taking the turn of a request that ended);
//   - while whether a newer one is a trusted person's cannot be told.
//
// Once asked, a withdrawal on GitHub stands (ninth review: kept as a
// patch-set number held back, the asks were made again, or lost, as
// what was seen moved on).
func (c *Comments) askAgain(ctx context.Context, s *commentSnapshot, prev *state.Reviewers, removed map[string]gerrit.AttentionSetInfo,
	requested map[int64]github.User, askedFor map[int64]int, trusted func(gerrit.AccountInfo) (bool, bool), ask func(github.User) error) {
	// The newest exported one a trusted person uploaded; one newer whose
	// uploader's trust cannot be told waits (third review).
	var newest gerrit.RevisionInfo
	for _, sha := range slices.SortedFunc(maps.Keys(s.revInfos), func(a, b string) int { return s.revInfos[b].Number - s.revInfos[a].Number }) {
		r := s.revInfos[sha]
		if r.Number <= prev.PatchSet {
			break
		}
		if !s.exported(sha) {
			continue
		}
		t, known := trusted(uploader(r))
		if !known {
			return
		}
		if t {
			newest = r
			break
		}
	}
	if newest.Number == 0 {
		return
	}
	// Its commits on GitHub, and those of every later patch set (an
	// import of a push made on it, say).
	// A commit recorded as no patch set is later too: a push not yet
	// imported (twelfth review: a review of it was asked again).
	pushed, older := map[string]bool{}, map[string]bool{}
	for _, rec := range s.revisions {
		if rec.PatchSet >= newest.Number {
			pushed[rec.GitHubSHA] = true
		} else {
			older[rec.GitHubSHA] = true
		}
	}
	for _, rv := range latestReviews(s.allReviews) {
		u := rv.User
		if rv.State != "CHANGES_REQUESTED" || u.Type == "Bot" || askedFor[u.ID] >= newest.Number {
			continue
		}
		if _, asked := requested[u.ID]; asked || slices.ContainsFunc(s.allReviews, func(rv github.Review) bool { return rv.User.ID == u.ID && (pushed[rv.CommitID] || !older[rv.CommitID]) }) {
			askedFor[u.ID] = newest.Number
			continue
		}
		user, ok, err := askOnly(ctx, c.Users, u)
		if err != nil {
			continue // asked once known
		}
		if ok {
			r, out := removed[strings.ToLower(user)]
			if strings.EqualFold(user, uploader(newest).Username) || out && r.LastUpdate >= newest.Created && !clearedReasons[r.Reason] && !strings.HasPrefix(r.Reason, reviewerReason) ||
				removedSince(s, user, gerritTime(rv.SubmittedAt)) {
				askedFor[u.ID] = newest.Number
				continue
			}
		}
		if err := ask(u); err == nil || githubRefusedForGood(err) {
			askedFor[u.ID] = newest.Number
		}
	}
}

// reviewedAfter reports whether GitHub account id reviewed the PR after
// Gerrit time at.
func reviewedAfter(s *commentSnapshot, id int64, at string) bool {
	t, err := time.Parse("2006-01-02 15:04:05.999999999", at)
	// Not the bridge's mirror of a Gerrit reply, stamped when it was
	// posted (tenth review: one posted after a later hand-off undid it).
	return err == nil && slices.ContainsFunc(s.allReviews, func(rv github.Review) bool {
		return rv.User.ID == id && rv.SubmittedAt.After(t) && !s.mirrored[rv.ID]
	})
}

// removedSince reports whether Gerrit user was last removed as a
// reviewer, at or after Gerrit time at: no longer theirs to review
// (eleventh review: asked again, they were made a reviewer again).
func removedSince(s *commentSnapshot, user, at string) bool {
	last := gerrit.ReviewerUpdate{}
	for _, u := range s.reviewerUpdates {
		if strings.EqualFold(u.Reviewer.Username, user) && u.Updated >= last.Updated {
			last = u
		}
	}
	return last.State == "REMOVED" && last.Updated >= at
}

// gerritTime is t as Gerrit writes times.
func gerritTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000000")
}

// clearedReasons are Gerrit's reasons for clearing the attention set.
var clearedReasons = map[string]bool{
	"Change was marked work in progress": true,
	"Change was abandoned":               true,
	"Change was submitted":               true,
}

// attentionAt is when Gerrit says each in the attention set was put in.
func attentionAt(attention map[string]gerrit.AttentionSetInfo) map[string]string {
	at := map[string]string{}
	for key, a := range attention {
		at[key] = a.LastUpdate
	}
	return at
}

// githubRefusedForGood reports whether GitHub refused err's request for good (a
// 4xx other than a conflict or a rate limit: a 422 for someone who may not
// review, a 403 or 404 for a token that may not): not made again every
// sync, each a GitHub write (third review). Not a 401, which is about the
// token, nor a secondary rate limit, which may come with no header saying
// how long to wait (fifth review).
func githubRefusedForGood(err error) bool {
	var ae *github.APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusConflict && ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusUnauthorized &&
		ae.RetryAfter == 0 && !strings.Contains(strings.ToLower(ae.Message), "rate limit")
}

// refused reports whether Gerrit refused err's request for good (a 4xx
// other than a conflict or a limit): not tried again every sync.
func refused(err error) bool {
	var ge *gerrit.Error
	return errors.As(err, &ge) && ge.Status >= 400 && ge.Status < 500 && ge.Status != http.StatusConflict && ge.Status != http.StatusTooManyRequests
}

// latestReviews is each reviewer's review that GitHub counts: their
// latest approval, change request or dismissal.
func latestReviews(rs []github.Review) []github.Review {
	latest := map[int64]github.Review{}
	for _, r := range rs {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			continue
		}
		if l, ok := latest[r.User.ID]; !ok || !r.SubmittedAt.Before(l.SubmittedAt) {
			latest[r.User.ID] = r
		}
	}
	return slices.SortedFunc(maps.Values(latest), func(a, b github.Review) int { return cmp.Compare(a.User.ID, b.User.ID) })
}
