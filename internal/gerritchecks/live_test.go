package gerritchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// actions is a GitHub Actions check run.
func actions(id int64, name, status, conclusion string) github.CheckRun {
	r := github.CheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion}
	r.App.Name, r.App.Slug = "GitHub Actions", actionsApp
	return r
}

// liveSource is GitHub for Live's tests: what it says of commit c1, and
// how often each part was read.
type liveSource struct {
	mu                    sync.Mutex
	runs                  []github.CheckRun
	statuses              []github.Status
	runReads, statusReads int
	pullReads             int
	// during, if set, runs inside a check-run read (a delivery arriving
	// while GitHub is read).
	during func()
}

func (s *liveSource) Commit(context.Context, int, string) (string, int, error) { return "c1", 7, nil }

func (s *liveSource) CheckRuns(context.Context, string) ([]github.CheckRun, error) {
	s.mu.Lock()
	s.runReads++
	runs, during := append([]github.CheckRun(nil), s.runs...), s.during
	s.mu.Unlock()
	if during != nil {
		during()
	}
	return runs, nil
}

func (s *liveSource) Statuses(context.Context, string) ([]github.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusReads++
	return append([]github.Status(nil), s.statuses...), nil
}

func (s *liveSource) GetPull(context.Context, int) (*github.PullRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pullReads++
	pr := &github.PullRequest{State: "open", MergeableState: "blocked"}
	pr.Head.SHA = "c1"
	return pr, nil
}

type liveEnv struct {
	t    *testing.T
	src  *liveSource
	live *Live
	h    *Handler
	mux  *http.ServeMux
	now  time.Time
}

func newLiveEnv(t *testing.T) *liveEnv {
	e := &liveEnv{t: t, src: &liveSource{}, now: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	e.live = &Live{now: func() time.Time { return e.now }}
	five := &gerrit.ChangeInfo{Project: "p", Revisions: map[string]gerrit.RevisionInfo{"g1": {Number: 1}}}
	reads := 0
	sc := sessionChanges{sees: map[string]fakeChanges{"alice": {5: five}}, reads: &reads}
	e.h = &Handler{Project: "p", GitHub: e.src, Live: e.live, Viewer: sc.viewer, Logger: slog.Default()}
	e.mux = http.NewServeMux()
	e.mux.Handle("GET /checks/{change}/{patchset}", e.h)
	return e
}

// get is the Checks tab's answer for change 5's patch set 1: each run's
// name and status, and its result if any.
func (e *liveEnv) get() map[string]string {
	e.t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/checks/5/1", nil)
	r.Header.Set("Cookie", gerrit.SessionCookie+"=alice")
	e.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		e.t.Fatalf("%d %s", w.Code, w.Body)
	}
	var resp Response
	json.Unmarshal(w.Body.Bytes(), &resp)
	got := map[string]string{}
	for _, run := range resp.Runs {
		v := run.Status
		if len(run.Results) == 1 {
			v += " " + run.Results[0].Summary
		}
		got[fmt.Sprintf("%s#%d", run.CheckName, run.Attempt)] = v
	}
	return got
}

// expire drops the handler's cached answers, as cacheTTL passing would.
func (e *liveEnv) expire() {
	e.h.mu.Lock()
	clear(e.h.cache)
	e.h.mu.Unlock()
}

func (e *liveEnv) deliver(event string, payload any) {
	b, _ := json.Marshal(payload)
	e.live.Deliver(&github.Delivery{Event: event, Payload: b})
}

func checkRunEvent(id int64, sha, name, status, conclusion string) map[string]any {
	return map[string]any{"action": "x", "check_run": map[string]any{"id": id, "head_sha": sha, "name": name, "status": status,
		"conclusion": conclusion, "app": map[string]any{"name": "GitHub Actions", "slug": "github-actions"}}}
}

func jobEvent(id int64, sha, name, status string) map[string]any {
	return map[string]any{"action": status, "workflow_job": map[string]any{"id": 99, "head_sha": sha, "name": name, "status": status,
		"check_run_url": fmt.Sprintf("https://api.github.com/repos/o/r/check-runs/%d", id)}}
}

func statusEvent(sha, context, state string, at time.Time) map[string]any {
	return map[string]any{"sha": sha, "context": context, "state": state, "description": state, "updated_at": at}
}

