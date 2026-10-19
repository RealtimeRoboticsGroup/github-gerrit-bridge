// Command fakebuildkite stands in for Buildkite in the test instance, so
// the real gerrit-buildkite can run against the test Gerrit:
//
//   - POST /v2/organizations/{org}/pipelines/{pipeline}/builds creates a
//     build (the only API gerrit-buildkite calls, plus cancel);
//   - after --duration the build finishes and a build.finished webhook goes
//     to --webhook_url with the X-Buildkite-Token header;
//   - GET /v2/organizations/{org}/pipelines/{pipeline}/builds?commit=
//     lists builds with their jobs, as the bridge reads them;
//   - GET /builds/{n} shows the build, so "Build Started: <url>" links work.
//
// Each build has two jobs, "Build" and then "Test", each taking half of
// --duration. On a Gerrit patch set, Test fails if any file the change
// touches has "FAIL" in its path; everything else passes. That makes both
// outcomes easy to try.
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
	"os"
	"strings"
	"sync"
	"time"
)

type build struct {
	ID         string            `json:"id"`
	Number     int               `json:"number"`
	WebURL     string            `json:"web_url"`
	URL        string            `json:"url"`
	State      string            `json:"state"`
	Commit     string            `json:"commit"`
	Branch     string            `json:"branch"`
	Message    string            `json:"message,omitempty"`
	Env        map[string]string `json:"env"`
	Created    time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at"`
	Jobs       []*job            `json:"jobs"`
	Pipeline   pipeline          `json:"pipeline"`
	Log        []string          `json:"-"`
}

type job struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	StepKey     string     `json:"step_key"`
	State       string     `json:"state"`
	WebURL      string     `json:"web_url"`
	ExitStatus  *int       `json:"exit_status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

type pipeline struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
}

type server struct {
	listen     string
	webhookURL string
	token      string
	gerritURL  string
	// gerritUser and gerritPassword, if set, read Gerrit as that account,
	// as a real CI agent would: AOS lets no anonymous user read changes.
	gerritUser, gerritPassword string
	duration                   time.Duration

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
	flag.StringVar(&s.gerritUser, "gerrit_user", "", "Gerrit account to read as (with --gerrit_password_file).")
	passwordFile := flag.String("gerrit_password_file", "", "File holding --gerrit_user's HTTP password.")
	flag.Parse()
	if *passwordFile != "" {
		b, err := os.ReadFile(*passwordFile)
		if err != nil {
			log.Fatal(err)
		}
		s.gerritPassword = strings.TrimSpace(string(b))
	}

	http.HandleFunc("POST /v2/organizations/{org}/pipelines/{pipeline}/builds", s.create)
	http.HandleFunc("PUT /v2/organizations/{org}/pipelines/{pipeline}/builds/{n}/cancel", s.cancel)
	http.HandleFunc("GET /v2/organizations/{org}/pipelines/{pipeline}/builds", s.list)
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
	now := time.Now()
	b := &build{ID: uuid(), Number: len(s.builds) + 1, State: "scheduled", Commit: in.Commit, Branch: in.Branch,
		Message: in.Message, Created: now, Env: in.Env,
		Pipeline: pipeline{Slug: r.PathValue("pipeline"), Name: r.PathValue("pipeline"), WebURL: fmt.Sprintf("http://%s/", s.listen)}}
	b.WebURL = fmt.Sprintf("http://%s/builds/%d", s.listen, b.Number)
	b.URL = b.WebURL
	for _, name := range []string{"Build", "Test"} {
		j := &job{ID: uuid(), Type: "script", Name: name, StepKey: strings.ToLower(name), State: "scheduled", ScheduledAt: &now}
		j.WebURL = b.WebURL + "#" + j.ID
		b.Jobs = append(b.Jobs, j)
	}
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
		if fmt.Sprint(b.Number) == r.PathValue("n") && !finished(b.State) {
			b.State, b.FinishedAt = "canceled", stamp()
			for _, j := range b.Jobs {
				if !finished(j.State) {
					j.State, j.FinishedAt = "canceled", b.FinishedAt
				}
			}
			b.Log = append(b.Log, "Canceled (a newer patch set was uploaded).")
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "{}")
}

// list serves the builds of ?commit=, newest first.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, `{"message":"Authentication required"}`, http.StatusUnauthorized)
		return
	}
	commit := r.URL.Query().Get("commit")
	s.mu.Lock()
	out := []*build{}
	for i := len(s.builds) - 1; i >= 0; i-- {
		if b := s.builds[i]; commit == "" || b.Commit == commit {
			out = append(out, b)
		}
	}
	body, _ := json.Marshal(out)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func stamp() *time.Time {
	t := time.Now()
	return &t
}

func (s *server) run(b *build) {
	s.mu.Lock()
	b.State, b.StartedAt = "running", stamp()
	b.Jobs[0].State, b.Jobs[0].StartedAt = "running", b.StartedAt
	s.mu.Unlock()
	time.Sleep(s.duration / 2)
	state, lines := s.verdict(b)
	s.mu.Lock()
	if b.State == "canceled" {
		s.mu.Unlock()
		return
	}
	zero, one := 0, 1
	b.Jobs[0].State, b.Jobs[0].FinishedAt, b.Jobs[0].ExitStatus = "passed", stamp(), &zero
	b.Jobs[1].State, b.Jobs[1].StartedAt = "running", stamp()
	s.mu.Unlock()
	time.Sleep(s.duration / 2)
	s.mu.Lock()
	if b.State == "canceled" {
		s.mu.Unlock()
		return
	}
	b.Jobs[1].State, b.Jobs[1].FinishedAt, b.Jobs[1].ExitStatus = state, stamp(), &zero
	if state != "passed" {
		b.Jobs[1].ExitStatus = &one
	}
	b.State, b.FinishedAt = state, stamp()
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

func finished(state string) bool {
	return state == "passed" || state == "failed" || state == "canceled"
}

// verdict "runs" the build: fail if a touched file has FAIL in its path.
func (s *server) verdict(b *build) (string, []string) {
	change, ps := b.Env["GERRIT_CHANGE_NUMBER"], b.Env["GERRIT_PATCH_NUMBER"]
	if change == "" {
		return "passed", []string{"Branch build of " + b.Branch + ": nothing to check, passed."}
	}
	base := s.gerritURL
	if s.gerritUser != "" {
		base += "/a"
	}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/changes/%s/revisions/%s/files/", base, url.PathEscape(change), url.PathEscape(ps)), nil)
	if s.gerritUser != "" {
		req.SetBasicAuth(s.gerritUser, s.gerritPassword)
	}
	resp, err := http.DefaultClient.Do(req)
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
			fmt.Fprintf(w, "<!doctype html><title>Build #%d</title><h1>Build #%d: %s</h1><p>Commit %s, branch %s, created %s</p><ul>",
				b.Number, b.Number, html.EscapeString(b.State), html.EscapeString(b.Commit), html.EscapeString(b.Branch),
				b.Created.Format(time.RFC3339))
			for _, j := range b.Jobs {
				fmt.Fprintf(w, `<li id="%s">%s: %s</li>`, j.ID, html.EscapeString(j.Name), html.EscapeString(j.State))
			}
			fmt.Fprintf(w, "</ul><pre>%s</pre>", html.EscapeString(strings.Join(b.Log, "\n")))
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
