package status

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

type fixed queue.Snapshot[string]

func (f fixed) Snapshot() queue.Snapshot[string] { return queue.Snapshot[string](f) }

// The page lists each section's keys, linked, with how long they have
// been there, and escapes what it shows (an error can quote a stranger's
// text).
func TestPage(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h := &Handler[string]{
		Queue: fixed{
			Running: []queue.Entry[string]{{Key: "pr/7", At: now.Add(-90 * time.Second), Waited: 3 * time.Second}},
			Waiting: []queue.Entry[string]{{Key: "comments/12", At: now.Add(-2 * time.Minute)}, {Key: "branch/main", At: now.Add(-time.Minute), Resync: "periodic"}},
			Held:    []queue.Entry[string]{{Key: "pr/7", At: now.Add(-10 * time.Second), Again: true}},
			Failing: []queue.Entry[string]{{Key: "pr/9", Failures: 3, Err: `422 <script>alert(1)</script>`}},
			Recent: []queue.Entry[string]{{Key: "pr/8", At: now.Add(-5 * time.Second), Took: 1200 * time.Millisecond},
				{Key: "pr/9", At: now.Add(-6 * time.Second), Waited: 300 * time.Microsecond, Took: 40 * time.Microsecond}},
		},
		Name: func(k string) string { return k },
		Link: func(k string) string {
			if strings.HasPrefix(k, "pr/") {
				return "https://github.com/o/r/pull/" + strings.TrimPrefix(k, "pr/")
			}
			return ""
		},
		Facts: func() []Fact { return []Fact{{Name: "GitHub API", Value: "4000 of 5000 left"}} },
		Now:   func() time.Time { return now },
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, get("/"))
	body := w.Body.String()
	for _, want := range []string{
		"Running (1)", `<a href="https://github.com/o/r/pull/7">pr/7</a>`, "1m30s", "3s",
		"Waiting (2)", "comments/12", "2m0s", "resync (periodic)",
		"Held (1)", "runs again after this run",
		"Failing (1)", "&lt;script&gt;", ">3<",
		"Recently done (2)", "1.2s", "&lt;1ms",
		"GitHub API", "4000 of 5000 left",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("an error's text was not escaped")
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("CSP %q", got)
	}
}

