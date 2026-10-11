package buildkite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuilds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/organizations/org/pipelines/ci/builds" || r.URL.Query().Get("commit") != "abc" ||
			r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		w.Write([]byte(`[{"id":"b","number":3,"state":"running","env":{"GERRIT_CHANGE_NUMBER":"12","N":4},
			"jobs":[{"id":"j","type":"script","name":"Build","state":"running","exit_status":null}]}]`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Org: "org", Pipeline: "ci", Token: "tok"}
	bs, err := c.Builds(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Number != 3 || bs[0].EnvString("GERRIT_CHANGE_NUMBER") != "12" || bs[0].EnvString("N") != "4" ||
		len(bs[0].Jobs) != 1 || bs[0].Jobs[0].ExitStatus != nil {
		t.Errorf("builds = %+v", bs)
	}
	if _, err := c.Builds(context.Background(), "other"); err == nil {
		t.Error("no error for a 400")
	}
}
