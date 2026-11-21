package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A review thread's comments come at most 100 at a time, as every GraphQL
// connection's do: Edits still says who edited the 101st, or someone
// else's edit there is mirrored to Gerrit as its author's own (sixteenth
// review).
func TestEditsSeesThreadCommentsPastTheFirst100(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			fmt.Fprint(w, "[]") // the REST lists that vouch for it
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		var nodes []string
		for i := 1; i <= 100; i++ {
			nodes = append(nodes, fmt.Sprintf(`{"databaseId":%d,"lastEditedAt":null,"editor":null}`, i))
		}
		first := `{"pageInfo":{"hasNextPage":true,"endCursor":"c100"},"nodes":[` + strings.Join(nodes, ",") + `]}`
		switch {
		case strings.Contains(req.Query, "node(id:"):
			if req.Variables["id"] != "T1" || req.Variables["after"] != "c100" {
				t.Errorf("asked for the rest with %v", req.Variables)
			}
			fmt.Fprint(w, `{"data":{"node":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":"c101"},"nodes":[{"databaseId":101,"lastEditedAt":"2026-11-21T00:00:00Z","editor":{"login":"mallory"}}]}}}}`)
		case strings.Contains(req.Query, "reviewThreads("):
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"T1","comments":%s}]}}}}}`, first)
		default:
			fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	edits, err := c.Edits(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if editor, ok := edits[101]; !ok || editor != "mallory" {
		t.Errorf("comment 101, edited by mallory: edits say %q (%v)", editor, ok)
	}
}
