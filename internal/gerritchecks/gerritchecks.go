// Package gerritchecks serves CI builds to Gerrit's Checks tab.
//
// Gerrit's web UI asks a ChecksProvider plugin (plugin/bridge-checks.js)
// for a patch set's check runs. The plugin fetches them from this
// handler, which reads them from Buildkite: a browser cannot hold a
// Buildkite token, and the runs then come from CI itself rather than from
// parsing CI's messages.
package gerritchecks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
)

// Builds lists CI builds of a commit.
type Builds interface {
	Builds(ctx context.Context, commit string) ([]buildkite.Build, error)
}

// Changes reads changes as an anonymous user.
type Changes interface {
	GetChange(ctx context.Context, change int, opts ...string) (*gerrit.ChangeInfo, error)
}

// Link is a Gerrit checks Link.
type Link struct {
	URL     string `json:"url"`
	Tooltip string `json:"tooltip,omitempty"`
	Primary bool   `json:"primary"`
	Icon    string `json:"icon"`
}

// Result is a Gerrit checks CheckResult.
type Result struct {
	ExternalID string `json:"externalId"`
	Category   string `json:"category"` // SUCCESS, INFO, WARNING, ERROR
	Summary    string `json:"summary"`
	Links      []Link `json:"links,omitempty"`
}

// Run is a Gerrit checks CheckRun. Timestamps are RFC 3339; the plugin
// turns them into Dates.
type Run struct {
	Change            int        `json:"change"`
	Patchset          int        `json:"patchset"`
	Attempt           int        `json:"attempt"`
	ExternalID        string     `json:"externalId"`
	CheckName         string     `json:"checkName"`
	CheckDescription  string     `json:"checkDescription,omitempty"`
	CheckLink         string     `json:"checkLink,omitempty"`
	Status            string     `json:"status"` // SCHEDULED, RUNNING, COMPLETED
	StatusDescription string     `json:"statusDescription,omitempty"`
	StatusLink        string     `json:"statusLink,omitempty"`
	LabelName         string     `json:"labelName,omitempty"`
	ScheduledAt       *time.Time `json:"scheduledTimestamp,omitempty"`
	StartedAt         *time.Time `json:"startedTimestamp,omitempty"`
	FinishedAt        *time.Time `json:"finishedTimestamp,omitempty"`
	Results           []Result   `json:"results,omitempty"`
}

// Response is what the plugin receives.
type Response struct {
	Runs  []Run  `json:"runs"`
	Links []Link `json:"links,omitempty"`
}

