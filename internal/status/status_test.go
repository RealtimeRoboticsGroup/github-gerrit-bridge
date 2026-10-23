package status

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
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
