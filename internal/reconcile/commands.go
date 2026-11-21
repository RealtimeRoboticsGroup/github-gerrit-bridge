package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Submitter submits a Gerrit change as a user.
type Submitter interface {
	// CanSubmitAs reports whether user may submit change now; if not, why.
	// Current is change's current patch set and when it was uploaded.
	Current(ctx context.Context, change int) (rev string, uploaded time.Time, err error)
	CanSubmitAs(ctx context.Context, user string, change int, rev string) (bool, string, error)
	// SubmitAs submits revision rev, which must still be current.
	SubmitAs(ctx context.Context, user string, change int, rev string) error
}

// RunAsSubmitter submits with X-Gerrit-RunAs, so the user's own
// permissions and Gerrit's submit rules apply. The bridge account needs
// the Run As capability.
type RunAsSubmitter struct{ Gerrit *gerrit.Client }

// Current implements Submitter.
func (r RunAsSubmitter) Current(ctx context.Context, change int) (string, time.Time, error) {
	ci, err := r.Gerrit.GetChange(ctx, change, "CURRENT_REVISION")
	if err != nil {
		return "", time.Time{}, err
	}
	rev, ok := ci.Current()
	if !ok {
		return "", time.Time{}, fmt.Errorf("change %d has no current revision", change)
	}
	t, err := time.Parse("2006-01-02 15:04:05.999999999", rev.Created)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("change %d: patch set time %q: %w", change, rev.Created, err)
	}
	return ci.CurrentRevision, t, nil
}

// CanSubmitAs implements Submitter.
func (r RunAsSubmitter) CanSubmitAs(ctx context.Context, user string, change int, rev string) (bool, string, error) {
	actions, err := r.Gerrit.As(user).RevisionActions(ctx, change, rev)
	if err != nil {
		return false, "", err
	}
	a, ok := actions["submit"]
	if !ok {
		return false, user + " may not submit this change, or it is not ready (see its submit requirements in Gerrit).", nil
	}
	if !a.Enabled {
		return false, "Gerrit says: " + a.Title, nil
	}
	return true, "", nil
}

// SubmitAs implements Submitter.
func (r RunAsSubmitter) SubmitAs(ctx context.Context, user string, change int, rev string) error {
	_, err := r.Gerrit.As(user).Submit(ctx, change, rev)
	return err
}

// GitHubMerger merges PRs on GitHub as whoever asked, where GitHub owns
// the branches (design §3.1).
type GitHubMerger interface {
	// Merge merges PR pr as GitHub user u, with their own linked token
	// (GitHub's rules for them decide), if its head is still head. linked
	// is false if u has no token, or no longer the Gerrit account it was
	// linked from.
	Merge(ctx context.Context, u github.User, pr int, head string, m github.MergeInput) (sha string, linked bool, err error)
	// HasSubtree reports whether PR pr's own commits, ending at head,
	// pull in a subtree (git subtree's commits, which name it with
	// git-subtree-dir).
	HasSubtree(ctx context.Context, pr int, head string) (bool, error)
}

// CommandsGitHub is what commands need from GitHub.
type CommandsGitHub interface {
	ListIssueComments(ctx context.Context, n int) ([]github.IssueComment, error)
	Edits(ctx context.Context, n int) (map[int64]string, error)
	CreateIssueComment(ctx context.Context, n int, body string) (*github.IssueComment, error)
	GetPull(ctx context.Context, n int) (*github.PullRequest, error)
	Pushes(ctx context.Context, repo, ref string) ([]github.Push, error)
}

