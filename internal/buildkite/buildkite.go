// Package buildkite reads builds from Buildkite's REST API, so CI results
// can be shown without parsing CI's Gerrit messages.
package buildkite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client lists one pipeline's builds. The token needs only read_builds.
type Client struct {
	// BaseURL is the API root; "" means https://api.buildkite.com.
	BaseURL  string
	Org      string
	Pipeline string
	Token    string
	HTTP     *http.Client
}

// Build is the subset of a Buildkite build the bridge reads.
type Build struct {
	ID         string         `json:"id"`
	Number     int            `json:"number"`
	State      string         `json:"state"`
	WebURL     string         `json:"web_url"`
	Commit     string         `json:"commit"`
	Branch     string         `json:"branch"`
	Env        map[string]any `json:"env"`
	CreatedAt  *time.Time     `json:"created_at"`
	StartedAt  *time.Time     `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
	Jobs       []Job          `json:"jobs"`
	Pipeline   Pipeline       `json:"pipeline"`
}

// Pipeline names a build's pipeline.
type Pipeline struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
}

// Job is one step of a build.
type Job struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"` // script, waiter, manual, trigger
	Name        string     `json:"name"`
	StepKey     string     `json:"step_key"`
	State       string     `json:"state"`
	WebURL      string     `json:"web_url"`
	ExitStatus  *int       `json:"exit_status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// Builds returns the pipeline's builds of commit, newest first.
func (c *Client) Builds(ctx context.Context, commit string) ([]Build, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://api.buildkite.com"
	}
	u := fmt.Sprintf("%s/v2/organizations/%s/pipelines/%s/builds?%s", strings.TrimRight(base, "/"),
		url.PathEscape(c.Org), url.PathEscape(c.Pipeline), url.Values{"commit": {commit}, "per_page": {"30"}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("buildkite: listing builds of %s: %d %s", commit, resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 500)])))
	}
	var out []Build
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("buildkite: decoding builds: %w", err)
	}
	return out, nil
}

// EnvString returns a build env var as a string ("" if unset).
func (b Build) EnvString(k string) string {
	switch v := b.Env[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// Finished reports whether a build or job state is final.
func Finished(state string) bool {
	switch state {
	case "passed", "failed", "canceled", "skipped", "not_run", "broken", "timed_out", "expired":
		return true
	}
	return false
}
