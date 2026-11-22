package gerritchecks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// GitHubSource is where GitHub's checks of a change's patch sets come
// from, where GitHub owns the branches (design §3.1): GitHub Actions and
// the rest are the CI, and its merge box the gate.
type GitHubSource interface {
	// Commit is the GitHub commit that is change's patch set sha (a
	// Gerrit commit), "" if GitHub has none; and the change's PR, 0 if
	// none.
	Commit(ctx context.Context, change int, sha string) (commit string, pr int, err error)
	CheckRuns(ctx context.Context, commit string) ([]github.CheckRun, error)
	Statuses(ctx context.Context, commit string) ([]github.Status, error)
	GetPull(ctx context.Context, n int) (*github.PullRequest, error)
}

// maxMessage bounds a check's text shown in Gerrit.
const maxMessage = 4000

// GitHubRuns turns a commit's check runs and statuses into check runs:
// each attempt of a check, oldest first, as its attempts; and, if pr is
// the open PR whose head commit is, whether GitHub would merge it now.
// The bridge's own gerrit/* statuses (which it posts only where Gerrit
// owns the branches) are left out.
func GitHubRuns(checks []github.CheckRun, statuses []github.Status, pr *github.PullRequest, commit string, change, ps int) []Run {
	runs := []Run{}
	checks = append([]github.CheckRun(nil), checks...)
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Name != checks[j].Name {
			return checks[i].Name < checks[j].Name
		}
		return startOf(checks[i]).Before(startOf(checks[j])) || (startOf(checks[i]).Equal(startOf(checks[j])) && checks[i].ID < checks[j].ID)
	})
	attempt := map[string]int{}
	for _, c := range checks {
		attempt[c.Name]++
		link := webURL(c.HTMLURL)
		if link == "" {
			link = webURL(c.DetailsURL)
		}
		r := Run{Change: change, Patchset: ps, Attempt: attempt[c.Name], ExternalID: "github-check-" + itoa(c.ID),
			CheckName: c.Name, CheckDescription: c.App.Name, CheckLink: link, StatusLink: link,
			StartedAt: c.StartedAt, FinishedAt: c.CompletedAt}
		switch c.Status {
		case "completed":
			r.Status = "COMPLETED"
			res := Result{ExternalID: r.ExternalID, Category: "INFO", Summary: c.Conclusion}
			switch c.Conclusion {
			case "success":
				res.Category, res.Summary = "SUCCESS", "Passed"
			case "failure":
				res.Category, res.Summary = "ERROR", "Failed"
			case "timed_out":
				res.Category, res.Summary = "ERROR", "Timed out"
			case "cancelled":
				res.Category, res.Summary = "WARNING", "Canceled"
			case "action_required":
				res.Category, res.Summary = "WARNING", "Action required"
			case "neutral":
				res.Summary = "Neutral"
			case "skipped":
				res.Summary = "Skipped"
			case "stale":
				res.Summary = "Stale"
			}
			if t := strings.TrimSpace(c.Output.Title); t != "" {
				res.Summary += ": " + oneLine(t)
			}
			res.Message = clip(strings.TrimSpace(c.Output.Summary))
			if link != "" {
				res.Links = []Link{{URL: link, Primary: true, Icon: "external", Tooltip: "Open on GitHub"}}
			}
			r.Results = []Result{res}
		case "in_progress":
			r.Status = "RUNNING"
		default: // queued, waiting, requested, pending
			r.Status = "SCHEDULED"
		}
		runs = append(runs, r)
	}
	for _, s := range statuses {
		if strings.HasPrefix(s.Context, "gerrit/") {
			continue
		}
		target := webURL(s.TargetURL)
		r := Run{Change: change, Patchset: ps, Attempt: 1, ExternalID: "github-status-" + s.Context, CheckName: s.Context,
			CheckLink: target, StatusLink: target, StatusDescription: oneLine(s.Description)}
		res := Result{ExternalID: r.ExternalID, Summary: oneLine(s.Description)}
		switch s.State {
		case "success":
			r.Status, res.Category = "COMPLETED", "SUCCESS"
		case "failure", "error":
			r.Status, res.Category = "COMPLETED", "ERROR"
		default: // pending
			r.Status = "RUNNING"
		}
		if res.Summary == "" {
			res.Summary = s.State
		}
		if r.Status == "COMPLETED" {
			if target != "" {
				res.Links = []Link{{URL: target, Primary: true, Icon: "external", Tooltip: "Open"}}
			}
			r.Results = []Result{res}
		}
		runs = append(runs, r)
	}
	if pr != nil && pr.Head.SHA == commit {
		runs = append(runs, mergeRun(pr, change, ps))
	}
	return runs
}