// Runs turns a patch set's builds into check runs: one per job (or one
// for a build that has no jobs yet), with each retry of the build as a
// new attempt.
func Runs(builds []buildkite.Build, change, ps int, label string) []Run {
	builds = append([]buildkite.Build(nil), builds...)
	sort.Slice(builds, func(i, j int) bool { return builds[i].Number < builds[j].Number })
	runs := []Run{}
	for i, b := range builds {
		attempt := i + 1
		desc := fmt.Sprintf("Build #%d", b.Number)
		if b.Pipeline.Name != "" {
			desc = b.Pipeline.Name + " " + desc
		}
		var jobs []buildkite.Job
		for _, j := range b.Jobs {
			if j.Type == "script" || j.Type == "trigger" {
				jobs = append(jobs, j)
			}
		}
		if len(jobs) == 0 {
			name := b.Pipeline.Name
			if name == "" {
				name = "CI"
			}
			r := Run{Change: change, Patchset: ps, Attempt: attempt, ExternalID: b.ID, CheckName: name,
				CheckLink: b.WebURL, StatusLink: b.WebURL, StatusDescription: desc, LabelName: label,
				Status: status(b.State, b.State), ScheduledAt: b.CreatedAt, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt}
			if r.Status == "COMPLETED" {
				r.Results = []Result{result(b.ID, b.State, nil, b.WebURL)}
			}
			runs = append(runs, r)
			continue
		}
		for _, j := range jobs {
			name := j.Name
			if name == "" {
				name = j.StepKey
			}
			r := Run{Change: change, Patchset: ps, Attempt: attempt, ExternalID: j.ID, CheckName: name,
				CheckDescription: b.Pipeline.Name, CheckLink: j.WebURL, StatusLink: j.WebURL,
				StatusDescription: desc, LabelName: label, Status: status(j.State, b.State),
				ScheduledAt: j.ScheduledAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
			if r.Status == "COMPLETED" {
				r.Results = []Result{result(j.ID, j.State, j.ExitStatus, j.WebURL)}
			}
			runs = append(runs, r)
		}
	}
	return runs
}

// status maps a Buildkite job (or build) state to a run status. A job
// that never ran in a finished build is complete too.
func status(state, buildState string) string {
	switch {
	case buildkite.Finished(state):
		return "COMPLETED"
	case state == "running" || state == "canceling" || state == "timing_out" || state == "failing":
		return "RUNNING"
	case buildkite.Finished(buildState):
		return "COMPLETED"
	}
	return "SCHEDULED"
}

func result(id, state string, exit *int, url string) Result {
	r := Result{ExternalID: id, Category: "INFO", Summary: "Not run"}
	switch state {
	case "passed":
		r.Category, r.Summary = "SUCCESS", "Passed"
	case "failed", "timed_out":
		r.Category, r.Summary = "ERROR", "Failed"
		if state == "timed_out" {
			r.Summary = "Timed out"
		}
		if exit != nil {
			r.Summary += fmt.Sprintf(" (exit status %d)", *exit)
		}
	case "canceled":
		r.Category, r.Summary = "WARNING", "Canceled"
	case "skipped":
		r.Summary = "Skipped"
	}
	if url != "" {
		r.Links = []Link{{URL: url, Primary: true, Icon: "external", Tooltip: "Open in Buildkite"}}
	}
	return r
}

// Handler serves GET /checks/{change}/{patchset} for the Checks plugin.
// It is public: it shows only Project's changes that are not private,
// which the bridge publishes as GitHub PRs anyway, and caches answers
// briefly so page loads cannot hammer Gerrit or Buildkite.
type Handler struct {
	Project string
	Gerrit  Changes // as the bridge: AOS lets no anonymous user read changes
	Builds  Builds
	// Label is the label CI votes, shown next to the runs.
	Label string
	// AllowOrigin is the Gerrit web origin, for CORS when the bridge is
	// not served from Gerrit's host.
	AllowOrigin string
	Logger      *slog.Logger
	// ClientAddr, if set, says who sent a request (behind a proxy, from
	// X-Forwarded-For), so one client cannot use up everyone's budget.
	ClientAddr func(*http.Request) string

	mu    sync.Mutex
	cache map[[2]int]entry
	busy  chan struct{}
	once  sync.Once
	// window, calls and reads count Buildkite calls and Gerrit reads in
	// the current minute.
	window time.Time
	calls  int
	reads  int
	// misses counts each client's uncached requests this minute.
	misses map[string]int
	now    func() time.Time // tests
}

type entry struct {
	at     time.Time
	status int
	body   []byte
}

const (
	cacheTTL    = 15 * time.Second
	maxCached   = 2000
	maxInFlight = 4
	// maxBuildkitePerMinute bounds the Buildkite calls anonymous requests
	// cause, well inside Buildkite's rate limit, which the bridge also
	// needs for gerrit/Verified.
	maxBuildkitePerMinute = 60
	// maxGerritPerMinute bounds the Gerrit reads anonymous requests cause.
	maxGerritPerMinute = 300
	// maxMissesPerClient bounds one client's uncached requests a minute,
	// so it alone cannot spend the budgets above.
	maxMissesPerClient = 30
)

func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// spend takes one Buildkite call from this minute's budget.
func (h *Handler) spend() bool { return h.take(&h.calls, maxBuildkitePerMinute) }

// read takes one Gerrit read from this minute's budget.
func (h *Handler) read() bool { return h.take(&h.reads, maxGerritPerMinute) }

// miss takes one uncached request from client's budget this minute.
func (h *Handler) miss(client string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roll()
	if h.misses == nil || len(h.misses) > 10000 {
		h.misses = map[string]int{} // bounded; the shared budgets still hold
	}
	if h.misses[client] >= maxMissesPerClient {
		return false
	}
	h.misses[client]++
	return true
}

// roll starts a new minute's budgets if one has passed. h.mu is held.
func (h *Handler) roll() {
	if now := h.clock(); now.Sub(h.window) >= time.Minute {
		h.window, h.calls, h.reads, h.misses = now, 0, 0, nil
	}
}

func (h *Handler) take(n *int, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roll()
	if *n >= limit {
		return false
	}
	*n++
	return true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() {
		h.cache = map[[2]int]entry{}
		h.busy = make(chan struct{}, maxInFlight)
	})
	if h.AllowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", h.AllowOrigin)
		w.Header().Set("Vary", "Origin")
	}
	change, err1 := strconv.Atoi(r.PathValue("change"))
	ps, err2 := strconv.Atoi(r.PathValue("patchset"))
	if err1 != nil || err2 != nil || change <= 0 || ps <= 0 {
		http.Error(w, "bad change or patch set", http.StatusBadRequest)
		return
	}
	key := [2]int{change, ps}
	h.mu.Lock()
	e, ok := h.cache[key]
	h.mu.Unlock()
	if !ok || time.Since(e.at) > cacheTTL {
		client := r.RemoteAddr
		if h.ClientAddr != nil {
			client = h.ClientAddr(r)
		}
		if host, _, err := net.SplitHostPort(client); err == nil {
			client = host
		}
		if !h.miss(client) {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		select {
		case h.busy <- struct{}{}:
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		// Not the request's context: a client that hangs up must not
		// leave a failure behind for everyone.
		e = h.compute(context.Background(), change, ps)
		<-h.busy
		if e.status >= 500 {
			// Transient: not cached.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(e.status)
			w.Write(e.body)
			return
		}
		h.mu.Lock()
		if len(h.cache) >= maxCached {
			for k, v := range h.cache {
				if time.Since(v.at) > cacheTTL {
					delete(h.cache, k)
				}
			}
			if len(h.cache) >= maxCached {
				clear(h.cache)
			}
		}
		h.cache[key] = e
		h.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(cacheTTL.Seconds())))
	w.WriteHeader(e.status)
	w.Write(e.body)
}

