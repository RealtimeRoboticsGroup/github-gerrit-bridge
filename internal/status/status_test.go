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
			Waiting: []queue.Entry[string]{{Key: "comments/12", At: now.Add(-2 * time.Minute)}, {Key: "branch/main", At: now.Add(-time.Minute), Resync: true}},
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
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		"Running (1)", `<a href="https://github.com/o/r/pull/7">pr/7</a>`, "1m30s", "3s",
		"Waiting (2)", "comments/12", "2m0s", "resync",
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
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
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
