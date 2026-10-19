package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerritchecks"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// ChecksGerrit is what vote and CI mirroring needs from Gerrit.
type ChecksGerrit interface {
	GetChange(ctx context.Context, change int, opts ...string) (*gerrit.ChangeInfo, error)
	GroupMembers(ctx context.Context, group string) ([]gerrit.AccountInfo, error)
	ChangeURL(project string, number int) string
}

// ChecksGitHub is what vote and CI mirroring needs from GitHub.
type ChecksGitHub interface {
	Statuses(ctx context.Context, sha string) ([]github.Status, error)
	CreateStatus(ctx context.Context, sha string, s github.Status) error
	CreateIssueComment(ctx context.Context, n int, body string) (*github.IssueComment, error)
}

// Checks mirrors Gerrit votes, CI and submittability onto the PR head as
// commit statuses (design §6.4, §6.5, §6.7), and human votes as a short PR
// comment.
//
// It uses commit statuses rather than check runs because check runs need
// a GitHub App, and the bridge runs on a personal token for now. For the
// same reason GitHub approvals are not created: GitHub does not let the
// token's user approve its own PRs.
type Checks struct {
	Project string
	Gerrit  ChecksGerrit
	GitHub  ChecksGitHub
	Store   *state.Store
	// CILabels are labels voted by CI (shown only as statuses).
	CILabels []string
	// TrustedGroup is the group whose uploads CI builds (gerrit-buildkite's
	// "Verified Users"). Empty disables the explanation.
	TrustedGroup string
	// CIMessage matches a CI message; group 1 is Started, Succeeded or
	// Failed and group 2 the build URL.
	CIMessage *regexp.Regexp
	// CIAccount is CI's Gerrit username: only its messages are CI runs.
	CIAccount string
	// Builds, if set, is read for CI runs instead of CI's messages.
	Builds gerritchecks.Builds
	DryRun bool
	Logger *slog.Logger

	mu        sync.Mutex
	trusted   map[int]bool
	trustedAt time.Time
}

// DefaultCIMessage matches gerrit-buildkite's messages.
var DefaultCIMessage = regexp.MustCompile(`Build (Started|Succeeded|Failed): (\S+)`)

const (
	statusPrefix      = "gerrit/"
	submittableStatus = "gerrit/submittable"
)

// ciRun is the latest CI message on the current patch set.
type ciRun struct {
	Phase string // Started, Succeeded, Failed, or "" for none
	URL   string
}

// Sync mirrors change n's votes, CI and submittability.
func (c *Checks) Sync(ctx context.Context, n int) error {
	st, err := c.Store.Load(ctx, n)
	if err != nil {
		return err
	}
	if st.State.PR == 0 {
		return nil
	}
	ch, err := c.Gerrit.GetChange(ctx, n, "DETAILED_LABELS", "DETAILED_ACCOUNTS", "CURRENT_REVISION",
		"SUBMITTABLE", "SUBMIT_REQUIREMENTS", "MESSAGES")
	if err != nil {
		return err
	}
	if ch.Status != "NEW" || ch.IsPrivate {
		return nil
	}
	rev, ok := ch.Current()
	if !ok {
		return fmt.Errorf("change %d: no current revision", n)
	}
	// Statuses go on the GitHub commit for the current patch set, which
	// exists only once the code mirror has exported (or imported) it; the
	// code mirror calls Sync again when it has.
	sha := ""
	for _, r := range st.Revisions {
		if r.GerritSHA == ch.CurrentRevision && r.GitHubSHA != "" {
			sha = r.GitHubSHA
		}
	}
	if sha == "" {
		c.Logger.Debug("current patch set not on GitHub yet; statuses wait", "change", n)
		return nil
	}

	trusted, known := c.isTrusted(ctx, rev.Uploader.AccountID)
	run := latestCIRun(ch, rev.Number, c.CIMessage, c.CIAccount)
	if c.Builds != nil {
		builds, err := c.Builds.Builds(ctx, ch.CurrentRevision)
		if err != nil {
			return err
		}
		run = latestBuild(builds)
	}
	want := desiredStatuses(ch, c.ChangeURL(n), c.CILabels, run, trusted || !known, c.TrustedGroup)
	have, err := c.GitHub.Statuses(ctx, sha)
	if err != nil {
		return err
	}
	cur := map[string]github.Status{}
	for _, s := range have {
		cur[s.Context] = s
	}
	for _, s := range want {
		if h, ok := cur[s.Context]; ok && h.State == s.State && h.Description == s.Description && h.TargetURL == s.TargetURL {
			continue
		}
		if c.DryRun {
			c.Logger.Info("dry run: would set status", "change", n, "sha", short(sha), "context", s.Context, "state", s.State, "description", s.Description)
			continue
		}
		if err := c.GitHub.CreateStatus(ctx, sha, s); err != nil {
			return err
		}
		c.Logger.Info("set status", "change", n, "pr", st.State.PR, "context", s.Context, "state", s.State, "description", s.Description)
	}
	// Votes show in gerrit/<label> statuses, and on the mirrored Gerrit
	// message they came with (Comments): no separate votes comment.
	return nil
}

