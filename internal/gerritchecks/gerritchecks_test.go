package gerritchecks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
)

func ptr[T any](v T) *T { return &v }

func TestRuns(t *testing.T) {
	at := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	builds := []buildkite.Build{
		{ID: "b2", Number: 8, State: "running", Pipeline: buildkite.Pipeline{Name: "ci"}, Jobs: []buildkite.Job{
			{ID: "j3", Type: "script", Name: "Build", State: "running", WebURL: "u3", StartedAt: &at},
			{ID: "w", Type: "waiter"},
			{ID: "j4", Type: "script", Name: "Test", State: "waiting"},
		}},
		{ID: "b1", Number: 7, State: "failed", Pipeline: buildkite.Pipeline{Name: "ci"}, Jobs: []buildkite.Job{
			{ID: "j1", Type: "script", Name: "Build", State: "passed", WebURL: "u1"},
			{ID: "j2", Type: "script", Name: "Test", State: "failed", ExitStatus: ptr(3), WebURL: "u2"},
			{ID: "j5", Type: "script", Name: "Deploy", State: "waiting_failed"},
		}},
		{ID: "b0", Number: 9, State: "scheduled", WebURL: "b0url", Pipeline: buildkite.Pipeline{Name: "ci"}},
	}
	runs := Runs(builds, 5, 2, "Verified")
	type got struct {
		name    string
		attempt int
		status  string
		result  string
	}
	var g []got
	for _, r := range runs {
		res := ""
		if len(r.Results) == 1 {
			res = r.Results[0].Category + " " + r.Results[0].Summary
		}
		if r.Change != 5 || r.Patchset != 2 || r.LabelName != "Verified" {
			t.Errorf("run %+v: wrong change, patch set or label", r)
		}
		g = append(g, got{r.CheckName, r.Attempt, r.Status, res})
	}
	want := []got{
		{"Build", 1, "COMPLETED", "SUCCESS Passed"},
		{"Test", 1, "COMPLETED", "ERROR Failed (exit status 3)"},
		{"Deploy", 1, "COMPLETED", "INFO Not run"},
		{"Build", 2, "RUNNING", ""},
		{"Test", 2, "SCHEDULED", ""},
		{"ci", 3, "SCHEDULED", ""},
	}
	if len(g) != len(want) {
		t.Fatalf("runs = %+v", g)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, g[i], want[i])
		}
	}
	if runs[1].Results[0].Links[0].URL != "u2" || runs[5].CheckLink != "b0url" {
		t.Error("links not set")
	}
}

type fakeChanges map[int]*gerrit.ChangeInfo

func (f fakeChanges) GetChange(_ context.Context, n int, _ ...string) (*gerrit.ChangeInfo, error) {
	if c, ok := f[n]; ok {
		return c, nil
	}
	return nil, &gerrit.Error{Status: http.StatusNotFound}
}

type fakeBuilds struct {
	calls  int
	builds map[string][]buildkite.Build
}

func (f *fakeBuilds) Builds(_ context.Context, sha string) ([]buildkite.Build, error) {
	f.calls++
	return f.builds[sha], nil
}

func TestHandler(t *testing.T) {
	changes := fakeChanges{
		5: {Project: "p", Revisions: map[string]gerrit.RevisionInfo{"aaa": {Number: 1}, "bbb": {Number: 2}}},
		6: {Project: "other", Revisions: map[string]gerrit.RevisionInfo{"ccc": {Number: 1}}},
	}
	fb := &fakeBuilds{builds: map[string][]buildkite.Build{
		"bbb": {{ID: "b", Number: 1, State: "passed", Pipeline: buildkite.Pipeline{Name: "ci", WebURL: "pipe"}}},
		"ccc": {{ID: "secret", Number: 1, State: "passed"}},
	}}
	h := &Handler{Project: "p", Gerrit: changes, Builds: fb, Label: "Verified", AllowOrigin: "http://gerrit"}
	mux := http.NewServeMux()
	mux.Handle("GET /checks/{change}/{patchset}", h)
	get := func(path string) (*httptest.ResponseRecorder, Response) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var r Response
		json.Unmarshal(w.Body.Bytes(), &r)
		return w, r
	}
	w, r := get("/checks/5/2")
	if w.Code != 200 || len(r.Runs) != 1 || r.Runs[0].ExternalID != "b" || len(r.Links) != 1 || r.Links[0].URL != "pipe" {
		t.Fatalf("GET /checks/5/2 = %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "http://gerrit" {
		t.Error("no CORS header")
	}
	get("/checks/5/2")
	if fb.calls != 1 {
		t.Errorf("Buildkite called %d times; want the second answer cached", fb.calls)
	}
	for _, path := range []string{"/checks/6/1", "/checks/7/1", "/checks/5/3", "/checks/5/x", "/checks/-1/1"} {
		if w, _ := get(path); w.Code == 200 || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("GET %s = %d %s", path, w.Code, w.Body)
		}
	}
}