// Each running or recent run with a trace links its timeline, which shows
// its spans in order, nested, with where the time went; others are not
// found.
func TestRunTimeline(t *testing.T) {
	now := time.Now()
	tr := trace.New()
	ctx := trace.With(context.Background(), tr)
	wait := trace.Region(ctx, "wait for branch main, shared")
	wait()
	end := trace.Region(ctx, "comments")
	tr.Add(time.Now(), 200*time.Millisecond, "github GET /repos/o/r/pulls/7/comments → 304", nil)
	tr.Add(time.Now(), 800*time.Millisecond, "github POST /repos/o/r/statuses/abc → 201", nil)
	tr.Add(time.Now(), time.Millisecond, "git fetch <script>", errors.New("exit status 1"))
	end()
	h := &Handler[string]{
		Queue: fixed{Recent: []queue.Entry[string]{
			{Key: "comments/7", ID: 42, At: now, Took: 2 * time.Second, Detail: tr},
			{Key: "pr/8", ID: 43, At: now},
		}},
		Name: func(k string) string { return k },
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, get(path))
		return w
	}
	if body := get("/").Body.String(); !strings.Contains(body, `href="run/42"`) || strings.Contains(body, `href="run/43"`) {
		t.Errorf("overview links: %s", body)
	}
	w := get("/run/42")
	body := w.Body.String()
	for _, want := range []string{"comments/7", "took 2s", "wait for branch main, shared", "pulls/7/comments → 304",
		"GitHub reads", "GitHub writes", "800ms", "git commands", "exit status 1", "&lt;script&gt;", `class="bar wait"`, "padding-left:14px"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("a span was not escaped")
	}
	if i, j := strings.Index(body, "pulls/7/comments"), strings.Index(body, "statuses/abc"); i < 0 || j < i {
		t.Error("spans out of order")
	}
	for _, path := range []string{"/run/43", "/run/99", "/other"} {
		if w := get(path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

// get asks for path as the operator's browser does, through a tunnel.
func get(path string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:18082"
	return r
}

// A page that rebinds its own name to 127.0.0.1 sends that name.
func TestOnlyLoopbackHosts(t *testing.T) {
	h := &Handler[string]{Queue: queue.New[string](), Name: func(k string) string { return k }}
	for host, ok := range map[string]bool{"127.0.0.1:18082": true, "[::1]:18082": true, "localhost:18082": true, "LOCALHOST": true, "127.0.0.1": true,
		"attacker.example:18082": false, "attacker.example": false, "192.0.2.1:18082": false, "[::1]": true, "127.0.0.1.attacker.example:18082": false, "": false} {
		for _, path := range []string{"/", "/run/1"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Code != http.StatusMisdirectedRequest; got != ok {
				t.Errorf("Host %q %s: %d", host, path, w.Code)
			}
		}
	}
}

// It is read only.
func TestOnlyReads(t *testing.T) {
	h := &Handler[string]{Queue: queue.New[string](), Name: func(k string) string { return k }}
	for method, ok := range map[string]bool{"GET": true, "HEAD": true, "POST": false, "PUT": false, "DELETE": false} {
		r := get("/")
		r.Method = method
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Code != http.StatusMethodNotAllowed; got != ok {
			t.Errorf("%s: %d", method, w.Code)
		}
	}
}

// The top of the page sums up what resyncs caught, linking the details:
// how many runs each kind of resync made and how many caught something,
// and what those wrote.
func TestResyncs(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rs := Resyncs[string]{Since: now.Add(-2 * time.Hour)}
	h := &Handler[string]{
		Queue:   fixed{},
		Name:    func(k string) string { return k },
		Link:    func(k string) string { return "https://example.test/" + k },
		Now:     func() time.Time { return now },
		Resyncs: func() Resyncs[string] { return rs },
	}
	page := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, get(path))
		return w.Body.String()
	}
	if body := page("/"); !strings.Contains(body, `<a href="resyncs">0 runs, 0 caught something</a>`) ||
		!strings.Contains(body, `<a href="resyncs">never, in 2h0m0s since start</a>`) {
		t.Errorf("empty tally's summary: %s", body)
	}
	if body := page("/resyncs"); !strings.Contains(body, "Since 2h0m0s ago") || !strings.Contains(body, "No resync runs yet.") {
		t.Errorf("empty tally: %s", body)
	}
	rs.Counts = []ResyncCount{{Reason: "periodic", Key: "branch", Runs: 40, Caught: 1, Failed: 2},
		{Reason: "connected", Key: "comments", Runs: 5, Caught: 1}}
	rs.Caught = []Caught[string]{
		{Key: "branch/main", At: now.Add(-time.Minute), Reason: "periodic", Writes: []string{"github PATCH /repos/o/r/pulls/3", "<b>x</b>"}, N: 5},
		{Key: "comments/4", At: now.Add(-time.Hour), Reason: "connected", Writes: []string{"gerrit POST x"}, N: 1},
	}
	if body := page("/"); !strings.Contains(body, ">45 runs, 2 caught something<") || !strings.Contains(body, ">1m0s ago<") ||
		strings.Contains(body, "github PATCH") {
		t.Errorf("summary: %s", body)
	}
	body := page("/resyncs")
	for _, want := range []string{
		"<td>periodic</td><td>branch</td>", ">40<", ">1<", "2.5%", ">2<",
		`<a href="https://example.test/branch/main">branch/main</a>`, "1m0s ago",
		"github PATCH /repos/o/r/pulls/3", "&lt;b&gt;x&lt;/b&gt;", "and 3 more",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("details lack %q", want)
		}
	}
	h.Resyncs = nil
	if strings.Contains(page("/"), "Resyncs") {
		t.Error("a summary with no tally")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, get("/resyncs"))
	if w.Code != http.StatusNotFound {
		t.Errorf("details with no tally: %d", w.Code)
	}
}