func (h *Handler) compute(ctx context.Context, change, ps int) entry {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	fail := func(status int, msg string) entry {
		b, _ := json.Marshal(map[string]string{"error": msg})
		return entry{at: time.Now(), status: status, body: b}
	}
	if !h.read() {
		return fail(http.StatusServiceUnavailable, "busy")
	}
	ch, err := h.Gerrit.GetChange(ctx, change, "ALL_REVISIONS")
	var ge *gerrit.Error
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return fail(http.StatusNotFound, "no such change")
	}
	if err != nil {
		h.Logger.Warn("checks: reading change", "change", change, "err", err)
		return fail(http.StatusBadGateway, "cannot read the change from Gerrit")
	}
	if ch.Project != h.Project || ch.IsPrivate {
		// Gerrit is read as the bridge (AOS lets no anonymous user read
		// changes), which could be made a private change's reviewer:
		// only what the bridge publishes on GitHub anyway is answered.
		return fail(http.StatusNotFound, "not a change the bridge serves")
	}
	sha := ""
	for s, rev := range ch.Revisions {
		if rev.Number == ps {
			sha = s
		}
	}
	if sha == "" {
		return fail(http.StatusNotFound, "no such patch set")
	}
	if !h.spend() {
		return fail(http.StatusServiceUnavailable, "busy")
	}
	builds, err := h.Builds.Builds(ctx, sha)
	if err != nil {
		h.Logger.Warn("checks: listing builds", "change", change, "patchset", ps, "err", err)
		return fail(http.StatusBadGateway, "cannot read builds from Buildkite")
	}
	resp := Response{Runs: Runs(builds, change, ps, h.Label)}
	for _, b := range builds {
		if b.Pipeline.WebURL != "" {
			resp.Links = []Link{{URL: b.Pipeline.WebURL, Tooltip: "Buildkite pipeline", Icon: "external"}}
			break
		}
	}
	b, _ := json.Marshal(resp)
	return entry{at: time.Now(), status: http.StatusOK, body: b}
}
