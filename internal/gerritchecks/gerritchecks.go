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
	"regexp"
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

// ViewerChanges reads changes as a signed-in user would.
type ViewerChanges interface {
	Changes
	// SignedIn reports whether Gerrit takes the session as someone's (an
	// unknown or expired one reads as anonymous).
	SignedIn(ctx context.Context) (bool, error)
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
	Message    string `json:"message,omitempty"`
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
// Without Viewer it is public: it shows only Project's changes that are
// not private, which the bridge publishes as GitHub PRs anyway. With
// Viewer, it answers only those signed in to Gerrit, for changes Gerrit
// lets them see. It caches answers briefly so page loads cannot hammer
// Gerrit, Buildkite or GitHub.
type Handler struct {
	Project string
	Gerrit  Changes // as the bridge: AOS lets no anonymous user read changes
	Builds  Builds
	// GitHub, if set, is read for the runs instead of Builds: GitHub's
	// checks, where GitHub owns the branches.
	GitHub GitHubSource
	// Viewer, if set, reads Gerrit as whoever has a Gerrit web session
	// (the GerritAccount cookie their browser sends, on Gerrit's own
	// origin): each request is answered only if Gerrit shows them the
	// change, asked each time. For a repository that is not public, whose
	// checks are not the bridge's to publish.
	Viewer func(session string) ViewerChanges
	// Live, if set (with GitHub), keeps the viewed commits' checks
	// current from webhooks, so they are not read from GitHub each time.
	Live *Live
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
	// viewerReads counts Gerrit reads made as viewers this minute: their
	// own budget, so sessions Gerrit does not know (anyone can send one)
	// cannot spend what anonymous requests need, nor the other way.
	viewerReads int
	// flights are the answers being computed, by key: a request for one
	// waits for it rather than computing it again.
	flights map[[2]int]*flight
	// misses counts each client's uncached requests this minute.
	misses map[string]int
	now    func() time.Time // tests
}

type flight struct {
	done chan struct{}
	e    entry
}

type entry struct {
	at     time.Time
	status int
	body   []byte
	// commit and liveSeq, with Live, are the GitHub commit the answer is
	// of and Live's change count for it then: a delivery since makes
	// the answer stale.
	commit  string
	liveSeq uint64
}

const (
	cacheTTL    = 15 * time.Second
	maxCached   = 2000
	maxInFlight = 4
	// maxBuildkitePerMinute bounds the Buildkite (or GitHub) calls
	// requests cause, well inside Buildkite's rate limit, which the
	// bridge also needs for gerrit/Verified (GitHub's, which it needs for
	// everything).
	maxBuildkitePerMinute = 60
	// maxGerritPerMinute bounds the Gerrit reads anonymous requests cause.
	maxGerritPerMinute = 300
	// maxMissesPerClient bounds one client's uncached requests a minute,
	// so it alone cannot spend the budgets above, and maxClients the
	// clients counted.
	maxMissesPerClient = 30
	maxClients         = 10000
	// maxViewerRequestsPerClient bounds one client's requests a minute
	// with Viewer, cached or not: each reads Gerrit as its viewer (twice).
	// Higher: a team behind one address polls from each open change.
	// maxViewerReadsPerMinute bounds all viewers' Gerrit reads, and
	// maxViewerFillsPerClient one client's uncached answers (each up to
	// three GitHub calls), so one viewer cannot spend everyone's GitHub
	// budget.
	maxViewerRequestsPerClient = 120
	maxViewerReadsPerMinute    = 2000
	maxViewerFillsPerClient    = 10
)

// sessionValue is what a Gerrit session cookie looks like: anything else
// is no session, and is not sent on.
var sessionValue = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{1,512}$`)

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
func (h *Handler) miss(client string) bool { return h.missUpTo(client, maxMissesPerClient) }

// missUpTo takes one request from client's budget of limit this minute.
func (h *Handler) missUpTo(client string, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roll()
	if h.misses == nil {
		h.misses = map[string]int{}
	}
	if _, known := h.misses[client]; !known && len(h.misses) >= maxClients {
		// Bounded: new clients wait for the next minute. (Starting over
		// would let enough of them reset everyone's budget; fourth audit.)
		return false
	}
	if h.misses[client] >= limit {
		return false
	}
	h.misses[client]++
	return true
}

// roll starts a new minute's budgets if one has passed. h.mu is held.
func (h *Handler) roll() {
	if now := h.clock(); now.Sub(h.window) >= time.Minute {
		h.window, h.calls, h.reads, h.viewerReads, h.misses = now, 0, 0, 0, nil
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
	if h.AllowOrigin != "" && h.Viewer == nil {
		w.Header().Set("Access-Control-Allow-Origin", h.AllowOrigin)
		w.Header().Set("Vary", "Origin")
	}
	change, err1 := strconv.Atoi(r.PathValue("change"))
	ps, err2 := strconv.Atoi(r.PathValue("patchset"))
	if err1 != nil || err2 != nil || change <= 0 || ps <= 0 {
		http.Error(w, "bad change or patch set", http.StatusBadRequest)
		return
	}
	if h.Viewer != nil {
		h.serveViewer(w, r, change, ps)
		return
	}
	key := [2]int{change, ps}
	h.mu.Lock()
	e, ok := h.cache[key]
	h.mu.Unlock()
	if !ok || time.Since(e.at) > cacheTTL {
		if !h.miss(h.client(r)) {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		var done bool
		if e, done = h.fill(key, func(ctx context.Context) entry { return h.compute(ctx, change, ps) }); !done {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
	}
	if e.status < 500 {
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(cacheTTL.Seconds())))
	}
	write(w, e)
}

// write writes e as the answer.
func write(w http.ResponseWriter, e entry) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(e.status)
	w.Write(e.body)
}

// client is who sent r, for its budget: by address, a /56 for IPv6.
func (h *Handler) client(r *http.Request) string {
	client := r.RemoteAddr
	if h.ClientAddr != nil {
		client = h.ClientAddr(r)
	}
	if host, _, err := net.SplitHostPort(client); err == nil {
		client = host
	}
	if ip := net.ParseIP(client); ip != nil && ip.To4() == nil {
		// One IPv6 host has a /64 to rotate through (fourth audit),
		// often a /56 or a /48 (fifth audit: 65,536 /64s fill the
		// clients counted): a /56, the usual home delegation, is one
		// client.
		client = ip.Mask(net.CIDRMask(56, 128)).String()
	}
	return client
}

// fill computes key's answer with compute, at most maxInFlight at once
// (done is false if that many are), and caches it unless it failed for
// now (5xx).
func (h *Handler) fill(key [2]int, compute func(context.Context) entry) (e entry, done bool) {
	h.mu.Lock()
	if f := h.flights[key]; f != nil {
		h.mu.Unlock()
		<-f.done
		return f.e, true
	}
	select {
	case h.busy <- struct{}{}:
	default:
		h.mu.Unlock()
		return entry{}, false
	}
	if h.flights == nil {
		h.flights = map[[2]int]*flight{}
	}
	f := &flight{done: make(chan struct{})}
	h.flights[key] = f
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.flights, key)
		h.mu.Unlock()
		f.e = e
		close(f.done)
	}()
	// Not the request's context: a client that hangs up must not leave a
	// failure behind for everyone.
	e = compute(context.Background())
	<-h.busy
	if e.status >= 500 {
		return e, true // transient: not cached
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
	return e, true
}

// serveViewer answers with Viewer: only someone signed in to Gerrit, for a
// change Gerrit shows them, read as them on every request (no answer of
// Gerrit's is reused: who may see a change can change at any time). Only
// GitHub's (or Buildkite's) part is cached, as it is the same for all.
// The answer is theirs: no shared cache keeps it, and no other origin
// reads it.
func (h *Handler) serveViewer(w http.ResponseWriter, r *http.Request, change, ps int) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	c, err := r.Cookie(gerrit.SessionCookie)
	if err != nil || !sessionValue.MatchString(c.Value) {
		write(w, failure(http.StatusUnauthorized, "sign in to Gerrit to see checks"))
		return
	}
	client := h.client(r)
	if !h.missUpTo(client, maxViewerRequestsPerClient) {
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	viewer := h.Viewer(c.Value)
	// Signed in, not just carrying a cookie: Gerrit reads a session it
	// does not know (expired, made up) as anonymous, who may see more
	// than nobody.
	if !h.take(&h.viewerReads, maxViewerReadsPerMinute) {
		write(w, failure(http.StatusServiceUnavailable, "busy"))
		return
	}
	if in, err := viewer.SignedIn(ctx); err != nil {
		h.Logger.Warn("checks: asking Gerrit who the viewer is", "err", err)
		write(w, failure(http.StatusBadGateway, "cannot reach Gerrit"))
		return
	} else if !in {
		write(w, failure(http.StatusUnauthorized, "sign in to Gerrit to see checks"))
		return
	}
	sha, e := h.patchSet(ctx, viewer, change, ps, false)
	if e != nil {
		write(w, *e)
		return
	}
	key := [2]int{change, ps}
	h.mu.Lock()
	cached, ok := h.cache[key]
	h.mu.Unlock()
	if !ok || time.Since(cached.at) > cacheTTL || h.Live.changed(cached.commit, cached.liveSeq) {
		if !h.missUpTo("fill "+client, maxViewerFillsPerClient) {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		var done bool
		if cached, done = h.fill(key, func(ctx context.Context) entry {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return h.runs(ctx, change, ps, sha)
		}); !done {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
	}
	write(w, cached)
}

// failure is an answer saying msg.
func failure(status int, msg string) entry {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return entry{at: time.Now(), status: status, body: b}
}

// patchSet reads change with g and finds patch set ps's commit, or the
// answer to give instead. asBridge says g reads as the bridge, which
// answers only for what it publishes.
func (h *Handler) patchSet(ctx context.Context, g Changes, change, ps int, asBridge bool) (string, *entry) {
	fail := func(status int, msg string) (string, *entry) {
		e := failure(status, msg)
		return "", &e
	}
	budget := h.read
	if !asBridge {
		budget = func() bool { return h.take(&h.viewerReads, maxViewerReadsPerMinute) }
	}
	if !budget() {
		return fail(http.StatusServiceUnavailable, "busy")
	}
	ch, err := g.GetChange(ctx, change, "ALL_REVISIONS")
	var ge *gerrit.Error
	// One answer for a change that is not there, is private or another
	// project's, or lacks the patch set: which, says what it is (fourth
	// audit: that a private change exists, say). For a viewer, Gerrit
	// answers 404 for what they may not see.
	const notFound = "no such change or patch set"
	if errors.As(err, &ge) && (ge.Status == http.StatusNotFound || (!asBridge && ge.Status/100 == 4)) {
		return fail(http.StatusNotFound, notFound)
	}
	if err != nil {
		h.Logger.Warn("checks: reading change", "change", change, "err", err)
		return fail(http.StatusBadGateway, "cannot read the change from Gerrit")
	}
	if ch.Project != h.Project || (asBridge && ch.IsPrivate) {
		// Gerrit is read as the bridge (AOS lets no anonymous user read
		// changes), which could be made a private change's reviewer:
		// only what the bridge publishes on GitHub anyway is answered.
		return fail(http.StatusNotFound, notFound)
	}
	for s, rev := range ch.Revisions {
		if rev.Number == ps {
			return s, nil
		}
	}
	return fail(http.StatusNotFound, notFound)
}

func (h *Handler) compute(ctx context.Context, change, ps int) entry {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sha, e := h.patchSet(ctx, h.Gerrit, change, ps, true)
	if e != nil {
		return *e
	}
	return h.runs(ctx, change, ps, sha)
}

// runs is the answer for change's patch set ps, commit sha: GitHub's
// checks, or Buildkite's builds.
func (h *Handler) runs(ctx context.Context, change, ps int, sha string) entry {
	if h.GitHub != nil {
		return h.githubEntry(ctx, change, ps, sha)
	}
	fail := failure
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