// mergeRun says whether GitHub would merge pr now, as its merge box does.
func mergeRun(pr *github.PullRequest, change, ps int) Run {
	link := webURL(pr.HTMLURL)
	r := Run{Change: change, Patchset: ps, Attempt: 1, ExternalID: "github-merge", CheckName: "GitHub merge",
		CheckDescription: "Whether GitHub would merge this PR now (its branch protection: reviews, required checks)",
		CheckLink:        link, StatusLink: link, Status: "COMPLETED"}
	res := Result{ExternalID: r.ExternalID, Category: "INFO"}
	switch {
	case pr.Merged:
		res.Category, res.Summary = "SUCCESS", "Merged"
	case pr.State != "open":
		res.Summary = "Closed"
	default:
		switch pr.MergeableState {
		case "clean", "has_hooks":
			res.Category, res.Summary = "SUCCESS", "Ready to merge"
		case "unstable":
			res.Category, res.Summary = "SUCCESS", "Ready to merge (some checks that are not required failed)"
		case "blocked":
			res.Category, res.Summary = "WARNING", "Blocked: it needs reviews or required checks (see the PR)"
		case "behind":
			res.Category, res.Summary = "WARNING", "Behind its base branch: update it on GitHub"
		case "dirty":
			res.Category, res.Summary = "ERROR", "Merge conflicts: rebase it"
		case "draft":
			res.Summary = "Draft: mark it ready for review"
		default: // unknown: GitHub is still working it out
			r.Status, r.StatusDescription = "RUNNING", "GitHub is still working out whether it can merge"
		}
	}
	if r.Status == "COMPLETED" {
		if link != "" {
			res.Links = []Link{{URL: link, Primary: true, Icon: "external", Tooltip: "Open the PR on GitHub"}}
		}
		r.Results = []Result{res}
	}
	return r
}

// webURL is u if it is an http or https URL, else "": links are set by
// whoever creates a check run or status (any workflow), and Gerrit's UI
// makes them links on its own origin, where a javascript: URL would run
// as whoever clicks it.
func webURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") {
		return ""
	}
	return u
}

// notOnGitHub is the run for a patch set GitHub does not have.
func notOnGitHub(change, ps int) Run {
	return Run{Change: change, Patchset: ps, Attempt: 1, ExternalID: "github-none", CheckName: "GitHub", Status: "COMPLETED",
		Results: []Result{{ExternalID: "github-none", Category: "INFO",
			Summary: "This patch set is not on GitHub, so GitHub checks nothing of it (the change's messages say why)"}}}
}

func startOf(c github.CheckRun) time.Time {
	if c.StartedAt != nil {
		return *c.StartedAt
	}
	return time.Time{}
}

var spaces = regexp.MustCompile(`\s+`)

// oneLine is s on one line, bounded.
func oneLine(s string) string { return clip(strings.TrimSpace(spaces.ReplaceAllString(s, " "))) }

