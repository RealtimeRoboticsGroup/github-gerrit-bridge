// Package httplog logs HTTP requests at debug level, with how long each
// took: what the bridge spends its time (and its API budget) on.
package httplog

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

// Transport logs each request it makes through Base (or
// http.DefaultTransport). URLs are logged with their query strings
// (the bridge never puts secrets in URLs).
type Transport struct {
	Base   http.RoundTripper
	Logger *slog.Logger
	Name   string // "gerrit", "github"
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	start := time.Now()
	resp, err := base.RoundTrip(r)
	if tr := trace.From(r.Context()); tr != nil {
		what := t.Name + " " + r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			what += "?" + r.URL.RawQuery
		}
		if resp != nil {
			what += " → " + strconv.Itoa(resp.StatusCode)
		}
		tr.Add(start, time.Since(start), what, err)
	}
	if t.Logger != nil && t.Logger.Enabled(r.Context(), slog.LevelDebug) {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Logger.Debug("http", "to", t.Name, "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
			"status", status, "took", time.Since(start).Round(time.Millisecond), "err", err)
	}
	return resp, err
}