// Commands runs commands given as PR comments (design §6.7):
//
//	/submit   submit this change (and the changes below it) in Gerrit
//
// A command is a line of its own in a PR conversation comment. Each runs
// once: the bridge's reply carries <!-- bridge:cmd/<comment id> -->, so a
// command is recognised as handled without any stored state.
type Commands struct {
	GitHub    CommandsGitHub
	Submitter Submitter
	Store     *state.Store
	// Identity maps GitHub logins to Gerrit usernames.
	Identity Identity
	// BeforeSubmit, if set, runs before the bridge submits a change
	// (Code.PrepareSubmit).
	BeforeSubmit func(ctx context.Context, n int) error
	// Merger, if set, is what /submit does instead of a Gerrit submit:
	// GitHub owns the branches, and the PR is merged there, as whoever
	// asked. LinkURL is where people link their GitHub accounts.
	Merger GitHubMerger
	// GitHubOwned says GitHub owns the branches: /submit is Merger's,
	// and without one it does nothing (never a Gerrit submit, which
	// nobody is granted there).
	GitHubOwned bool
	LinkURL     string
	// BridgeLogin is the bridge's GitHub login: only its replies mark a
	// command handled.
	BridgeLogin string
	// Replies, if set, bounds the commands each GitHub user (by id) has
	// answered: each answer is a comment, written with the bridge's
	// token, and anyone can post /submit over and over on their own PR.
	// Past it, commands are recorded as handled without an answer.
	Replies *Budget
	DryRun  bool
	Logger  *slog.Logger
	// Now is the clock (time.Now if nil).
	Now func() time.Time
}

// maxCommandAge is how old a command may be and still run.
const maxCommandAge = 24 * time.Hour

func (c *Commands) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

var (
	submitCommand = regexp.MustCompile(`(?m)^\s*/submit\s*$`)
	// The bridge's replies end with it; one anywhere else is not the
	// bridge's.
	handledMarker = regexp.MustCompile(`<!-- bridge:cmd/(\d+) -->\s*\z`)
)

// errNeedsBranch says a command is due that must run with its branch
// locked alone (a /submit: PrepareSubmit moves PRs and their bases), not
// shared as comment syncs hold it.
var errNeedsBranch = errors.New("a command needs the branch alone")

// Run handles new commands on change n's PR. The caller holds its branch
// alone.
func (c *Commands) Run(ctx context.Context, n int) error { return c.run(ctx, n, true, nil) }

// run is Run; holding the branch only shared (not alone), it stops with
// errNeedsBranch before running a command. With read, a comment sync's
// read of the PR moments ago, it reads none of it again.
func (c *Commands) run(ctx context.Context, n int, alone bool, read *prRead) error {
	st, err := c.Store.Load(ctx, n)
	if err != nil || st.State.PR == 0 || st.State.Lifecycle != "NEW" {
		return err
	}
	pr := st.State.PR
	if read == nil || read.pull == nil || read.pull.Number != pr {
		if read, err = c.read(ctx, n, pr, st); err != nil {
			return err
		}
	}
	ics, edits, heads := read.ics, read.edits, read.heads
	// Handled: recorded in state, or (from before that was kept, or a
	// crash before it was saved) named by the bridge's own unedited reply.
	// Anyone with write access can edit the bridge's replies: an edited
	// marker could otherwise run a command again, or keep one from
	// running.
	handled := map[int64]bool{}
	for _, id := range st.State.HandledCommands {
		handled[id] = true
	}
	for _, ic := range ics {
		if c.BridgeLogin != "" && !strings.EqualFold(ic.User.Login, c.BridgeLogin) {
			continue
		}
		if editor, ok := edits[ic.ID]; ok && (c.BridgeLogin == "" || !strings.EqualFold(editor, c.BridgeLogin)) {
			continue
		}
		for _, sm := range handledMarker.FindAllStringSubmatch(ic.Body, -1) {
			id, _ := strconv.ParseInt(sm[1], 10, 64)
			handled[id] = true
		}
	}
	var wait time.Duration
	for _, ic := range ics {
		if handled[ic.ID] || isMirror(ic.Body) || !submitCommand.MatchString(ic.Body) {
			continue
		}
		if w := editSettle - c.now().Sub(lastChanged(ic)); w > 0 {
			// Edited by someone else just now, GraphQL may not say yet.
			wait = max(wait, w)
			continue
		}
		if c.DryRun {
			c.Logger.Info("dry run: would run /submit", "change", n, "pr", pr, "by", ic.User.Login)
			continue
		}
		if !alone {
			return errNeedsBranch
		}
		if !c.Replies.Take(ctx, int(ic.User.ID)) {
			c.Logger.Info("/submit not answered: its author posted too many lately", "change", n, "pr", pr, "by", ic.User.Login)
			if err := updateStateFn(ctx, c.Store, n, fmt.Sprintf("Ignored command %d", ic.ID), func(st *state.Change) {
				st.State.HandledCommands = append(st.State.HandledCommands, ic.ID)
			}); err != nil {
				return err
			}
			continue
		}
		reply := ""
		if editedByOther(edits, ic.ID, ic.User.Login) {
			// Anyone with write access can edit anyone's comment.
			reply = fmt.Sprintf("@%s: `/submit` was not run because the comment was edited. Post it as a new comment.", ic.User.Login)
		} else if c.GitHubOwned && c.Merger == nil {
			c.Logger.Warn("/submit: GitHub owns the branches, but the bridge has no way to merge there", "change", n)
			reply = fmt.Sprintf("@%s: `/submit` was not run: the bridge cannot merge on GitHub here (see its log). Use the merge button.", ic.User.Login)
		} else if c.GitHubOwned {
			reply = c.merge(ctx, n, ic, heads, read.pull)
		} else {
			reply = c.submit(ctx, n, ic, heads)
		}
		if err := updateStateFn(ctx, c.Store, n, fmt.Sprintf("Handled command %d", ic.ID), func(st *state.Change) {
			st.State.HandledCommands = append(st.State.HandledCommands, ic.ID)
		}); err != nil {
			return err
		}
		if _, err := c.GitHub.CreateIssueComment(ctx, pr, fmt.Sprintf("%s\n\n<!-- bridge:cmd/%d -->", reply, ic.ID)); err != nil {
			return err
		}
	}
	return waitFor(wait)
}

