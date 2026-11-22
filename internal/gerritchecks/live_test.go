package gerritchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// age makes the handler's cached answers d older.
func (e *liveEnv) age(d time.Duration) {
	e.h.mu.Lock()
	for k, v := range e.h.cache {
		v.at = v.at.Add(-d)
		e.h.cache[k] = v
	}
	e.h.mu.Unlock()
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

var statusIDs int64

// statusEvent is a status delivery; each new one has a larger ID, as
// GitHub's do.
func statusEvent(sha, context, state string, at time.Time) map[string]any {
	statusIDs++
	return map[string]any{"id": statusIDs, "sha": sha, "context": context, "state": state, "description": state, "updated_at": at}
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
	// A rerun is a new check run, and a new job.
	e.deliver("check_run", checkRunEvent(2, "c1", "build", "queued", ""))
	e.deliver("workflow_job", jobEvent(2, "c1", "build", "queued"))
	if got := e.get(); got["build#2"] != "SCHEDULED" {
		t.Errorf("after a rerun: %v", got)
	}
	// A job first heard of by its own delivery is Actions', so followed.
	e.deliver("workflow_job", jobEvent(3, "c1", "test", "queued"))
	if got := e.get(); got["test#1"] != "SCHEDULED" {
		t.Errorf("after a new job: %v", got)
	}
	if e.src.runReads != 1 || e.src.statusReads != 1 || e.h.RunReads() != 1 {
		t.Errorf("GitHub read %d times for check runs (counted %d) and %d for statuses, want once each", e.src.runReads, e.h.RunReads(), e.src.statusReads)
	}
	// Each answer a delivery changed is built again from what was
	// kept, the PR as read (deliveries are not reads of GitHub: the
	// cache's time bounds those). Past it, the PR is read again:
	// nothing is delivered when whether it can merge changes.
	if e.src.pullReads != 1 {
		t.Errorf("the PR read %d times within cacheTTL", e.src.pullReads)
	}
	e.expire()
	e.get()
	if e.src.pullReads != 2 || e.src.runReads != 1 {
		t.Errorf("past cacheTTL: PR read %d times, runs %d", e.src.pullReads, e.src.runReads)
	}
}

// However many deliveries, one change's answer reads GitHub at most
// once a cacheTTL, as without Live, for any number of viewers.
func TestLiveDeliveriesAreNotReads(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.src.statuses = []github.Status{}
	e.get()
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "queued"))
	for i := 1; i <= 20; i++ {
		e.deliver("workflow_job", jobEvent(int64(100+i), "c1", fmt.Sprint("job", i), "queued"))
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/checks/5/1", nil)
		r.Header.Set("Cookie", gerrit.SessionCookie+"=alice")
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i)
		e.mux.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"job%d"`, i)) {
			t.Fatalf("poll %d: %d, without its job: %s", i, w.Code, w.Body)
		}
	}
	if e.src.pullReads != 1 || e.src.runReads != 1 || e.src.statusReads != 1 {
		t.Errorf("GitHub read %d/%d/%d times (PR/runs/statuses)", e.src.pullReads, e.src.runReads, e.src.statusReads)
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
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "queued"))
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
	pending := statusEvent("c1", "ci", "pending", e.now.Add(-time.Minute))
	e.deliver("status", statusEvent("c1", "ci", "success", e.now))
	e.deliver("status", pending)
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
	// Read without IDs or times: which is newer cannot be told, and
	// the delivery, which came after the read began, is kept.
	e.src.statuses = []github.Status{{Context: "ci", State: "pending"}}
	e.src.during = func() {
		e.deliver("status", statusEvent("c1", "ci", "success", e.now))
		e.deliver("check_run", checkRunEvent(1, "c1", "build", "completed", "failure"))
		// Late news of lint's start, which the read already has as done.
		e.deliver("check_run", checkRunEvent(2, "c1", "lint", "queued", ""))
	}
	e.get()
	e.src.during = nil
	if got := e.get(); got["build#1"] != "COMPLETED Failed" || got["lint#1"] != "COMPLETED Passed" || got["ci#1"] != "COMPLETED success" {
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
	e.deliver("workflow_job", jobEvent(2, "c1", "build", "queued"))
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

// A quick check's pending and success share a second (updated_at is to
// the second): delivered out of order, the late pending does not take
// the success back. GitHub's status IDs grow.
func TestLiveStatusesSameSecondOutOfOrder(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.statuses = []github.Status{}
	e.get()
	pending := statusEvent("c1", "ci/quick", "pending", e.now)
	success := statusEvent("c1", "ci/quick", "success", e.now)
	e.deliver("status", success)
	e.deliver("status", pending) // late
	if got := e.get(); got["ci/quick#1"] != "COMPLETED success" {
		t.Errorf("a late pending of the same second took the status back: %v", got)
	}
}

// Without IDs, deliveries of the same second cannot be told apart: the
// one that came last is taken.
func TestLiveStatusesWithoutIDs(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.statuses = []github.Status{}
	e.get()
	for _, state := range []string{"pending", "success"} {
		ev := statusEvent("c1", "ci", state, e.now)
		delete(ev, "id")
		e.deliver("status", ev)
	}
	if got := e.get(); got["ci#1"] != "COMPLETED success" {
		t.Errorf("%v", got)
	}
}

// What is kept is bounded: each check's text clipped as the tab shows
// it, overlong URLs dropped, and all commits' text together, the least
// recently viewed dropped first (anyone whose workflow may create check
// runs writes it).
func TestLiveTextIsBounded(t *testing.T) {
	l := &Live{}
	l.get("c1")
	ev := checkRunEvent(1, "c1", strings.Repeat("n", 65535), "completed", "success")
	ev["check_run"].(map[string]any)["output"] = map[string]any{"title": strings.Repeat("t", 65535), "summary": strings.Repeat("x", 65535)}
	ev["check_run"].(map[string]any)["html_url"] = "https://github.com/" + strings.Repeat("u", 65535)
	b, _ := json.Marshal(ev)
	l.Deliver(&github.Delivery{Event: "check_run", Payload: b})
	st := statusEvent("c1", strings.Repeat("c", 65535), "success", time.Now())
	st["description"], st["target_url"] = strings.Repeat("d", 65535), "https://ci.example.com/"+strings.Repeat("u", 65535)
	b, _ = json.Marshal(st)
	l.Deliver(&github.Delivery{Event: "status", Payload: b})
	v, _ := l.get("c1")
	r, s := v.runs[0], v.statuses[0]
	for name, n := range map[string]int{"name": len(r.Name), "title": len(r.Output.Title), "summary": len(r.Output.Summary),
		"context": len(s.Context), "description": len(s.Description)} {
		if n > maxMessage+len("…") {
			t.Errorf("kept %d bytes of a %s", n, name)
		}
	}
	if r.HTMLURL != "" || s.TargetURL != "" {
		t.Errorf("overlong URLs kept")
	}

	// All commits' text: each read of 100 runs of 12 kB.
	l = &Live{}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var runs []github.CheckRun
	for i := range 200 {
		r := actions(int64(i+1), "build", "completed", "failure")
		r.Output.Title, r.Output.Summary = strings.Repeat("t", 6000), strings.Repeat("x", 6000)
		runs = append(runs, r)
	}
	for i := range 100 {
		l.now = func() time.Time { return t0.Add(time.Duration(i) * time.Second) }
		l.merge(fmt.Sprint("sha", i), 0, runs, true, nil, true)
	}
	total := 0
	for _, c := range l.commits {
		total += c.bytes
		if len(c.runs) > 100 { // as many as a read returns
			t.Errorf("%d runs kept of one commit", len(c.runs))
		}
		for _, r := range c.runs {
			if n := len(r.run.Output.Summary); n > maxMessage+len("…") {
				t.Fatalf("kept %d bytes of a summary read", n)
			}
		}
	}
	if l.bytes > maxLiveBytes || total != l.bytes || l.commits["sha99"] == nil || l.commits["sha0"] != nil {
		t.Errorf("kept %d bytes (counted %d) of %d commits; newest kept %v, oldest %v", total, l.bytes, len(l.commits),
			l.commits["sha99"] != nil, l.commits["sha0"] != nil)
	}
}

// A commit dropped while GitHub is read (others viewed meanwhile) misses
// the deliveries until it is kept again: the read is not taken as
// current, and the next request reads GitHub again.
func TestLiveDroppedDuringARead(t *testing.T) {
	l := &Live{sawCheckRun: true, sawWorkflowJob: true, sawStatus: true}
	_, begin := l.get("c1")
	for i := range maxLiveCommits {
		l.get(fmt.Sprint("o", i))
	}
	b, _ := json.Marshal(checkRunEvent(1, "c1", "build", "completed", "success"))
	l.Deliver(&github.Delivery{Event: "check_run", Payload: b})
	l.merge("c1", begin, []github.CheckRun{actions(1, "build", "queued", "")}, true, nil, true)
	if v, _ := l.get("c1"); v.haveRuns || v.haveStatuses {
		t.Errorf("a read that missed a delivery taken as current: %+v", v)
	}
	// Read again, it is.
	_, begin = l.get("c1")
	l.merge("c1", begin, []github.CheckRun{actions(1, "build", "completed", "success")}, true, nil, true)
	if v, _ := l.get("c1"); !v.haveRuns || !v.haveStatuses || v.runs[0].Status != "completed" {
		t.Errorf("read again: %+v", v)
	}
}

// Runs a workflow makes itself (a test reporter, with the workflow's
// token) are GitHub Actions' too, but no workflow_job is about them, so
// their start is delivered to nobody: while one is unfinished, the runs
// are read. An Actions job is followed once a delivery is about it.
func TestLiveWorkflowsOwnRunsAreRead(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "report", "queued", "")}
	e.get()
	e.src.runs = []github.CheckRun{actions(1, "report", "in_progress", "")}
	e.expire()
	if got := e.get(); got["report#1"] != "RUNNING" || e.src.runReads != 2 {
		t.Errorf("a workflow's own run, started: %v (%d reads)", got, e.src.runReads)
	}
	e.deliver("workflow_job", jobEvent(1, "c1", "report", "in_progress"))
	e.expire()
	e.get()
	if e.src.runReads != 2 {
		t.Errorf("a job a delivery is about read again: %d reads", e.src.runReads)
	}
	// Still followed after its check run's own deliveries.
	e.deliver("workflow_job", jobEvent(2, "c1", "lint", "queued"))
	e.deliver("check_run", checkRunEvent(2, "c1", "lint", "queued", ""))
	e.expire()
	e.get()
	if e.src.runReads != 2 {
		t.Errorf("a job read again after its check run's delivery: %d reads", e.src.runReads)
	}
}

// GitHub's API can lag its webhooks: a read just after a delivery that
// still says less (in progress, not done) does not take the run back,
// though the runs are read on each request (another app's is unfinished)
// and would then be trusted once that one finishes. Statuses are never
// set back by a read.
func TestLiveLaggingReadDoesNotUndoADelivery(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	other := github.CheckRun{ID: 9, Name: "ext", Status: "in_progress"}
	other.App.Name, other.App.Slug = "Ext", "ext"
	e.src.runs = []github.CheckRun{other, actions(1, "build", "in_progress", "")}
	e.src.statuses = []github.Status{{ID: 1, Context: "ci", State: "pending"}}
	e.get()
	e.deliver("check_run", checkRunEvent(1, "c1", "build", "completed", "success"))
	ev := statusEvent("c1", "ci", "success", e.now)
	ev["id"] = 2
	e.deliver("status", ev)
	e.expire()
	if got := e.get(); got["build#1"] != "COMPLETED Passed" || got["ci#1"] != "COMPLETED success" {
		t.Errorf("a lagging read took them back: %v", got)
	}
	// Past liveLag a read is believed: an app may set its run back.
	e.now = e.now.Add(liveLag + time.Second)
	e.expire()
	if got := e.get(); got["build#1"] != "RUNNING" {
		t.Errorf("past liveLag: %v", got)
	}
}

// A job's delivery says less than its check run: a read meeting it
// keeps what only the read says.
func TestLiveReadKeepsDetailsOfASparseDelivery(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	ext := github.CheckRun{ID: 9, Name: "ext", Status: "queued"} // keeps the runs read
	ext.App.Slug = "ext"
	run := actions(1, "build", "in_progress", "")
	run.DetailsURL, run.Output.Title = "https://ci.example.com/1", "Building"
	e.src.runs = []github.CheckRun{ext}
	e.get()
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "in_progress"))
	// A lagging read, still queued: the delivery's state, the read's
	// details.
	lagging := run
	lagging.Status = "queued"
	e.src.runs = []github.CheckRun{ext, lagging}
	e.expire()
	e.get()
	if v, _ := e.live.get("c1"); !slices.ContainsFunc(v.runs, func(r github.CheckRun) bool {
		return r.ID == 1 && r.Status == "in_progress" && r.DetailsURL == run.DetailsURL && r.Output.Title == "Building"
	}) {
		t.Errorf("a lagging read: %+v", v.runs)
	}
	// A read the job's delivery comes in the middle of.
	run2 := actions(2, "test", "in_progress", "")
	run2.DetailsURL = "https://ci.example.com/2"
	e.src.runs = []github.CheckRun{ext, run, run2}
	e.src.during = func() { e.deliver("workflow_job", jobEvent(2, "c1", "test", "in_progress")) }
	e.expire()
	e.get()
	v, _ := e.live.get("c1")
	for _, r := range v.runs {
		if r.ID == 1 && (r.Status != "in_progress" || r.DetailsURL != run.DetailsURL || r.Output.Title != "Building") {
			t.Errorf("a lagging read: %+v", r)
		}
		if r.ID == 2 && r.DetailsURL != run2.DetailsURL {
			t.Errorf("a delivery during a read: %+v", r)
		}
	}
}

// Status IDs grow: a read never sets one back, however old the
// delivery it lags.
func TestLiveReadNeverSetsAStatusBack(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.statuses = []github.Status{{ID: 1, Context: "ci", State: "pending"}}
	e.get()
	ev := statusEvent("c1", "ci", "success", e.now)
	ev["id"] = 2
	e.deliver("status", ev)
	e.now = e.now.Add(liveMaxAge + time.Second)
	e.expire()
	if got := e.get(); got["ci#1"] != "COMPLETED success" || e.src.statusReads != 2 {
		t.Errorf("%v (%d reads)", got, e.src.statusReads)
	}
}

// Past maxLiveRuns a delivery is not kept, so what is kept is not all
// there is: a rerun of a large matrix shows on the next poll, read from
// GitHub (which answers with the newest), not ten minutes later.
// Statuses likewise.
func TestLiveDeliveryPastTheCapIsRead(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	var runs []github.CheckRun
	var statuses []github.Status
	for i := range maxLiveRuns {
		runs = append(runs, actions(int64(i+1), fmt.Sprint("job", i), "completed", "failure"))
		statuses = append(statuses, github.Status{ID: int64(i + 1), Context: fmt.Sprint("s", i), State: "failure"})
	}
	e.src.runs, e.src.statuses = runs, statuses
	e.get()
	e.src.runs = append([]github.CheckRun{actions(1000, "job0", "queued", "")}, runs[:maxLiveRuns-1]...)
	e.src.statuses = append([]github.Status{{ID: 1000, Context: "new", State: "pending"}}, statuses[:maxLiveStatuses-1]...)
	e.deliver("check_run", checkRunEvent(1000, "c1", "job0", "queued", ""))
	e.expire()
	if got := e.get(); got["job0#2"] != "SCHEDULED" {
		t.Errorf("past the cap: job0#2 %q", got["job0#2"])
	}
	e.deliver("status", statusEvent("c1", "new", "pending", e.now))
	e.expire()
	if got := e.get(); got["new#1"] != "RUNNING" {
		t.Errorf("past the cap: new#1 %q", got["new#1"])
	}
}

// Past maxLiveBytes only commits holding text are dropped: others'
// large ones do not push out the many that hold little.
func TestLiveByteBoundSparesCommitsWithoutText(t *testing.T) {
	l := &Live{}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := 0
	l.now = func() time.Time { tick++; return t0.Add(time.Duration(tick) * time.Second) }
	for i := range 1000 {
		l.get(fmt.Sprint("small", i))
	}
	var runs []github.CheckRun
	for i := range 100 {
		r := actions(int64(i+1), strings.Repeat("n", 3990), "completed", "failure")
		r.Output.Title, r.Output.Summary = strings.Repeat("t", 3990), strings.Repeat("x", 3990)
		runs = append(runs, r)
	}
	for i := range 80 {
		l.merge(fmt.Sprint("big", i), 0, runs, true, nil, true)
	}
	small := 0
	for sha := range l.commits {
		if strings.HasPrefix(sha, "small") {
			small++
		}
	}
	if small != 1000 || l.bytes > maxLiveBytes {
		t.Errorf("%d of 1000 small commits kept, %d bytes", small, l.bytes)
	}
}

// An answer built again for a delivery keeps its time: the PR is read
// again cacheTTL after it was, however many deliveries come.
func TestLiveRebuiltAnswerKeepsItsTime(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.src.statuses = []github.Status{}
	e.get()
	e.age(cacheTTL - 5*time.Second)
	e.deliver("workflow_job", jobEvent(1, "c1", "build", "in_progress"))
	e.get()
	if e.src.pullReads != 1 {
		t.Fatalf("setup: the PR read %d times", e.src.pullReads)
	}
	e.age(6 * time.Second)
	e.get()
	if e.src.pullReads != 2 {
		t.Errorf("the PR read %d times", e.src.pullReads)
	}
}

// A commit Live dropped (others viewed) is not built again from nothing:
// the cached answer stands until it expires.
func TestLiveRebuildOfADroppedCommit(t *testing.T) {
	e := newLiveEnv(t)
	e.subscribe()
	e.src.runs = []github.CheckRun{actions(1, "build", "queued", "")}
	e.get()
	for i := range maxLiveCommits {
		e.now = e.now.Add(time.Second)
		e.live.get(fmt.Sprint("o", i))
	}
	if e.live.commits["c1"] != nil {
		t.Fatal("setup: c1 not dropped")
	}
	if got := e.get(); got["build#1"] != "SCHEDULED" {
		t.Errorf("after the commit was dropped: %v", got)
	}
}
