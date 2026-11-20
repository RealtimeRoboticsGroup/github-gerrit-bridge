package gerritchecks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

func TestGitHubRuns(t *testing.T) {
	at := func(m int) *time.Time { v := time.Date(2026, 11, 20, 10, m, 0, 0, time.UTC); return &v }
	fail := github.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/o/r/runs/1",
		StartedAt: at(0), CompletedAt: at(5)}
	fail.Output.Title, fail.Output.Summary = "2 tests failed", "TestA\nTestB"
	fail.App.Name = "GitHub Actions"
	checks := []github.CheckRun{
		{ID: 2, Name: "build", Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/o/r/runs/2", StartedAt: at(10)},
		fail,
		{ID: 3, Name: "lint", Status: "in_progress", DetailsURL: "https://ci.example.com/3"},
		{ID: 4, Name: "deploy", Status: "queued"},
	}
	statuses := []github.Status{
		{Context: "ci/legacy", State: "failure", Description: "it broke", TargetURL: "https://ci.example.com/l"},
		{Context: "gerrit/Verified", State: "pending"},
		{Context: "ci/slow", State: "pending"},
	}
	pr := &github.PullRequest{State: "open", HTMLURL: "https://github.com/o/r/pull/7", MergeableState: "blocked"}
	pr.Head.SHA = "c1"
	runs := GitHubRuns(checks, statuses, pr, "c1", 5, 2)
	type got struct {
		name    string
		attempt int
		status  string
		result  string
		link    string
	}
	var g []got
	for _, r := range runs {
		res := ""
		if len(r.Results) == 1 {
			res = r.Results[0].Category + " " + r.Results[0].Summary
		}
		if r.Change != 5 || r.Patchset != 2 {
			t.Errorf("run %+v: wrong change or patch set", r)
		}
		g = append(g, got{r.CheckName, r.Attempt, r.Status, res, r.CheckLink})
	}
	want := []got{
		{"build", 1, "COMPLETED", "ERROR Failed: 2 tests failed", "https://github.com/o/r/runs/1"},
		{"build", 2, "COMPLETED", "SUCCESS Passed", "https://github.com/o/r/runs/2"},
		{"deploy", 1, "SCHEDULED", "", ""},
		{"lint", 1, "RUNNING", "", "https://ci.example.com/3"},
		{"ci/legacy", 1, "COMPLETED", "ERROR it broke", "https://ci.example.com/l"},
		{"ci/slow", 1, "RUNNING", "", ""},
		{"GitHub merge", 1, "COMPLETED", "WARNING Blocked: it needs reviews or required checks (see the PR)", "https://github.com/o/r/pull/7"},
	}
	if len(g) != len(want) {
		t.Fatalf("runs = %+v", g)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, g[i], want[i])
		}
	}
	if r := runs[0]; r.Results[0].Message != "TestA\nTestB" || r.CheckDescription != "GitHub Actions" || r.Results[0].Links[0].URL != fail.HTMLURL {
		t.Errorf("failed run's details: %+v", r)
	}
	// The PR's merge box only for its head commit.
	if runs := GitHubRuns(nil, nil, pr, "older", 5, 1); len(runs) != 0 {
		t.Errorf("an older commit got %+v", runs)
	}
}

