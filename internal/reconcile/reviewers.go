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
	if prev == nil {
		if s.pull.CreatedAt.Before(c.Since) {
			// From before the bridge started: as it is.
			return &state.Reviewers{At: s.at, PatchSet: s.current, Attention: attentionNow, Requested: accounts(requested)}
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
	wasRequested := map[int64]github.User{}
	for _, a := range prev.Requested {
		wasRequested[a.ID] = github.User{Login: a.Login, ID: a.ID}
	}
	noted := map[int64]bool{}
	for _, id := range prev.Noted {
		noted[id] = true
	}
	// Retries: what failed is left to look new (or gone) next time; and
	// whom the bridge took out of the attention set is out.
	// The bridge's own turns, handed out for GitHub requests, are seen as
	// they are made: Gerrit names nobody for one, and a person's post in
	// the same second would be taken for its maker (third review).
	notSeen, stillSeen := map[string]bool{}, map[string]bool{}
	notRequested, stillRequested := map[int64]bool{}, map[int64]github.User{}
	// Whether a is trusted; known false: the group could not be read, and
	// what a did waits for the next sync, unseen.
	// Asked once a sync for each (the group is read afresh each time).
	type trust struct{ trusted, known bool }
	trustOf := map[int]trust{}
	trusted := func(a gerrit.AccountInfo) (bool, bool) {
		if c.Trusted == nil {
			return false, true
		}
		t, ok := trustOf[a.AccountID]
		if !ok {
			t.trusted, t.known = c.Trusted(ctx, a.AccountID)
			trustOf[a.AccountID] = t
		}
		return t.trusted, t.known
	}
	holdPatchSet := false
	githubFor := func(a gerrit.AccountInfo) (github.User, bool, error) {
		gh, ok, err := c.Reviewers.GitHubFor(ctx, a)
		if err != nil {
			c.Logger.Warn("finding a Gerrit reviewer's GitHub account", "change", n, "gerrit_user", a.Username, "err", err)
		}
		return gh, ok, err
	}
	ask := func(gh github.User) error {
		if err := c.GitHub.RequestReviewers(ctx, pr, []string{gh.Login}); err != nil {
			c.Logger.Warn("asking someone to review on GitHub", "change", n, "pr", pr, "github_login", gh.Login, "err", err)
			return err
		}
		requested[gh.ID] = gh
		return nil
	}

	// Gerrit → GitHub. Not while a draft: it is their turn once ready.
	for _, key := range attentionNow {
		a := attention[key]
		if wasAttention[key] || sameAccount(a.Account, s.owner) {
			continue
		}
		if !reviewer[key] {
			// A CC's turn is not seen: made a reviewer while it is still
			// theirs, they are asked then (third review).
			notSeen[key] = true
			continue
		}
		if s.pull.Draft {
			notSeen[key] = true
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
		gh, ok, err := githubFor(a.Account)
		if err != nil {
			notSeen[key] = true
			continue
		}
		if _, asked := requested[gh.ID]; !ok || asked || gh.ID == s.pull.User.ID {
			continue
		}
		if err := ask(gh); err != nil && !githubRefusedForGood(err) {
			notSeen[key] = true
		}
	}
	for _, key := range prev.Attention {
		r := removed[key] // none: nobody Gerrit names took them out
		if _, in := attention[key]; in {
			continue
		}
		who, ok, wait := c.actorAt(s, r)
		if wait {
			stillSeen[key] = true
			continue
		}
		if !ok {
			continue
		}
		if !sameAccount(who, r.Account) {
			if t, known := trusted(who); !known {
				stillSeen[key] = true
				continue
			} else if !t {
				continue
			}
		}
		gh, ok, err := githubFor(r.Account)
		if err != nil {
			stillSeen[key] = true
			continue
		}
		u, asked := requested[gh.ID]
		if !ok || !asked {
			continue
		}
		if err := c.GitHub.RemoveRequestedReviewers(ctx, pr, []string{u.Login}); err != nil {
			c.Logger.Warn("withdrawing a review request on GitHub", "change", n, "pr", pr, "github_login", u.Login, "err", err)
			if !githubRefusedForGood(err) {
				stillSeen[key] = true
			}
			continue
		}
		delete(requested, gh.ID)
	}
	if s.current > prev.PatchSet && !s.pull.Draft {
		// Any of the new patch sets a trusted person uploaded (one may be
		// followed by an import before the sync: third review).
		byTrusted := false
		for sha, r := range s.revInfos {
			if r.Number <= prev.PatchSet || !s.exported(sha) {
				continue
			}
			t, known := trusted(uploader(r))
			holdPatchSet = holdPatchSet || !known
			byTrusted = byTrusted || t && known
		}
		if byTrusted {
			for _, rv := range latestReviews(s.allReviews) {
				u := rv.User
				if _, asked := requested[u.ID]; rv.State != "CHANGES_REQUESTED" || asked || u.Type == "Bot" {
					continue
				}
				ask(u) // failing, not asked again: the next patch set may
			}
		}
	}

	// GitHub → Gerrit. Not while a draft (a WIP change in Gerrit, where
	// it is nobody's turn): once ready (third review).
	for _, id := range slices.Sorted(maps.Keys(requested)) {
		u := requested[id]
		_, was := wasRequested[id]
		if was && !noted[id] {
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
		if !reviewer[key] {
			err = c.Gerrit.SetReview(ctx, n, s.revision, gerrit.ReviewInput{Tag: commentTag, Reviewers: []gerrit.ReviewerInput{{Reviewer: user}}})
		} else if !in {
			err = c.Gerrit.AddToAttentionSet(ctx, n, user, reviewerReason+"asked to review on GitHub")
		}
		if err == nil && !in {
			stillSeen[key] = true
		}
		if err != nil && !refused(err) {
			c.Logger.Warn("making someone asked to review on GitHub a Gerrit reviewer", "change", n, "gerrit_user", user, "github_login", u.Login, "err", err)
			notRequested[id] = true
		} else if err != nil {
			c.Logger.Info("Gerrit will not make someone asked to review on GitHub a reviewer", "change", n, "gerrit_user", user, "github_login", u.Login, "err", err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(wasRequested)) {
		if _, still := requested[id]; still {
			continue
		}
		u := wasRequested[id]
		user, _, err := askOnly(ctx, c.Users, u)
		if err != nil {
			stillRequested[id] = u
			continue
		}
		a, in := attention[strings.ToLower(user)]
		if !in {
			continue
		}
		// Their review ends it too (mirrored as them, it usually has).
		if err := c.Gerrit.RemoveFromAttentionSet(ctx, n, a.Account.AccountID, reviewerReason+"review request ended"); err != nil {
			c.Logger.Warn("taking someone no longer asked on GitHub out of the attention set", "change", n, "gerrit_user", user, "err", err)
			if !refused(err) {
				stillRequested[id] = u
			}
			continue
		}
		notSeen[strings.ToLower(user)] = true
	}

	next := &state.Reviewers{At: s.at, PatchSet: s.current}
	if s.pull.Draft || !s.recorded(s.revision) || holdPatchSet {
		// Asked again once ready, or once the bridge has recorded where
		// the patch set came from.
		next.PatchSet = prev.PatchSet
	}
	for _, key := range attentionNow {
		if !notSeen[key] {
			next.Attention = append(next.Attention, key)
		}
	}
	for key := range stillSeen { // none of whose turn it is now
		next.Attention = append(next.Attention, key)
	}
	slices.Sort(next.Attention)
	for id := range notRequested {
		delete(requested, id)
	}
	maps.Copy(requested, stillRequested)
	next.Requested = accounts(requested)
	for _, a := range next.Requested {
		if noted[a.ID] {
			next.Noted = append(next.Noted, a.ID)
		}
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
		case !s.exported(sha):
			wait = true
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

// githubRefusedForGood reports whether GitHub refused err's request for good (a
// 4xx other than a conflict or a rate limit: a 422 for someone who may not
// review, a 403 or 404 for a token that may not): not made again every
// sync, each a GitHub write (third review).
func githubRefusedForGood(err error) bool {
	var ae *github.APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusConflict && ae.Status != http.StatusTooManyRequests && ae.RetryAfter == 0
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

// accounts is requested as the state keeps it, by id.
func accounts(requested map[int64]github.User) []state.GitHubAccount {
	var out []state.GitHubAccount
	for _, id := range slices.Sorted(maps.Keys(requested)) {
		out = append(out, state.GitHubAccount{ID: id, Login: requested[id].Login})
	}
	return out
}
