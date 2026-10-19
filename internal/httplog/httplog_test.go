package httplog

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
