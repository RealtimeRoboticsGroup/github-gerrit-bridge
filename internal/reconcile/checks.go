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

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
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
	DryRun    bool
	Logger    *slog.Logger

	mu        sync.Mutex
	trusted   map[int]bool
	trustedAt time.Time
}

// DefaultCIMessage matches gerrit-buildkite's messages.
var DefaultCIMessage = regexp.MustCompile(`Build (Started|Succeeded|Failed): (\S+)`)

const (
	statusPrefix      = "gerrit/"
	submittableStatus = "gerrit/submittable"
	votesMarker       = "<!-- bridge:votes -->"
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
	want := desiredStatuses(ch, c.ChangeURL(n), c.CILabels, latestCIRun(ch, rev.Number, c.CIMessage), trusted || !known, c.TrustedGroup)
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

	// Human votes as a PR comment, when they change.
	votes := humanVotes(ch, c.CILabels)
	msg := votesComment(st.Votes, votes, ch.Labels, c.ChangeURL(n))
	if msg == "" || c.DryRun {
		return nil
	}
	if _, err := c.GitHub.CreateIssueComment(ctx, st.State.PR, msg); err != nil {
		return err
	}
	return updateStateFn(ctx, c.Store, n, "Mirror votes", func(s *state.Change) { s.Votes = votes })
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

func latestCIRun(ch *gerrit.ChangeInfo, ps int, re *regexp.Regexp) ciRun {
	var run ciRun
	if re == nil {
		return run
	}
	for _, m := range ch.Messages { // oldest first
		if m.RevisionNumber != ps {
			continue
		}
		if sm := re.FindStringSubmatch(m.Message); len(sm) >= 3 {
			run = ciRun{Phase: sm[1], URL: sm[2]}
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

func who(a gerrit.AccountInfo) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Username != "" {
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
			s.Description = fmt.Sprintf("CI not started: uploader is not in %s. A trusted reviewer can comment retest in Gerrit.", group)
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

// humanVotes lists non-zero votes on labels other than CI labels.
func humanVotes(ch *gerrit.ChangeInfo, ciLabels []string) []state.Vote {
	var out []state.Vote
	for name, l := range ch.Labels {
		if slices.Contains(ciLabels, name) {
			continue
		}
		for _, a := range l.All {
			if a.Value != 0 {
				out = append(out, state.Vote{Account: a.AccountID, Label: name, Value: a.Value})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// votesComment describes what changed between the mirrored and current
// votes, or returns "" if nothing did.
func votesComment(old, cur []state.Vote, labels map[string]gerrit.LabelInfo, changeURL string) string {
	type key struct {
		account int
		label   string
	}
	was := map[key]int{}
	for _, v := range old {
		was[key{v.Account, v.Label}] = v.Value
	}
	names := map[int]string{}
	for _, l := range labels {
		for _, a := range l.All {
			names[a.AccountID] = who(a.AccountInfo)
		}
	}
	name := func(id int) string {
		if n := names[id]; n != "" {
			return n
		}
		return fmt.Sprintf("account %d", id)
	}
	var lines []string
	for _, v := range cur {
		k := key{v.Account, v.Label}
		if w, ok := was[k]; !ok || w != v.Value {
			lines = append(lines, fmt.Sprintf("- **%s**: %s %s", name(v.Account), v.Label, voteString(v.Value)))
		}
		delete(was, k)
	}
	gone := make([]key, 0, len(was))
	for k := range was {
		gone = append(gone, k)
	}
	sort.Slice(gone, func(i, j int) bool {
		return gone[i].label+fmt.Sprint(gone[i].account) < gone[j].label+fmt.Sprint(gone[j].account)
	})
	for _, k := range gone {
		lines = append(lines, fmt.Sprintf("- **%s**: %s vote removed", name(k.account), k.label))
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("Votes in [Gerrit](%s):\n%s\n\n%s", changeURL, strings.Join(lines, "\n"), votesMarker)
}