func TestMergeRun(t *testing.T) {
	for state, want := range map[string]string{
		"clean":     "COMPLETED SUCCESS",
		"has_hooks": "COMPLETED SUCCESS",
		"unstable":  "COMPLETED SUCCESS",
		"blocked":   "COMPLETED WARNING",
		"behind":    "COMPLETED WARNING",
		"dirty":     "COMPLETED ERROR",
		"draft":     "COMPLETED INFO",
		"unknown":   "RUNNING ",
		"":          "RUNNING ",
	} {
		r := mergeRun(&github.PullRequest{State: "open", MergeableState: state}, 1, 1)
		got := r.Status + " "
		if len(r.Results) == 1 {
			got += r.Results[0].Category
		}
		if got != want {
			t.Errorf("%q: %s, want %s", state, got, want)
		}
	}
	if r := mergeRun(&github.PullRequest{State: "closed", Merged: true, MergeableState: "dirty"}, 1, 1); r.Results[0].Summary != "Merged" {
		t.Errorf("merged: %+v", r)
	}
	if r := mergeRun(&github.PullRequest{State: "closed", MergeableState: "clean"}, 1, 1); r.Results[0].Summary != "Closed" {
		t.Errorf("closed: %+v", r)
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("é", maxMessage)
	if c := clip(s); len(c) > maxMessage+len("…") || !utf8.ValidString(c) || !strings.HasSuffix(c, "…") {
		t.Errorf("clip: %d bytes, valid %v", len(c), utf8.ValidString(c))
	}
	if oneLine(" a\n b\t c ") != "a b c" {
		t.Errorf("oneLine: %q", oneLine(" a\n b\t c "))
	}
}

type fakeSource struct {
	commits map[string]string // Gerrit commit -> GitHub commit
	pr      *github.PullRequest
	calls   int
}

func (f *fakeSource) Commit(_ context.Context, change int, sha string) (string, int, error) {
	return f.commits[sha], 7, nil
}
func (f *fakeSource) CheckRuns(context.Context, string) ([]github.CheckRun, error) {
	f.calls++
	return []github.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"}}, nil
}
func (f *fakeSource) Statuses(context.Context, string) ([]github.Status, error) { return nil, nil }
func (f *fakeSource) GetPull(context.Context, int) (*github.PullRequest, error) {
	if f.pr != nil {
		return f.pr, nil
	}
	pr := &github.PullRequest{State: "open", MergeableState: "clean"}
	pr.Head.SHA = "c2"
	return pr, nil
}

// sessionChanges is Gerrit as each session sees it: who may see what.
type sessionChanges struct {
	sees  map[string]fakeChanges
	reads *int
}

func (s sessionChanges) viewer(session string) Changes {
	return readCounter{s.sees[session], s.reads}
}

type readCounter struct {
	fakeChanges
	n *int
}

func (r readCounter) GetChange(ctx context.Context, n int, o ...string) (*gerrit.ChangeInfo, error) {
	*r.n++
	if r.fakeChanges == nil {
		return nil, &gerrit.Error{Status: http.StatusNotFound}
	}
	return r.fakeChanges.GetChange(ctx, n, o...)
}

// Design §3.1: where the repository is not public, GitHub's checks are
// shown only to those signed in to Gerrit who may see the change, asked
// of Gerrit as them on every request; the answer is theirs alone.
func TestViewerHandler(t *testing.T) {
	five := &gerrit.ChangeInfo{Project: "p", Revisions: map[string]gerrit.RevisionInfo{"g1": {Number: 1}, "g2": {Number: 2}}}
	reads := 0
	sc := sessionChanges{sees: map[string]fakeChanges{"alice-session": {5: five}, "bob-session": {}}, reads: &reads}
	src := &fakeSource{commits: map[string]string{"g2": "c2"}}
	h := &Handler{Project: "p", GitHub: src, Viewer: sc.viewer, AllowOrigin: "https://gerrit.example.com"}
	mux := http.NewServeMux()
	mux.Handle("GET /checks/{change}/{patchset}", h)
	get := func(path, session string) (*httptest.ResponseRecorder, Response) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if session != "" {
			r.Header.Set("Cookie", gerrit.SessionCookie+"="+session)
		}
		mux.ServeHTTP(w, r)
		var resp Response
		json.Unmarshal(w.Body.Bytes(), &resp)
		return w, resp
	}
	if w, _ := get("/checks/5/2", ""); w.Code != http.StatusUnauthorized || reads != 0 {
		t.Errorf("no session: %d, %d reads", w.Code, reads)
	}
	if w, _ := get("/checks/5/2", "x{y}"); w.Code != http.StatusUnauthorized || reads != 0 {
		t.Errorf("not a session: %d, %d reads", w.Code, reads)
	}
	w, resp := get("/checks/5/2", "alice-session")
	if w.Code != 200 || len(resp.Runs) != 2 || resp.Runs[0].CheckName != "build" || resp.Runs[1].CheckName != "GitHub merge" {
		t.Fatalf("alice: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Vary") != "Cookie" {
		t.Errorf("headers %v: want no CORS, and no shared caching", w.Header())
	}
	// Bob may not see it: not even what is cached for alice.
	if w, resp := get("/checks/5/2", "bob-session"); w.Code != http.StatusNotFound || len(resp.Runs) != 0 {
		t.Errorf("bob: %d %s", w.Code, w.Body)
	}
	// Gerrit is asked every time: alice's access can end at any time.
	readsBefore := reads
	sc.sees["alice-session"] = fakeChanges{}
	if w, _ := get("/checks/5/2", "alice-session"); w.Code != http.StatusNotFound || reads != readsBefore+1 {
		t.Errorf("alice, no longer allowed: %d (%d reads)", w.Code, reads-readsBefore)
	}
	if src.calls != 1 {
		t.Errorf("GitHub asked %d times, want once (its answer is the same for all)", src.calls)
	}
	// A patch set GitHub does not have says so.
	sc.sees["alice-session"] = fakeChanges{5: five}
	if w, resp := get("/checks/5/1", "alice-session"); w.Code != 200 || len(resp.Runs) != 1 || resp.Runs[0].CheckName != "GitHub" {
		t.Errorf("not on GitHub: %d %s", w.Code, w.Body)
	}
	// Another project's change, seen or not, is not answered.
	sc.sees["alice-session"] = fakeChanges{6: {Project: "other", Revisions: map[string]gerrit.RevisionInfo{"g2": {Number: 1}}}}
	if w, _ := get("/checks/6/1", "alice-session"); w.Code != http.StatusNotFound {
		t.Errorf("another project's: %d", w.Code)
	}
}

// Gerrit refusing a viewer (a session it no longer takes, say) is not
// seeing the change, not a failure.
func TestViewerRefusedIsNotFound(t *testing.T) {
	h := &Handler{Project: "p", GitHub: &fakeSource{}, Viewer: func(string) Changes { return refusing{} }}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("change", "5")
	r.SetPathValue("patchset", "1")
	r.Header.Set("Cookie", gerrit.SessionCookie+"=s")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("refused: %d", w.Code)
	}
}

