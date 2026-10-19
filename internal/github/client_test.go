package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestPusher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/brt/fork/activity" || r.URL.Query().Get("ref") != "refs/heads/main" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[
 {"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000","activity_type":"branch_deletion","actor":{"login":"x"}},
 {"ref":"refs/heads/main","after":"bbb","activity_type":"force_push","actor":{"login":"maintainer"}},
 {"ref":"refs/heads/main","after":"aaa","activity_type":"push","actor":{"login":"brt"}}]`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	ctx := context.Background()
	for sha, want := range map[string]string{"aaa": "brt", "bbb": "maintainer", "ccc": ""} {
		if got, err := c.Pusher(ctx, "brt/fork", "refs/heads/main", sha); err != nil || got != want {
			t.Errorf("Pusher(%s) = %q, %v; want %q", sha, got, err, want)
		}
	}
}

// GETs are revalidated with their ETag: an unchanged answer comes back as
// a 304 (free against the rate limit) and is served from the cache, links
// and all; a changed one replaces it.
func TestETagCache(t *testing.T) {
	version, calls, notModified := "v1", 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		etag := `"` + version + `"`
		if r.Header.Get("If-None-Match") == etag {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Link", `<https://api.github.com/next>; rel="next"`)
		fmt.Fprintf(w, `[{"number":1,"title":%q}]`, version)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	ctx := context.Background()
	get := func() (string, string) {
		var out []PullRequest
		resp, err := c.Do(ctx, http.MethodGet, "/repos/o/r/pulls", nil, &out)
		if err != nil || len(out) != 1 {
			t.Fatalf("get: %v %v", out, err)
		}
		return out[0].Title, resp.Header.Get("Link")
	}
	get()
	if title, link := get(); title != "v1" || notModified != 1 || link == "" {
		t.Errorf("cached: %q link %q, %d not-modified", title, link, notModified)
	}
	version = "v2"
	if title, _ := get(); title != "v2" {
		t.Errorf("changed: %q", title)
	}
	if title, _ := get(); title != "v2" || notModified != 2 || calls != 4 {
		t.Errorf("recached: %q, %d not-modified, %d calls", title, notModified, calls)
	}
}

// fakeLists serves PR 1's REST lists with ETags (bump a version to change
// one) and counts GraphQL queries by kind.
type fakeLists struct {
	version map[string]int // list path suffix -> version
	items   map[string]int // list path suffix -> item count
	noETag  bool
	queries map[string]int
}

func (f *fakeLists) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/graphql" {
		b, _ := io.ReadAll(r.Body)
		kind := "edits"
		switch {
		case strings.Contains(string(b), "reviewThreads(first: 100"):
			kind = "threads"
		case strings.Contains(string(b), "reviews(first: 100"):
			kind = "review-edits"
		}
		f.queries[kind]++
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{}}}}`)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/repos/o/r/")
	etag := fmt.Sprintf(`"%s-%d"`, suffix, f.version[suffix])
	if !f.noETag {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
	}
	items := make([]string, f.items[suffix])
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":%d}`, i+1)
	}
	fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
}

