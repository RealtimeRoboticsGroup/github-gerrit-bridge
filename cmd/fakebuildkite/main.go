// Command fakebuildkite stands in for Buildkite in the test instance, so
// the real gerrit-buildkite can run against the test Gerrit:
//
//   - POST /v2/organizations/{org}/pipelines/{pipeline}/builds creates a
//     build (the only API gerrit-buildkite calls, plus cancel);
//   - after --duration the build finishes and a build.finished webhook goes
//     to --webhook_url with the X-Buildkite-Token header;
//   - GET /builds/{n} shows the build, so "Build Started: <url>" links work.
//
// A build of a Gerrit patch set fails if any file it touches has "FAIL" in
// its path; everything else passes. That makes both outcomes easy to try.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type build struct {
	ID      string            `json:"id"`
	Number  int               `json:"number"`
	WebURL  string            `json:"web_url"`
	URL     string            `json:"url"`
	State   string            `json:"state"`
	Commit  string            `json:"commit"`
	Branch  string            `json:"branch"`
	Message string            `json:"message,omitempty"`
	Created time.Time         `json:"-"`
	Env     map[string]string `json:"-"`
	Log     []string          `json:"-"`
}

type server struct {
	listen     string
	webhookURL string
	token      string
	gerritURL  string
	duration   time.Duration

	mu     sync.Mutex
	builds []*build
}

func main() {
	s := &server{}
	flag.StringVar(&s.listen, "listen", "127.0.0.1:18090", "Address to serve the fake API and build pages on.")
	flag.StringVar(&s.webhookURL, "webhook_url", "http://127.0.0.1:10005/", "gerrit-buildkite's webhook listener.")
	flag.StringVar(&s.token, "webhook_token", "test-webhook-token", "Sent as X-Buildkite-Token.")
	flag.StringVar(&s.gerritURL, "gerrit_url", "http://127.0.0.1:18080", "Gerrit, for listing a patch set's files.")
	flag.DurationVar(&s.duration, "duration", 20*time.Second, "How long a build takes.")
	flag.Parse()

	http.HandleFunc("POST /v2/organizations/{org}/pipelines/{pipeline}/builds", s.create)
	http.HandleFunc("PUT /v2/organizations/{org}/pipelines/{pipeline}/builds/{n}/cancel", s.cancel)
	http.HandleFunc("GET /builds/{n}", s.page)
	http.HandleFunc("GET /{$}", s.index)
	log.Printf("fake Buildkite on http://%s, webhooks to %s", s.listen, s.webhookURL)
	log.Fatal(http.ListenAndServe(s.listen, nil))
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Commit  string            `json:"commit"`
		Branch  string            `json:"branch"`
		Message string            `json:"message"`
		Env     map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	b := &build{ID: uuid(), Number: len(s.builds) + 1, State: "scheduled", Commit: in.Commit, Branch: in.Branch,
		Message: in.Message, Created: time.Now(), Env: in.Env}
	b.WebURL = fmt.Sprintf("http://%s/builds/%d", s.listen, b.Number)
	b.URL = b.WebURL
	s.builds = append(s.builds, b)
	s.mu.Unlock()
	log.Printf("build #%d created: commit %s branch %s env %v", b.Number, b.Commit, b.Branch, b.Env)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(b)
	go s.run(b)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.builds {
		if fmt.Sprint(b.Number) == r.PathValue("n") && b.State == "running" {
			b.State = "canceled"
			b.Log = append(b.Log, "Canceled (a newer patch set was uploaded).")
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "{}")
}

func (s *server) run(b *build) {
	s.mu.Lock()
	b.State = "running"
	s.mu.Unlock()
	time.Sleep(s.duration)
	state, lines := s.verdict(b)
	s.mu.Lock()
	if b.State == "canceled" {
		s.mu.Unlock()
		return
	}
	b.State = state
	b.Log = append(b.Log, lines...)
	payload, _ := json.Marshal(map[string]any{"event": "build.finished", "build": b})
	s.mu.Unlock()
	log.Printf("build #%d %s", b.Number, state)
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, s.webhookURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Buildkite-Token", s.token)
		req.Header.Set("X-Buildkite-Event", "build.finished")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		log.Printf("webhook for build #%d failed: %v; retrying", b.Number, err)
		time.Sleep(5 * time.Second)
	}
}

// verdict "runs" the build: fail if a touched file has FAIL in its path.
func (s *server) verdict(b *build) (string, []string) {
	change, ps := b.Env["GERRIT_CHANGE_NUMBER"], b.Env["GERRIT_PATCH_NUMBER"]
	if change == "" {
		return "passed", []string{"Branch build of " + b.Branch + ": nothing to check, passed."}
	}
	resp, err := http.Get(fmt.Sprintf("%s/changes/%s/revisions/%s/files/", s.gerritURL, url.PathEscape(change), url.PathEscape(ps)))
	if err != nil {
		return "failed", []string{"Could not list files: " + err.Error()}
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	var files map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(buf.Bytes(), []byte(")]}'")), &files); err != nil {
		return "failed", []string{fmt.Sprintf("Could not list files (HTTP %d): %v", resp.StatusCode, err)}
	}
	lines := []string{fmt.Sprintf("Checked change %s patch set %s:", change, ps)}
	state := "passed"
	for f := range files {
		if f == "/COMMIT_MSG" {
			continue
		}
		verdict := "ok"
		if strings.Contains(f, "FAIL") {
			verdict, state = "FAIL (path contains FAIL)", "failed"
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", f, verdict))
	}
	return state, lines
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.builds {
		if fmt.Sprint(b.Number) == r.PathValue("n") {
			fmt.Fprintf(w, "<!doctype html><title>Build #%d</title><h1>Build #%d: %s</h1><p>Commit %s, branch %s, created %s</p><pre>%s</pre>",
				b.Number, b.Number, html.EscapeString(b.State), html.EscapeString(b.Commit), html.EscapeString(b.Branch),
				b.Created.Format(time.RFC3339), html.EscapeString(strings.Join(b.Log, "\n")))
			return
		}
	}
	http.NotFound(w, r)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(w, "<!doctype html><title>Fake Buildkite</title><h1>Builds</h1><ul>")
	for i := len(s.builds) - 1; i >= 0; i-- {
		b := s.builds[i]
		fmt.Fprintf(w, `<li><a href="/builds/%d">#%d</a> %s %s</li>`, b.Number, b.Number, html.EscapeString(b.State), html.EscapeString(b.Commit))
	}
	fmt.Fprint(w, "</ul>")
}