type refusing struct{}

func (refusing) GetChange(context.Context, int, ...string) (*gerrit.ChangeInfo, error) {
	return nil, &gerrit.Error{Status: http.StatusForbidden}
}

// Each viewer request reads Gerrit, cached answer or not: one client's
// are bounded, more loosely than anonymous misses (a team behind one
// address polls from each change it has open).
func TestViewerRequestsAreBounded(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reads := 0
	sc := sessionChanges{sees: map[string]fakeChanges{"s": {}}, reads: &reads}
	h := &Handler{Project: "p", GitHub: &fakeSource{}, Viewer: sc.viewer, now: func() time.Time { return now }}
	refused := 0
	for range maxViewerRequestsPerClient + 5 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("change", "5")
		r.SetPathValue("patchset", "1")
		r.Header.Set("Cookie", gerrit.SessionCookie+"=s")
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 5 || reads != maxViewerRequestsPerClient {
		t.Errorf("refused %d, reads %d", refused, reads)
	}
}

// Links come from whoever creates a check run or status: only web links
// reach Gerrit's UI, which makes them links on its own origin.
func TestGitHubLinksAreWebLinks(t *testing.T) {
	bad := []string{"javascript:alert(document.cookie)", " JavaScript:alert(1)", "data:text/html,x", "//evil.example/x", "vbscript:x"}
	for _, u := range bad {
		pr := &github.PullRequest{State: "open", MergeableState: "clean", HTMLURL: u}
		pr.Head.SHA = "c"
		runs := GitHubRuns([]github.CheckRun{{ID: 1, Name: "a", Status: "completed", Conclusion: "failure", HTMLURL: u, DetailsURL: u}},
			[]github.Status{{Context: "s", State: "failure", TargetURL: u}}, pr, "c", 1, 1)
		b, _ := json.Marshal(runs)
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(strings.TrimSpace(u))) {
			t.Errorf("%q reached the runs: %s", u, b)
		}
	}
	runs := GitHubRuns([]github.CheckRun{{ID: 1, Name: "a", Status: "in_progress", HTMLURL: "javascript:x", DetailsURL: "https://ci.example.com/1"}}, nil, nil, "c", 1, 1)
	if runs[0].CheckLink != "https://ci.example.com/1" {
		t.Errorf("fallback to the details link: %q", runs[0].CheckLink)
	}
}