// clip bounds s to maxMessage bytes, on a rune boundary.
func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// githubEntry is the answer for change's patch set ps, Gerrit commit sha,
// from GitHub.
func (h *Handler) githubEntry(ctx context.Context, change, ps int, sha string) entry {
	fail := failure
	commit, prNum, err := h.GitHub.Commit(ctx, change, sha)
	if err != nil {
		h.Logger.Warn("checks: finding the patch set's GitHub commit", "change", change, "patchset", ps, "err", err)
		return fail(http.StatusBadGateway, "cannot tell which GitHub commit this patch set is")
	}
	if commit == "" {
		b, _ := json.Marshal(Response{Runs: []Run{notOnGitHub(change, ps)}})
		return entry{at: time.Now(), status: http.StatusOK, body: b}
	}
	// What Live keeps current from deliveries is not read again. The PR
	// always is: GitHub sends nothing when whether it can merge changes
	// (its base moving, say).
	have, begin := h.Live.get(commit)
	reads := 0
	for _, need := range []bool{!have.haveRuns, !have.haveStatuses, prNum != 0} {
		if need {
			reads++
		}
	}
	for range reads {
		if !h.spend() {
			return fail(http.StatusServiceUnavailable, "busy")
		}
	}
	// What is read is read at once, not one after another: each is a
	// round trip to GitHub, and the Checks tab waits for all of them.
	var (
		wg                          sync.WaitGroup
		checks                      []github.CheckRun
		statuses                    []github.Status
		pr                          *github.PullRequest
		checksErr, statusErr, prErr error
	)
	if !have.haveRuns {
		h.runReads.Add(1)
		wg.Go(func() { checks, checksErr = h.GitHub.CheckRuns(ctx, commit) })
	}
	if !have.haveStatuses {
		wg.Go(func() { statuses, statusErr = h.GitHub.Statuses(ctx, commit) })
	}
	if prNum != 0 {
		wg.Go(func() { pr, prErr = h.GitHub.GetPull(ctx, prNum) })
	}
	wg.Wait()
	if checksErr != nil {
		h.Logger.Warn("checks: listing GitHub check runs", "change", change, "patchset", ps, "err", checksErr)
		return fail(http.StatusBadGateway, "cannot read checks from GitHub")
	}
	if statusErr != nil {
		h.Logger.Warn("checks: reading GitHub statuses", "change", change, "patchset", ps, "err", statusErr)
		return fail(http.StatusBadGateway, "cannot read statuses from GitHub")
	}
	if prErr != nil {
		h.Logger.Warn("checks: reading the PR", "change", change, "pr", prNum, "err", prErr)
		return fail(http.StatusBadGateway, "cannot read the PR from GitHub")
	}
	view := have
	if !have.haveRuns || !have.haveStatuses {
		view = h.Live.merge(commit, begin, checks, !have.haveRuns, statuses, !have.haveStatuses)
	}
	return h.answer(time.Now(), change, ps, commit, view, pr)
}

// answer is the answer for change's patch set ps, GitHub commit, from
// view and pr, as of at.
func (h *Handler) answer(at time.Time, change, ps int, commit string, view liveView, pr *github.PullRequest) entry {
	resp := Response{}
	if pr != nil {
		if link := webURL(pr.HTMLURL); link != "" {
			resp.Links = []Link{{URL: link, Tooltip: "The PR on GitHub", Icon: "external"}}
		}
	}
	resp.Runs = GitHubRuns(view.runs, view.statuses, pr, commit, change, ps)
	b, _ := json.Marshal(resp)
	e := entry{at: at, status: http.StatusOK, body: b}
	if h.Live != nil {
		e.commit, e.liveSeq, e.change, e.ps, e.pr = commit, view.seq, change, ps, pr
	}
	return e
}

// rebuilt is e, a cached answer that deliveries have changed since,
// built again from what Live has and e's PR, reading nothing (each
// delivery is not another read of GitHub: cacheTTL bounds those, and e
// keeps its time). What Live has is what e was built of and the
// deliveries since; unless Live dropped the commit meanwhile, then it
// is e itself.
func (h *Handler) rebuilt(e entry) entry {
	v, _ := h.Live.get(e.commit)
	if v.born > e.liveSeq {
		return e
	}
	return h.answer(e.at, e.change, e.ps, e.commit, v, e.pr)
}
