package httplog

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

func TestTransportLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	var buf bytes.Buffer
	c := &http.Client{Transport: &Transport{Name: "github", Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	resp, err := c.Get(srv.URL + "/repos/o/r/pulls?state=open")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, want := range []string{"to=github", "method=GET", "path=/repos/o/r/pulls", `query="state=open"`, "status=204", "took="} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q lacks %q", buf.String(), want)
		}
	}
}

// A request in a run's context is a span of its trace, whatever the log
// level: path, query and status, never headers.
func TestTransportTraces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) }))
	defer srv.Close()
	c := &http.Client{Transport: &Transport{Name: "github"}}
	tr := trace.New()
	req, _ := http.NewRequestWithContext(trace.With(context.Background(), tr), "GET", srv.URL+"/repos/o/r/pulls/7?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	spans, _ := tr.Spans()
	if len(spans) != 1 || spans[0].What != "github GET /repos/o/r/pulls/7?per_page=100 → 304" || spans[0].Took <= 0 {
		t.Errorf("spans %+v", spans)
	}
}