func TestPlugin(t *testing.T) {
	js := string(Plugin("https://example.com/bridge/", `p"q`))
	if !strings.Contains(js, `const BRIDGE_URL = "https://example.com/bridge";`) || !strings.Contains(js, `const PROJECT = "p\"q";`) {
		t.Errorf("plugin not filled in:\n%s", js[:400])
	}
}

// flakyChanges fails while down, or when the request's context is done.
type flakyChanges struct {
	fakeChanges
	down bool
}

func (f *flakyChanges) GetChange(ctx context.Context, n int, o ...string) (*gerrit.ChangeInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.down {
		return nil, errors.New("connection refused")
	}
	return f.fakeChanges.GetChange(ctx, n, o...)
}

// Failures are not cached, and a client hanging up does not make one: a
// stranger could otherwise keep a patch set's Checks tab broken.
func TestHandlerDoesNotCacheFailures(t *testing.T) {
	gc := &flakyChanges{fakeChanges: fakeChanges{5: {Project: "p", Revisions: map[string]gerrit.RevisionInfo{"aaa": {Number: 1}}}}, down: true}
	h := &Handler{Project: "p", Gerrit: gc, Builds: &fakeBuilds{}, Logger: slog.Default()}
	get := func(ctx context.Context) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/checks/5/1", nil).WithContext(ctx)
		r.SetPathValue("change", "5")
		r.SetPathValue("patchset", "1")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := get(context.Background()); code != http.StatusBadGateway {
		t.Fatalf("Gerrit down: %d", code)
	}
	gc.down = false
	if code := get(context.Background()); code != 200 {
		t.Errorf("after Gerrit came back: %d (a failure was cached)", code)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	h.cache = map[[2]int]entry{}
	if code := get(gone); code != 200 {
		t.Errorf("client hung up: %d", code)
	}
}

func TestHandlerBoundsBuildkiteCalls(t *testing.T) {
	revs := map[string]gerrit.RevisionInfo{}
	for i := 1; i <= maxBuildkitePerMinute+5; i++ {
		revs[fmt.Sprintf("sha%d", i)] = gerrit.RevisionInfo{Number: i}
	}
	fb := &fakeBuilds{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := &Handler{Project: "p", Gerrit: fakeChanges{5: {Project: "p", Revisions: revs}}, Builds: fb, Logger: slog.Default(),
		now: func() time.Time { return now }}
	busy := 0
	for i := 1; i <= maxBuildkitePerMinute+5; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("change", "5")
		r.SetPathValue("patchset", strconv.Itoa(i))
		h.ServeHTTP(w, r)
		if w.Code == http.StatusServiceUnavailable {
			busy++
		}
	}
	if fb.calls != maxBuildkitePerMinute || busy != 5 {
		t.Errorf("Buildkite calls %d, busy answers %d", fb.calls, busy)
	}
	now = now.Add(time.Minute)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("change", "5")
	r.SetPathValue("patchset", "1")
	h.cache = map[[2]int]entry{}
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("next minute: %d", w.Code)
	}
}

// Anonymous requests for changes that do not exist still read Gerrit:
// those reads are bounded too.
func TestHandlerBoundsGerritReads(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reads := 0
	h := &Handler{Project: "p", Gerrit: countingChanges{&reads}, Builds: &fakeBuilds{}, Logger: slog.Default(),
		now: func() time.Time { return now }}
	busy := 0
	for i := 1; i <= maxGerritPerMinute+5; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("change", strconv.Itoa(i))
		r.SetPathValue("patchset", "1")
		h.ServeHTTP(w, r)
		if w.Code == http.StatusServiceUnavailable {
			busy++
		}
	}
	if reads != maxGerritPerMinute || busy != 5 {
		t.Errorf("Gerrit reads %d, busy answers %d", reads, busy)
	}
}

type countingChanges struct{ n *int }

func (c countingChanges) GetChange(context.Context, int, ...string) (*gerrit.ChangeInfo, error) {
	*c.n++
	return nil, &gerrit.Error{Status: http.StatusNotFound}
}