// read reads change n's PR pr as a comment sync does (prRead), noting
// its head.
func (c *Commands) read(ctx context.Context, n, pr int, st *state.Change) (*prRead, error) {
	ctx = github.WithReads(ctx)
	ics, err := c.GitHub.ListIssueComments(ctx, pr)
	if err != nil {
		return nil, err
	}
	edits, err := c.GitHub.Edits(ctx, pr)
	if err != nil {
		return nil, err
	}
	// What GitHub has now, read after the comments (state.Head).
	p, err := c.GitHub.GetPull(ctx, pr)
	if err != nil {
		return nil, err
	}
	at := c.now()
	if !slices.Equal(withHead(st.Heads, p.Head.SHA, at), st.Heads) && !c.DryRun {
		if err := noteHead(ctx, c.Store, n, p.Head.SHA, at); err != nil {
			return nil, err
		}
	}
	heads, err := withPushes(ctx, c.GitHub, p, st.Heads)
	if err != nil {
		return nil, err
	}
	return &prRead{ics: ics, edits: edits, pull: p, heads: withHead(heads, p.Head.SHA, at), at: at}, nil
}

// lastChanged is when ic was posted or last updated.
func lastChanged(ic github.IssueComment) time.Time { return latest(ic.CreatedAt, ic.UpdatedAt) }

// latest is the later of a and b.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// seen is the current patch set of change n (rev) and the PR head that is
// it (sha), if the /submit in ic is for it: what its author could have
// seen. If not, reply says why.
func (c *Commands) seen(ctx context.Context, n int, ic github.IssueComment, heads []state.Head) (rev, sha string, st *state.Change, reply string) {
	// A command is for the patch set its author saw: not for one uploaded
	// after it, nor, long after, for whatever the change has become (the
	// bridge was down, say).
	rev, uploaded, err := c.Submitter.Current(ctx, n)
	if err != nil {
		c.Logger.Warn("finding the patch set to submit", "change", n, "err", err)
		return "", "", nil, fmt.Sprintf("@%s: `/submit` was not run: the bridge could not reach Gerrit (see its log). Post it again.", ic.User.Login)
	}
	if ic.CreatedAt.Before(uploaded) {
		return "", "", nil, fmt.Sprintf("@%s: `/submit` was not run: change %d has a patch set newer than your comment. Post it again to submit that.", ic.User.Login, n)
	}
	if age := c.now().Sub(ic.CreatedAt); age > maxCommandAge {
		return "", "", nil, fmt.Sprintf("@%s: `/submit` was not run: it is more than %v old. Post it again.", ic.User.Login, maxCommandAge)
	}
	// And one the commenter could have seen: the PR's head from a minute
	// (minSeen) before they wrote it until now (state.Head). A patch set
	// uploaded in Gerrit just before the comment may not be on GitHub
	// yet, one pushed moments before it was not read (fourth audit), and
	// the branch may have gone back to an older patch set's commit, or on
	// to one never imported (seventh audit).
	st, err = c.Store.Load(ctx, n)
	if err != nil {
		c.Logger.Warn("reading state for /submit", "change", n, "err", err)
		return "", "", nil, fmt.Sprintf("@%s: `/submit` was not run: the bridge could not read its state (see its log). Post it again.", ic.User.Login)
	}
	sha = headThroughout(heads, ic.CreatedAt.Add(-minSeen), c.now())
	seen := sha != "" && (sha == rev || slices.ContainsFunc(st.Revisions, func(r state.Revision) bool { return r.GitHubSHA == sha && r.GerritSHA == rev }))
	if !seen {
		return "", "", nil, fmt.Sprintf("@%s: `/submit` was not run: change %d's current patch set was not on GitHub when you wrote it (not yet, only just, or not at all: see the change). Submit it in Gerrit, or post again once it is here.", ic.User.Login, n)
	}
	return rev, sha, st, ""
}

