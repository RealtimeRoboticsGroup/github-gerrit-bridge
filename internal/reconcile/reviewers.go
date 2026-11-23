package reconcile

import (
	"context"
	"fmt"
	"maps"
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
	user, ok, err := r.Users.Lookup(ctx, gh)
	if err != nil || !ok || !strings.EqualFold(user, a.Username) {
		return github.User{}, false, err
	}
	return gh, true, nil
}

// syncReviewers carries reviewers over both ways (design §6.6): a Gerrit
// reviewer (not a CC) is asked to review the PR, and someone asked to
// review the PR is made a Gerrit reviewer (as the bridge: GitHub lets only
// those with write or triage access ask). Each pair is carried over once,
// and kept in pairs: a review on GitHub ends its request, and must not
// bring it back. A reviewer removed in Gerrit is no longer asked on
// GitHub; a request withdrawn on GitHub leaves Gerrit alone, where
// removing a reviewer removes their votes. Reviewers who have voted, the
// change's owner, the PR's author and the bridge are not asked.
//
// Someone asked on GitHub with no Gerrit account is said so in Gerrit,
// once while asked (kept as a pair with no Gerrit user), and made a
// reviewer once they have one.
//
// What fails is logged and tried again by the next sync; it never holds
// up the comments.
func (c *Comments) syncReviewers(ctx context.Context, n, pr int, s *commentSnapshot, pairs []state.Reviewer) []state.Reviewer {
	if c.Reviewers == nil || s.pull == nil || s.pull.State != "open" || s.status != "NEW" {
		return pairs
	}
	asked := map[string]gerrit.AccountInfo{} // Gerrit's reviewers, by lower-case username
	for _, a := range s.reviewers["REVIEWER"] {
		if a.Username != "" {
			asked[strings.ToLower(a.Username)] = a
		}
	}
	requested := map[int64]github.User{}
	for _, u := range s.pull.RequestedReviewers {
		requested[u.ID] = u
	}
	reviewed := map[int64]bool{}
	for _, r := range s.reviews {
		reviewed[r.User.ID] = true
	}
	var out []state.Reviewer
	pairedUser, pairedID := map[string]bool{}, map[int64]bool{}
	keep := func(p state.Reviewer) {
		out = append(out, p)
		pairedUser[strings.ToLower(p.GerritUser)], pairedID[p.GitHubID] = true, true
	}
	noted := map[int64]bool{} // asked on GitHub, with no Gerrit account: said so in Gerrit
	for _, p := range pairs {
		if p.GerritUser == "" {
			noted[p.GitHubID] = true
			continue
		}
		if _, ok := asked[strings.ToLower(p.GerritUser)]; ok {
			keep(p)
			continue
		}
		// No longer a Gerrit reviewer: no longer asked on GitHub either.
		if u, ok := requested[p.GitHubID]; ok {
			if err := c.GitHub.RemoveRequestedReviewers(ctx, pr, []string{u.Login}); err != nil {
				c.Logger.Warn("withdrawing a review request on GitHub", "change", n, "pr", pr, "github_login", u.Login, "err", err)
				keep(p)
				continue
			}
			delete(requested, p.GitHubID)
		}
	}
	voted := func(a gerrit.AccountInfo) bool {
		for _, l := range s.labels {
			for _, v := range l.All {
				if v.Value != 0 && sameAccount(v.AccountInfo, a) {
					return true
				}
			}
		}
		return false
	}
	for _, key := range slices.Sorted(maps.Keys(asked)) {
		a := asked[key]
		if pairedUser[key] || sameAccount(a, s.owner) || (c.BridgeAccount != 0 && a.AccountID == c.BridgeAccount) || voted(a) {
			continue
		}
		gh, ok, err := c.Reviewers.GitHubFor(ctx, a)
		if err != nil {
			c.Logger.Warn("finding a Gerrit reviewer's GitHub account", "change", n, "gerrit_user", a.Username, "err", err)
			continue
		}
		if !ok || gh.ID == s.pull.User.ID {
			continue
		}
		if _, ok := requested[gh.ID]; !ok && !reviewed[gh.ID] {
			if err := c.GitHub.RequestReviewers(ctx, pr, []string{gh.Login}); err != nil {
				c.Logger.Warn("asking a Gerrit reviewer to review on GitHub", "change", n, "pr", pr, "gerrit_user", a.Username, "github_login", gh.Login, "err", err)
				if !isUnprocessable(err) {
					continue
				}
				// GitHub will not ask them (not a collaborator, say):
				// not again every sync.
			}
		}
		keep(state.Reviewer{GerritUser: a.Username, GitHubID: gh.ID})
	}
	for _, id := range slices.Sorted(maps.Keys(requested)) {
		u := requested[id]
		if pairedID[id] {
			continue
		}
		user, ok, err := c.Users.Lookup(ctx, u)
		if err != nil {
			c.Logger.Warn("finding the Gerrit account of someone asked to review on GitHub", "change", n, "github_login", u.Login, "err", err)
			continue
		}
		if !ok {
			// No Gerrit account (yet): said once in Gerrit, while the
			// request stands; added once they have one.
			if u.Type == "Bot" {
				continue
			}
			if !noted[id] {
				msg := fmt.Sprintf("@%s was asked to review on GitHub (%s), but has no Gerrit account the bridge knows, so is not a reviewer here. "+
					"They are added once they have one (by signing in to Gerrit).", u.Login, s.pull.HTMLURL)
				if err := c.Gerrit.SetReview(ctx, n, s.revision, gerrit.ReviewInput{Message: msg, Tag: commentTag, Notify: "OWNER"}); err != nil {
					c.Logger.Warn("saying in Gerrit that someone asked to review on GitHub has no Gerrit account", "change", n, "github_login", u.Login, "err", err)
					continue
				}
			}
			keep(state.Reviewer{GitHubID: id})
			continue
		}
		if _, ok := asked[strings.ToLower(user)]; !ok {
			in := gerrit.ReviewInput{Tag: commentTag, Reviewers: []gerrit.ReviewerInput{{Reviewer: user}}}
			if err := c.Gerrit.SetReview(ctx, n, s.revision, in); err != nil {
				c.Logger.Warn("adding someone asked to review on GitHub as a Gerrit reviewer", "change", n, "gerrit_user", user, "github_login", u.Login, "err", err)
				continue
			}
		}
		keep(state.Reviewer{GerritUser: user, GitHubID: id})
	}
	return out
}