// subscribe delivers one of each kind for another commit: the deployment's
// webhook sends them.
func (e *liveEnv) subscribe() {
	e.deliver("check_run", checkRunEvent(1000, "other", "x", "completed", "success"))
	e.deliver("workflow_job", jobEvent(1000, "other", "x", "completed"))
	e.deliver("status", statusEvent("other", "x", "success", e.now))
}

// Where the webhook sends checks' deliveries, a viewed commit's checks
// are read from GitHub once and then kept current from deliveries: a
// check finishing shows on the next poll, not a cached answer's worth
// later, and GitHub's check runs and statuses are not read again (its PR
// is: nothing says when whether it can merge changes).
func TestLiveChecksFollowDeliveries(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.src.statuses = []github.Status{{Context: "ci/legacy", State: "pending"}}
	if got := e.get(); got["build#1"] != "SCHEDULED" || got["ci/legacy#1"] != "RUNNING" {
		t.Fatalf("first view: %v", got)
	}
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "in_progress"))
	if got := e.get(); got["build#1"] != "RUNNING" {
		t.Errorf("after the job started: %v", got)
	}
	e.deliver("check_run", checkRunEvent(1, "c1", "build", "completed", "failure"))
	e.deliver("status", statusEvent("c1", "ci/legacy", "success", e.now))
	if got := e.get(); got["build#1"] != "COMPLETED Failed" || got["ci/legacy#1"] != "COMPLETED success" {
		t.Errorf("after it finished: %v", got)
	}
	// A rerun is a new check run.
	e.deliver("check_run", checkRunEvent(2, "c1", "build", "queued", ""))
	if got := e.get(); got["build#2"] != "SCHEDULED" {
		t.Errorf("after a rerun: %v", got)
	}
	// A job first heard of by its own delivery is Actions', so followed.
	e.deliver("workflow_job", jobEvent(3, "c1", "test", "queued"))
	if got := e.get(); got["test#1"] != "SCHEDULED" {
		t.Errorf("after a new job: %v", got)
	}
	if e.src.runReads != 1 || e.src.statusReads != 1 {
		t.Errorf("GitHub read %d times for check runs and %d for statuses, want once each", e.src.runReads, e.src.statusReads)
	}
	if e.src.pullReads < 5 {
		t.Errorf("the PR read %d times: whether it can merge is not delivered", e.src.pullReads)
	}
	// Unchanged, a cached answer is reused.
	before := e.src.pullReads
	e.get()
	if e.src.pullReads != before {
		t.Errorf("an unchanged answer was not reused")
	}
}

// A kind of delivery not seen since the bridge started may not be
// subscribed to: its part is read from GitHub each time, as without
// Live. Check runs need workflow_job too (GitHub sends no check_run
// event when one starts).
func TestLiveChecksNeedTheirDeliveries(t *testing.T) {
	e := newLiveEnv(t)
	e.deliver("check_run", checkRunEvent(1000, "other", "x", "completed", "success"))
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.get()
	e.expire()
	e.src.runs = []github.CheckRun{actions(1, "build", "in_progress", "")}
	if got := e.get(); got["build#1"] != "RUNNING" {
		t.Errorf("without workflow_job deliveries: %v", got)
	}
	if e.src.runReads != 2 || e.src.statusReads != 2 {
		t.Errorf("read %d and %d times, want each read on each request", e.src.runReads, e.src.statusReads)
	}
}

// A delivery can miss (the bridge down, GitHub dropping it): what
// deliveries kept is read from GitHub again after liveMaxAge.
func TestLiveChecksAreReadAgain(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.get()
	e.src.runs = []github.CheckRun{actions(1, "build", "completed", "success")}
	e.now = e.now.Add(liveMaxAge - time.Second)
	e.expire()
	if got := e.get(); got["build#1"] != "SCHEDULED" || e.src.runReads != 1 {
		t.Errorf("before liveMaxAge: %v (%d reads)", got, e.src.runReads)
	}
	e.now = e.now.Add(2 * time.Second)
	e.expire()
	if got := e.get(); got["build#1"] != "COMPLETED Passed" || e.src.runReads != 2 {
		t.Errorf("after liveMaxAge: %v (%d reads)", got, e.src.runReads)
	}
}