// ChangeURL is the Gerrit web URL of change n.
func (c *Checks) ChangeURL(n int) string { return c.Gerrit.ChangeURL(c.Project, n) }

// isTrusted reports whether account is in the trusted group. known is
// false if the group could not be read (then nothing is claimed).
func (c *Checks) isTrusted(ctx context.Context, account int) (trusted, known bool) {
	if c.TrustedGroup == "" {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trusted == nil || time.Since(c.trustedAt) > 5*time.Minute {
		members, err := c.Gerrit.GroupMembers(ctx, c.TrustedGroup)
		if err != nil {
			c.Logger.Warn("cannot read trusted group; not explaining CI", "group", c.TrustedGroup, "err", err)
			return false, false
		}
		c.trusted = map[int]bool{}
		for _, m := range members {
			c.trusted[m.AccountID] = true
		}
		c.trustedAt = time.Now()
	}
	return c.trusted[account], true
}

func latestCIRun(ch *gerrit.ChangeInfo, ps int, re *regexp.Regexp, account string) ciRun {
	var run ciRun
	if re == nil {
		return run
	}
	for _, m := range ch.Messages { // oldest first
		if m.RevisionNumber != ps || isBridgeTag(m.Tag) || (account != "" && !strings.EqualFold(m.Author.Username, account)) {
			continue // not from CI: mirrored from GitHub, or someone else
		}
		if sm := re.FindStringSubmatch(m.Message); len(sm) >= 3 {
			run = ciRun{Phase: sm[1], URL: sm[2]}
		}
	}
	return run
}

// latestBuild is the newest build as a CI run.
func latestBuild(builds []buildkite.Build) ciRun {
	var run ciRun
	newest := 0
	for _, b := range builds {
		if b.Number < newest {
			continue
		}
		newest = b.Number
		run = ciRun{Phase: "Started", URL: b.WebURL}
		switch {
		case b.State == "passed":
			run.Phase = "Succeeded"
		case buildkite.Finished(b.State):
			run.Phase = "Failed"
		}
	}
	return run
}

func voteString(v int) string {
	if v > 0 {
		return fmt.Sprintf("+%d", v)
	}
	return fmt.Sprint(v)
}

// who names a Gerrit account: its full name, and the username too, since
// anyone can set their full name to someone else's.
func who(a gerrit.AccountInfo) string {
	switch {
	case a.Name != "" && a.Username != "" && !strings.EqualFold(a.Name, a.Username):
		return a.Name + " (" + a.Username + ")"
	case a.Name != "":
		return a.Name
	case a.Username != "":
		return a.Username
	}
	return fmt.Sprintf("account %d", a.AccountID)
}

func truncateDesc(s string) string {
	if len(s) <= 140 {
		return s
	}
	return s[:137] + "..."
}

// desiredStatuses computes the statuses for a change's current revision.
func desiredStatuses(ch *gerrit.ChangeInfo, changeURL string, ciLabels []string, run ciRun, uploaderTrusted bool, group string) []github.Status {
	var out []github.Status
	names := make([]string, 0, len(ch.Labels))
	for name := range ch.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		l := ch.Labels[name]
		lo, hi := l.Range()
		s := github.Status{Context: statusPrefix + name, TargetURL: changeURL, State: "pending"}
		var max, min, others []string
		for _, a := range l.All {
			if a.Value == 0 {
				continue
			}
			e := voteString(a.Value) + " " + who(a.AccountInfo)
			switch {
			case a.Value == hi && hi > 0:
				max = append(max, e)
			case a.Value == lo && lo < 0:
				min = append(min, e)
			default:
				others = append(others, e)
			}
		}
		isCI := slices.Contains(ciLabels, name)
		switch {
		case len(min) > 0:
			s.State, s.Description = "failure", strings.Join(append(min, others...), ", ")
		case len(max) > 0:
			s.State, s.Description = "success", strings.Join(append(max, others...), ", ")
		case len(others) > 0:
			s.Description = strings.Join(others, ", ")
		case isCI && run.Phase == "Started":
			s.Description = "CI running"
		case isCI && run.Phase != "":
			s.Description = "CI " + strings.ToLower(run.Phase) + "; waiting for the vote"
		case isCI && !uploaderTrusted:
			s.Description = fmt.Sprintf("CI not started: uploader is not in %s. A trusted reviewer can comment retest (here or in Gerrit).", group)
		case isCI:
			s.Description = "Waiting for CI"
		default:
			s.Description = "No votes yet"
		}
		if isCI && run.URL != "" {
			s.TargetURL = run.URL
		}
		s.Description = truncateDesc(s.Description)
		out = append(out, s)
	}
	sub := github.Status{Context: submittableStatus, TargetURL: changeURL, State: "pending"}
	if ch.Submittable {
		sub.State, sub.Description = "success", "Submittable in Gerrit"
	} else {
		var missing []string
		for _, r := range ch.SubmitRequirements {
			if r.Status == "UNSATISFIED" || r.Status == "ERROR" {
				missing = append(missing, r.Name)
			}
		}
		sub.Description = "Not submittable yet"
		if len(missing) > 0 {
			sub.Description = "Needs " + strings.Join(missing, ", ")
		}
	}
	sub.Description = truncateDesc(sub.Description)
	return append(out, sub)
}