func (c *Commands) submit(ctx context.Context, n int, ic github.IssueComment, heads []state.Head) string {
	user, ok := c.Identity.GerritUserFor(ctx, ic.User)
	if !ok {
		return fmt.Sprintf("@%s: `/submit` needs a linked Gerrit account, and yours is not linked. "+
			"Ask an admin to add you to the bridge's identity map, or submit in Gerrit.", ic.User.Login)
	}
	rev, _, _, reply := c.seen(ctx, n, ic, heads)
	if reply != "" {
		return reply
	}
	// Check first, so PRs are only reshaped for a submit that will happen.
	if ok, why, err := c.Submitter.CanSubmitAs(ctx, user, n, rev); err != nil || !ok {
		if err != nil {
			c.Logger.Warn("checking submit from GitHub", "change", n, "by", ic.User.Login, "err", err)
			why = "The bridge could not ask Gerrit (see its log)."
		}
		return fmt.Sprintf("@%s: Gerrit would not submit change %d:\n\n> %s", ic.User.Login, n, strings.ReplaceAll(forGitHub(why), "\n", "\n> "))
	}
	if c.BeforeSubmit != nil {
		if err := c.BeforeSubmit(ctx, n); err != nil {
			// Not fatal: the PRs then close with a notice instead of
			// showing merged.
			c.Logger.Warn("preparing PRs for submit", "change", n, "err", err)
		}
	}
	err := c.Submitter.SubmitAs(ctx, user, n, rev)
	if err == nil {
		c.Logger.Info("submitted from GitHub", "change", n, "by", ic.User.Login, "as", user)
		return fmt.Sprintf("@%s: submitted change %d in Gerrit as %s, with any changes below it. "+
			"This PR closes once Gerrit reports the merge.", ic.User.Login, n, user)
	}
	c.Logger.Warn("submit from GitHub failed", "change", n, "by", ic.User.Login, "err", err)
	// Gerrit's own refusals are for the user; anything else stays in the
	// log, which may name internal hosts.
	reason := "The bridge could not reach Gerrit (see its log)."
	var ge *gerrit.Error
	if errors.As(err, &ge) && ge.Status < 500 {
		reason = strings.TrimSpace(ge.Body)
	}
	return fmt.Sprintf("@%s: Gerrit did not submit change %d:\n\n> %s", ic.User.Login, n,
		strings.ReplaceAll(forGitHub(reason), "\n", "\n> "))
}

// maxListedCommits is how many of a PR's commits GitHub lists.
const maxListedCommits = 250

