package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Submitter submits a Gerrit change as a user.
type Submitter interface {
	SubmitAs(ctx context.Context, user string, change int) error
}

// RunAsSubmitter submits with X-Gerrit-RunAs, so the user's own
// permissions and Gerrit's submit rules apply. The bridge account needs
// the Run As capability.
type RunAsSubmitter struct{ Gerrit *gerrit.Client }

// SubmitAs implements Submitter.
func (r RunAsSubmitter) SubmitAs(ctx context.Context, user string, change int) error {
	_, err := r.Gerrit.As(user).Submit(ctx, change)
	return err
}

// CommandsGitHub is what commands need from GitHub.
type CommandsGitHub interface {
	ListIssueComments(ctx context.Context, n int) ([]github.IssueComment, error)
	CreateIssueComment(ctx context.Context, n int, body string) (*github.IssueComment, error)
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
	DryRun   bool
	Logger   *slog.Logger
}

var (
	submitCommand = regexp.MustCompile(`(?m)^\s*/submit\s*$`)
	handledMarker = regexp.MustCompile(`<!-- bridge:cmd/(\d+) -->`)
)

// Run handles new commands on change n's PR.
func (c *Commands) Run(ctx context.Context, n int) error {
	st, err := c.Store.Load(ctx, n)
	if err != nil || st.State.PR == 0 || st.State.Lifecycle != "NEW" {
		return err
	}
	pr := st.State.PR
	ics, err := c.GitHub.ListIssueComments(ctx, pr)
	if err != nil {
		return err
	}
	handled := map[int64]bool{}
	for _, ic := range ics {
		for _, sm := range handledMarker.FindAllStringSubmatch(ic.Body, -1) {
			id, _ := strconv.ParseInt(sm[1], 10, 64)
			handled[id] = true
		}
	}
	for _, ic := range ics {
		if handled[ic.ID] || isMirror(ic.Body) || !submitCommand.MatchString(ic.Body) {
			continue
		}
		if c.DryRun {
			c.Logger.Info("dry run: would run /submit", "change", n, "pr", pr, "by", ic.User.Login)
			continue
		}
		reply := c.submit(ctx, n, ic)
		if _, err := c.GitHub.CreateIssueComment(ctx, pr, fmt.Sprintf("%s\n\n<!-- bridge:cmd/%d -->", reply, ic.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Commands) submit(ctx context.Context, n int, ic github.IssueComment) string {
	user, ok := c.Identity.GerritUser(ctx, ic.User.Login)
	if !ok {
		return fmt.Sprintf("@%s: `/submit` needs a linked Gerrit account, and yours is not linked. "+
			"Ask an admin to add you to the bridge's identity map, or submit in Gerrit.", ic.User.Login)
	}
	err := c.Submitter.SubmitAs(ctx, user, n)
	if err == nil {
		c.Logger.Info("submitted from GitHub", "change", n, "by", ic.User.Login, "as", user)
		return fmt.Sprintf("@%s: submitted change %d in Gerrit as %s, with any changes below it. "+
			"This PR closes once Gerrit reports the merge.", ic.User.Login, n, user)
	}
	c.Logger.Warn("submit from GitHub failed", "change", n, "by", ic.User.Login, "err", err)
	reason := err.Error()
	var ge *gerrit.Error
	if errors.As(err, &ge) {
		reason = strings.TrimSpace(ge.Body)
	}
	return fmt.Sprintf("@%s: Gerrit did not submit change %d:\n\n> %s", ic.User.Login, n,
		strings.ReplaceAll(reason, "\n", "\n> "))
}
