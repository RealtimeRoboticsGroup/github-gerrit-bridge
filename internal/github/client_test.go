package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Owner: "o", Repo: "r", Tokens: StaticToken("tok")}
}

func TestDoHeadersAndDecode(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-GitHub-Api-Version") == "" ||
			r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("headers: %v", r.Header)
		}
		if r.URL.Path != "/repos/o/r/pulls/4" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4997")
		w.Header().Set("X-RateLimit-Reset", "1791098302")
		w.Header().Set("X-RateLimit-Resource", "core")
		// Shape captured from the real API (AustinSchuh/bridge-scratch).
		io.WriteString(w, `{"base":{"ref":"layer2"},"head":{"ref":"layer3","sha":"b166"},"node_id":"PR_kw","number":4,
"stack":{"base":{"ref":"main","sha":"fe25"},"id":1725048,"number":6,"position":3,"size":3},"state":"open","extra":1}`)
	})
	pr, err := c.GetPull(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 4 || pr.Base.Ref != "layer2" || pr.Stack == nil || pr.Stack.Number != 6 || pr.Stack.Position != 3 {
		t.Errorf("pr = %+v stack %+v", pr, pr.Stack)
	}
	if rl := c.RateLimit(); rl.Remaining != 4997 || rl.Limit != 5000 || rl.Reset.Unix() != 1791098302 {
		t.Errorf("rate = %+v", rl)
	}
}

func TestPagination(t *testing.T) {
	var srvURL string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("state") != "open" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		switch page {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?state=open&per_page=100&page=2>; rel="next", <%s/x?page=3>; rel="last"`, srvURL, srvURL))
			io.WriteString(w, `[{"number":1},{"number":2}]`)
		case "2":
			io.WriteString(w, `[{"number":3}]`)
		}
	})
	srvURL = c.BaseURL
	prs, err := c.ListPulls(context.Background(), "open")
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range prs {
		nums = append(nums, p.Number)
	}
	if !reflect.DeepEqual(nums, []int{1, 2, 3}) {
		t.Errorf("numbers = %v", nums)
	}
}

func TestAPIErrors(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/404":
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		case "/repos/o/r/pulls":
			w.WriteHeader(422)
			io.WriteString(w, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists"}]}`)
		case "/repos/o/r/pulls/1":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(30*time.Second).Unix()))
			w.WriteHeader(403)
			io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		case "/repos/o/r/pulls/2":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		}
	})
	ctx := context.Background()
	if _, err := c.GetPull(ctx, 404); !IsNotFound(err) {
		t.Errorf("404: %v", err)
	}
	_, err := c.CreatePull(ctx, NewPull{Title: "t", Head: "h", Base: "b"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 422 || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("422: %v", err)
	}
	_, err = c.GetPull(ctx, 1)
	if !errors.As(err, &ae) || ae.RetryAfter < 20*time.Second || ae.RetryAfter > 31*time.Second {
		t.Errorf("primary rate limit: %v (%v)", err, ae.RetryAfter)
	}
	_, err = c.GetPull(ctx, 2)
	if !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second {
		t.Errorf("secondary rate limit: %v", err)
	}
}