// merge is /submit where GitHub owns the branches (design §3.1): the PR
// is merged on GitHub with the commenter's own linked token, so GitHub's
// branch protection decides as it would for their button press, and only
// at the head they could have seen, the change's current patch set. Its
// commit message is the PR's description, ending with the change's
// Change-Id, so Gerrit closes the change as merged when the merge reaches
// its branch. A GitHub-born PR that pulls in a subtree (git subtree's
// commits, whose history later subtree pulls need) is merged with a merge
// commit; everything else is squashed, whatever merges it has (an
// "Update branch", the bridge's own patch-set history): a merge commit
// would bring every commit of it, Change-Id footers that close other
// changes included. Gerrit-born PRs' history is the patch sets'.
func (c *Commands) merge(ctx context.Context, n int, ic github.IssueComment, heads []state.Head, pr *github.PullRequest) string {
	_, sha, st, reply := c.seen(ctx, n, ic, heads)
	if reply != "" {
		return reply
	}
	if pr == nil || !changeIDPattern.MatchString(st.State.ChangeID) {
		c.Logger.Warn("/submit: PR or Change-Id unknown", "change", n, "pr", st.State.PR)
		return fmt.Sprintf("@%s: `/submit` was not run: the bridge could not tell which PR or change this is (see its log). Use the merge button.", ic.User.Login)
	}
	if pr.Base.Ref != st.State.Branch || (pr.Stack != nil && pr.Stack.Position != 1) {
		// A stacked PR would merge into the one below it, not the
		// branch, or (merged as a stack) take the PRs below it along:
		// only a stack's bottom, on the branch, merges alone.
		return fmt.Sprintf("@%s: `/submit` was not run: this PR is based on `%s`, not `%s`, or is not the bottom of its stack. Merge the PRs below it first.",
			ic.User.Login, forGitHub(pr.Base.Ref), st.State.Branch)
	}
	method := "squash"
	if st.State.Origin == "github" {
		// GitHub lists at most 250 of a PR's commits: one with more is
		// never vetted whole, and squashed (thirteenth review).
		has, err := false, error(nil)
		if pr.Commits < maxListedCommits {
			has, err = c.Merger.HasSubtree(ctx, pr.Number, sha)
		}
		if err != nil {
			c.Logger.Warn("/submit: listing the PR's commits", "change", n, "pr", pr.Number, "err", err)
			return fmt.Sprintf("@%s: `/submit` was not run: the bridge could not read this PR's commits (see its log). Post it again, or use the merge button.", ic.User.Login)
		}
		if has {
			method = "merge"
		}
	}
	msg := prBodyForMessage(pr.Body)
	if msg != "" {
		msg += "\n\n"
	}
	// The description's sign-offs stay, as the merge button keeps them,
	// in the trailer paragraph.
	user, _ := splitBody(pr.Body)
	trailers := append(signOffs(user), "Change-Id: "+st.State.ChangeID)
	// A PR in a native stack (the bridge's own, of a relation chain, or
	// one made on GitHub) merges only asynchronously; being based on the
	// branch, it is the bottom, and merges alone.
	in := github.MergeInput{Method: method, SHA: sha,
		Title:   fmt.Sprintf("%s (#%d)", strings.TrimSpace(pr.Title), pr.Number),
		Message: msg + strings.Join(trailers, "\n"), Stacked: pr.Stack != nil, Base: st.State.Branch}
	merged, linked, err := c.Merger.Merge(ctx, ic.User, pr.Number, sha, in)
	switch {
	case !linked && err == nil:
		link := "link it"
		if c.LinkURL != "" {
			link = "link it at " + c.LinkURL
		}
		return fmt.Sprintf("@%s: `/submit` merges on GitHub as you, with your linked GitHub account, and yours is not linked: %s, or use the merge button.", ic.User.Login, link)
	case err == nil:
		c.Logger.Info("merged on GitHub for /submit", "change", n, "pr", pr.Number, "by", ic.User.Login, "method", method, "commit", merged)
		return fmt.Sprintf("@%s: merged on GitHub as you (%s), as %s. Gerrit shows change %d merged once it has the commit.", ic.User.Login, map[string]string{"squash": "squashed", "merge": "with a merge commit"}[method], merged, n)
	}
	c.Logger.Warn("merge on GitHub for /submit failed", "change", n, "pr", pr.Number, "by", ic.User.Login, "err", err)
	// GitHub's own refusals are for the user (not mergeable, a newer
	// head, their permissions); anything else stays in the log.
	why := "The bridge could not reach GitHub (see its log)."
	var ae *github.APIError
	if errors.As(err, &ae) && ae.Status < 500 && ae.Message != "" {
		why = ae.Message
	}
	return fmt.Sprintf("@%s: GitHub did not merge this PR:\n\n> %s", ic.User.Login, strings.ReplaceAll(forGitHub(why), "\n", "\n> "))
}