// GitHub does not promise to deliver in order: a late delivery of an
// earlier state does not take a check back.
func TestLiveChecksOutOfOrder(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.src.statuses = []github.Status{}
	e.get()
	e.deliver("check_run", checkRunEvent(1, "c1", "build", "completed", "success"))
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "in_progress"))
	e.deliver("check_run", checkRunEvent(1, "c1", "build", "queued", ""))
	e.deliver("status", statusEvent("c1", "ci", "success", e.now))
	e.deliver("status", statusEvent("c1", "ci", "pending", e.now.Add(-time.Minute)))
	if got := e.get(); got["build#1"] != "COMPLETED Passed" || got["ci#1"] != "COMPLETED success" {
		t.Errorf("after late deliveries: %v", got)
	}
}

// A delivery that arrives while GitHub is being read is not undone by
// the read, which began before it.
func TestLiveDeliveryDuringARead(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "in_progress", ""), actions(2, "lint", "completed", "success")}
	e.src.during = func() {
		e.deliver("check_run", checkRunEvent(1, "c1", "build", "completed", "failure"))
		// Late news of lint's start, which the read already has as done.
		e.deliver("check_run", checkRunEvent(2, "c1", "lint", "queued", ""))
	}
	e.get()
	e.src.during = nil
	if got := e.get(); got["build#1"] != "COMPLETED Failed" || got["lint#1"] != "COMPLETED Passed" {
		t.Errorf("the read undid a delivery: %v", got)
	}
}

// Only commits someone has viewed are kept, and only so many.
func TestLiveKeepsOnlyViewedCommits(t *testing.T) {
	l := &Live{}
	for i := range 5 {
		b, _ := json.Marshal(checkRunEvent(int64(i+1), fmt.Sprint("sha", i), "build", "queued", ""))
		l.Deliver(&github.Delivery{Event: "check_run", Payload: b})
	}
	if len(l.commits) != 0 {
		t.Errorf("deliveries for commits nobody viewed kept: %d", len(l.commits))
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxLiveCommits + 1 {
		l.now = func() time.Time { return t0.Add(time.Duration(i) * time.Second) }
		l.merge(fmt.Sprint("sha", i), 0, nil, true, nil, true)
	}
	if len(l.commits) != maxLiveCommits || l.commits["sha0"] != nil || l.commits[fmt.Sprint("sha", maxLiveCommits)] == nil {
		t.Errorf("%d kept; oldest kept %v", len(l.commits), l.commits["sha0"] != nil)
	}
}

// A job's delivery says less than its check run's: what that said
// (its app, its output) is kept.
func TestLiveJobKeepsTheCheckRunsDetails(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	run := github.CheckRun{ID: 1, Name: "build", Status: "queued", DetailsURL: "https://ci.example.com/1"}
	run.App.Name = "Some CI"
	e.src.runs = []github.CheckRun{run}
	e.get()
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "in_progress"))
	v, _ := e.live.get("c1")
	if len(v.runs) != 1 || v.runs[0].App.Name != "Some CI" || v.runs[0].DetailsURL != "https://ci.example.com/1" || v.runs[0].Status != "in_progress" {
		t.Errorf("runs %+v", v.runs)
	}
}

// Another app's check run starting is delivered to nobody (check_run has
// no such event for a repository webhook; workflow_job is Actions'
// alone): while one has not finished, the runs are read each time.
func TestLiveOtherAppsRunsAreRead(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	run := github.CheckRun{ID: 1, Name: "buildkite", Status: "queued"}
	run.App.Name, run.App.Slug = "Buildkite", "buildkite"
	e.src.runs = []github.CheckRun{run, actions(2, "build", "queued", "")}
	e.get()
	run.Status = "in_progress"
	e.src.runs = []github.CheckRun{run, actions(2, "build", "queued", "")}
	e.now = e.now.Add(5 * time.Minute)
	e.expire()
	if got := e.get(); got["buildkite#1"] != "RUNNING" {
		t.Errorf("5 minutes after another app's check started: %v", got)
	}
	// Finished, it changes only by a delivery: no more reads.
	run.Status, run.Conclusion = "completed", "success"
	e.src.runs = []github.CheckRun{run, actions(2, "build", "queued", "")}
	e.expire()
	e.get()
	reads := e.src.runReads
	e.expire()
	if e.get(); e.src.runReads != reads {
		t.Errorf("runs read again with only Actions' unfinished")
	}
}