// GraphQL answers that REST vouches for are reused while every page of
// the lists behind them is unchanged, and asked again the moment one
// changes; thread resolution, which REST cannot see, is always asked
// unless there are no review comments at all.
func TestGraphQLAnswersVouchedByREST(t *testing.T) {
	f := &fakeLists{version: map[string]int{}, items: map[string]int{"pulls/1/comments": 1}, queries: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	ctx := context.Background()
	call := func() {
		t.Helper()
		if _, err := c.Edits(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReviewEdits(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReviewThreads(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	want := func(edits, reviewEdits, threads int) {
		t.Helper()
		// queryEdits asks twice: issue comments, then review threads.
		if f.queries["edits"] != 2*edits || f.queries["review-edits"] != reviewEdits || f.queries["threads"] != threads {
			t.Errorf("queries = %v, want edits %d, review-edits %d, threads %d", f.queries, edits, reviewEdits, threads)
		}
	}
	call()
	want(1, 1, 1)
	call() // nothing changed: only the threads are asked again
	want(1, 1, 2)
	f.version["issues/1/comments"]++ // an issue comment edited
	call()
	want(2, 1, 3)
	f.version["pulls/1/comments"]++ // a review comment edited
	call()
	want(3, 1, 4)
	f.version["pulls/1/reviews"]++ // a review body edited
	call()
	want(3, 2, 5)
	f.items["pulls/1/comments"] = 0 // no review comments: no threads
	f.version["pulls/1/comments"]++
	call()
	want(4, 2, 5)
}

// The answers kept are bounded: PRs anyone can open each leave some.
func TestGraphQLAnswersBounded(t *testing.T) {
	defer func(m int) { maxAnswers = m }(maxAnswers)
	maxAnswers = 3
	f := &fakeLists{version: map[string]int{}, items: map[string]int{}, queries: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	for pr := 1; pr <= 10; pr++ {
		if _, err := c.Edits(context.Background(), pr); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		n := len(c.answers)
		c.mu.Unlock()
		if n > maxAnswers || n == 0 {
			t.Fatalf("%d answers kept", n)
		}
	}
}

// Without ETags, nothing is vouched for.
func TestGraphQLAnswersWithoutETags(t *testing.T) {
	f := &fakeLists{version: map[string]int{}, items: map[string]int{}, noETag: true, queries: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	for range 2 {
		if _, err := c.Edits(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if f.queries["edits"] != 4 {
		t.Errorf("queries = %v", f.queries)
	}
}

// GitHub's ETag covers a page's body, not its Link header: a full last
// page still answers 304 after a next page appears. Lists must still see
// the new items.
func TestETagCacheSeesNewPages(t *testing.T) {
	items := 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		var b strings.Builder
		b.WriteString("[")
		for i := (page-1)*100 + 1; i <= min(page*100, items); i++ {
			if b.Len() > 1 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d}`, i)
		}
		b.WriteString("]")
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(b.String()))) // body only, as GitHub
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		if page*100 < items {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/1/comments?per_page=100&page=%d>; rel="next"`, "http://"+r.Host, page+1))
		}
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	ctx := context.Background()
	if got, err := c.ListIssueComments(ctx, 1); err != nil || len(got) != 100 {
		t.Fatalf("first: %d %v", len(got), err)
	}
	items = 101
	if got, err := c.ListIssueComments(ctx, 1); err != nil || len(got) != 101 {
		t.Errorf("after comment 101: %d %v", len(got), err)
	}
}

func TestETagCacheBoundedByBytes(t *testing.T) {
	big := strings.Repeat("x", maxCachedBody-100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+r.URL.Path+`"`)
		fmt.Fprintf(w, "%q", big)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	for i := range maxCachedBytes/maxCachedBody + 10 {
		if _, err := c.Do(context.Background(), http.MethodGet, fmt.Sprintf("/x/%d", i), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if c.etagBytes > maxCachedBytes || len(c.etags) != len(c.etagOrder) {
		t.Errorf("cache holds %d bytes in %d entries (%d ordered)", c.etagBytes, len(c.etags), len(c.etagOrder))
	}
}

// An answer computed while a list changed under it is not kept: the next
// call asks again.
func TestGraphQLAnswerRacingAChangeIsNotKept(t *testing.T) {
	f := &fakeLists{version: map[string]int{}, items: map[string]int{}, queries: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" && f.queries["edits"] == 0 {
			f.version["issues/1/comments"]++ // an edit lands while GraphQL is asked
		}
		f.handler(w, r)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("t"), Owner: "o", Repo: "r"}
	for range 2 {
		if _, err := c.Edits(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if f.queries["edits"] != 4 { // two queries each time: not reused
		t.Errorf("queries = %v", f.queries)
	}
}

// The token goes to GitHub only: an absolute URL elsewhere (a Link header
// pointing off-site, say) is refused.
func TestDoSendsTheTokenOnlyToGitHub(t *testing.T) {
	got := ""
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer elsewhere.Close()
	c := &Client{BaseURL: "https://api.github.com", Tokens: StaticToken("secret")}
	if _, err := c.Do(context.Background(), http.MethodGet, elsewhere.URL+"/x", nil, nil); err == nil || got != "" {
		t.Errorf("err = %v, token sent: %q", err, got)
	}
	if !sameOrigin("https://api.github.com/x?page=2", "https://API.github.com") || sameOrigin("http://api.github.com/x", "https://api.github.com") {
		t.Error("sameOrigin")
	}
}