func TestStacks(t *testing.T) {
	var got []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.URL.Path == "/repos/o/r/stacks/6/unstack":
			w.WriteHeader(204)
		case r.Method == "POST":
			var in stackPRs
			json.Unmarshal(b, &in)
			var prs []string
			for _, n := range in.PullRequests {
				prs = append(prs, fmt.Sprintf(`{"number":%d,"state":"open","head":{"ref":"l%d"}}`, n, n))
			}
			fmt.Fprintf(w, `{"id":1,"number":7,"base":{"ref":"main"},"open":true,"pull_requests":[%s]}`, strings.Join(prs, ","))
		default:
			io.WriteString(w, `[{"id":1725048,"number":6,"node_id":"PRS_kw","base":{"ref":"main"},"open":true,"created_at":"2026-10-04T05:11:02Z",
"pull_requests":[{"number":1,"state":"open","draft":false,"merged_at":null,"head":{"ref":"layer1","sha":"9d4c"}},
{"number":2,"state":"open","head":{"ref":"layer2","sha":"190a"}}]}]`)
		}
	})
	ctx := context.Background()
	stacks, err := c.ListStacks(ctx)
	if err != nil || len(stacks) != 1 || !reflect.DeepEqual(stacks[0].Numbers(), []int{1, 2}) || stacks[0].Base.Ref != "main" {
		t.Fatalf("ListStacks = %+v, %v", stacks, err)
	}
	s, err := c.CreateStack(ctx, []int{1, 2, 4})
	if err != nil || s.Number != 7 || !reflect.DeepEqual(s.Numbers(), []int{1, 2, 4}) {
		t.Errorf("CreateStack = %+v, %v", s, err)
	}
	if _, err := c.AddToStack(ctx, 7, []int{5}); err != nil {
		t.Error(err)
	}
	if err := c.Unstack(ctx, 6); err != nil {
		t.Error(err)
	}
	want := []string{
		"GET /repos/o/r/stacks ",
		`POST /repos/o/r/stacks {"pull_requests":[1,2,4]}`,
		`POST /repos/o/r/stacks/7/add {"pull_requests":[5]}`,
		"POST /repos/o/r/stacks/6/unstack ",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requests:\n%q\nwant\n%q", got, want)
	}
}

func TestUpdatePullOmitsUnset(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "PATCH" || string(b) != `{"base":"main"}` {
			t.Errorf("%s %s", r.Method, b)
		}
		io.WriteString(w, `{"number":3}`)
	})
	base := "main"
	if _, err := c.UpdatePull(context.Background(), 3, PullUpdate{Base: &base}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandToken(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	script := filepath.Join(dir, "tok.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+counter+"\necho '  ghp_abc  '\n"), 0o755)
	ct := &CommandToken{Command: []string{script}, TTL: time.Hour}
	for range 3 {
		if tok, err := ct.Token(context.Background()); err != nil || tok != "ghp_abc" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	ct.Invalidate()
	ct.Token(context.Background())
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Errorf("command ran %d times, want 2 (cache + invalidate)", n)
	}

	bad := &CommandToken{Command: []string{"/bin/sh", "-c", "echo nope >&2; exit 3"}}
	if _, err := bad.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
}

func TestUnauthorizedInvalidatesToken(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	script := filepath.Join(dir, "tok.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+counter+"\necho t\n"), 0o755)
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"Bad credentials"}`)
	})
	c.Tokens = &CommandToken{Command: []string{script}, TTL: time.Hour}
	c.GetRepo(context.Background())
	c.GetRepo(context.Background())
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Errorf("token fetched %d times, want 2", n)
	}
}

func TestNextLink(t *testing.T) {
	for in, want := range map[string]string{
		``: ``,
		`<https://a/x?page=2>; rel="next", <https://a/x?page=5>; rel="last"`:  `https://a/x?page=2`,
		`<https://a/x?page=1>; rel="prev", <https://a/x?page=1>; rel="first"`: ``,
	} {
		if got := nextLink(in); got != want {
			t.Errorf("nextLink(%q) = %q", in, got)
		}
	}
}

func TestGraphQLAndDraft(t *testing.T) {
	var queries []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		queries = append(queries, in.Query)
		if in.Variables["id"] == "bad" {
			io.WriteString(w, `{"data":null,"errors":[{"message":"Could not resolve to a node"}]}`)
			return
		}
		io.WriteString(w, `{"data":{"x":{"pullRequest":{"isDraft":true}}}}`)
	})
	ctx := context.Background()
	if err := c.SetDraft(ctx, "PR_1", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDraft(ctx, "PR_1", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queries[0], "convertPullRequestToDraft") || !strings.Contains(queries[1], "markPullRequestReadyForReview") {
		t.Errorf("queries = %q", queries)
	}
	var ge *GraphQLError
	if err := c.SetDraft(ctx, "bad", true); !errors.As(err, &ge) || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("err = %v", err)
	}
}
