package gerrit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

func TestQueryChangesPaging(t *testing.T) {
	const total = 5
	const pageSize = 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bridge" || p != "secret" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/a/changes/" {
			http.NotFound(w, r)
			return
		}
		if q := r.URL.Query().Get("q"); q != `project:"a/b" status:open` {
			t.Errorf("q = %q", q)
		}
		if o := r.URL.Query()["o"]; len(o) != 1 || o[0] != "CURRENT_REVISION" {
			t.Errorf("o = %q", o)
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("S"))
		fmt.Fprint(w, ")]}'\n[")
		for i := start; i < min(start+pageSize, total); i++ {
			if i > start {
				fmt.Fprint(w, ",")
			}
			more := ""
			if i == start+pageSize-1 && i < total-1 {
				more = `,"_more_changes":true`
			}
			fmt.Fprintf(w, `{"_number":%d,"change_id":"I%d","status":"NEW","unknown":1%s}`, 100+i, i, more)
		}
		fmt.Fprint(w, "]")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	got, err := c.QueryChanges(context.Background(), "project:"+QueryString("a/b")+" status:open", "CURRENT_REVISION")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("got %d changes, want %d", len(got), total)
	}
	for i, ch := range got {
		if ch.Number != 100+i || ch.MoreChanges {
			t.Errorf("change %d = %+v", i, ch)
		}
	}
}

func TestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not found: 99", http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	err := c.Do(context.Background(), http.MethodGet, "changes/99", nil, &struct{}{})
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 404 || ge.Body != "Not found: 99" {
		t.Fatalf("err = %#v", err)
	}
}

func TestQueryString(t *testing.T) {
	if got := QueryString(`a "b" \c`); got != `"a \"b\" \\c"` {
		t.Errorf("QueryString = %s", got)
	}
}

func TestRunAsRefusesNamesGerritReadsAsOtherAccounts(t *testing.T) {
	var runAs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runAs = append(runAs, r.Header.Get("X-Gerrit-RunAs"))
		fmt.Fprint(w, ")]}'\n{}")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	ctx := context.Background()
	for _, user := range []string{"1000000", "self", "Me"} {
		if err := c.As(user).Do(ctx, http.MethodGet, "accounts/self", nil, nil); err == nil {
			t.Errorf("ran as %q", user)
		}
	}
	if err := c.As("alice").Do(ctx, http.MethodGet, "accounts/self", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(runAs) != 1 || runAs[0] != "alice" {
		t.Errorf("requests ran as %q", runAs)
	}
}

func TestRegistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/accounts/1000001/detail" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, ")]}'\n"+`{"_account_id":1000001,"registered_on":"2026-10-01 12:34:56.000000000"}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	got, err := c.Registered(context.Background(), 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 12, 34, 56, 0, time.UTC); !got.Equal(want) {
		t.Errorf("registered = %v, want %v", got, want)
	}
}

func TestSubmitNamesTheRevision(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		fmt.Fprint(w, ")]}'\n{}")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	if _, err := c.Submit(context.Background(), 5, "abc123"); err != nil {
		t.Fatal(err)
	}
	if path != "POST /a/changes/5/revisions/abc123/submit" {
		t.Errorf("submitted %s", path)
	}
}

func TestFileContentIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("A"), (maxFileContent*4/3+100)/4*4)) // valid base64
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	if _, err := c.FileContent(context.Background(), 5, "abc", "big.bin"); err == nil {
		t.Error("read a file larger than the bound")
	}
}

// A write Gerrit takes is noted in the run's trace, with whom it ran as;
// reads and refused writes are not.
func TestWritesAreTraced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a/changes/6/revisions/abc/review" {
			http.Error(w, "blocked", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, ")]}'\n{}")
	}))
	defer srv.Close()
	tr := trace.New()
	ctx := trace.With(context.Background(), tr)
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	if err := c.Do(ctx, http.MethodGet, "changes/5", nil, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, "changes/6/revisions/abc/review", struct{}{}, nil); err == nil {
		t.Fatal("refused review: no error")
	}
	if w, n := tr.Writes(); n != 0 {
		t.Fatalf("a read and a refused write traced as writes: %q", w)
	}
	alice := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret", RunAs: "alice"}
	if err := alice.Do(ctx, http.MethodPost, "changes/5/revisions/abc/review", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if w, n := tr.Writes(); n != 1 || w[0] != "gerrit POST changes/5/revisions/abc/review as alice" {
		t.Errorf("writes %q", w)
	}
}

// A viewer's reads carry their session and nothing of the bridge's, at
// Gerrit's unauthenticated paths; and nothing is written as them.
func TestViewerReadsAsTheViewer(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, ")]}'\n{\"_number\":5}")
	}))
	defer srv.Close()
	bridge := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret", RunAs: "alice"}
	v := bridge.Viewer("sess1")
	if _, err := v.GetChange(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/changes/5" {
		t.Errorf("path %s, want Gerrit's unauthenticated one", got.URL.Path)
	}
	if c, err := got.Cookie(SessionCookie); err != nil || c.Value != "sess1" {
		t.Errorf("session cookie %v %v", c, err)
	}
	if _, _, ok := got.BasicAuth(); ok || got.Header.Get("X-Gerrit-RunAs") != "" {
		t.Errorf("the bridge's credentials went along: %v", got.Header)
	}
	got = nil
	if err := v.Do(context.Background(), http.MethodPost, "changes/5/abandon", nil, nil); err == nil || got != nil {
		t.Errorf("a write as the viewer: %v (sent %v)", err, got != nil)
	}
}

// Gerrit reads an unknown session as anonymous, who has no account.
func TestSignedIn(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/self" {
			t.Errorf("asked %s", r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, ")]}'\n{\"_account_id\":7}")
	}))
	defer srv.Close()
	v := (&Client{BaseURL: srv.URL}).Viewer("s")
	for code, want := range map[int]bool{200: true, 401: false, 403: false, 404: false} {
		status = code
		if in, err := v.SignedIn(context.Background()); in != want || err != nil {
			t.Errorf("%d: %v %v", code, in, err)
		}
	}
	status = 500
	if _, err := v.SignedIn(context.Background()); err == nil {
		t.Error("a failure is not an error")
	}
}

// InBranch asks Gerrit which branches have a commit, for the project
// (escaped: names have slashes), and matches the branch with or without
// refs/heads/.
func TestInBranch(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.EscapedPath()
		io.WriteString(w, ")]}'\n"+`{"branches":["main","release"],"tags":[]}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()
	for branch, want := range map[string]bool{"main": true, "refs/heads/release": true, "dev": false} {
		if in, err := c.InBranch(ctx, "frc971/971-Robot-Code", branch, "abc"); err != nil || in != want {
			t.Errorf("%s: %v %v", branch, in, err)
		}
	}
	if asked != "/a/projects/frc971%2F971-Robot-Code/commits/abc/in" {
		t.Errorf("asked %s", asked)
	}
}

// A review can add reviewers; a change lists its reviewers by state, who
// changed them, and its attention set; the attention set can be changed.
func TestReviewers(t *testing.T) {
	var body string
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			posts = append(posts, r.URL.Path+" "+body)
			fmt.Fprint(w, ")]}'\n{}")
			return
		}
		fmt.Fprint(w, ")]}'\n"+`{"_number":5,"reviewers":{"REVIEWER":[{"_account_id":20,"username":"mikebauer"}],"CC":[{"_account_id":21,"username":"jane"}]},`+
			`"reviewer_updates":[{"updated":"2026-11-23 08:35:29.000000000","updated_by":{"_account_id":25,"username":"austin"},"reviewer":{"_account_id":20},"state":"REVIEWER"}],`+
			`"attention_set":{"20":{"account":{"_account_id":20,"username":"mikebauer"},"last_update":"2026-11-23 08:35:29.000000000","reason":"Reviewer was added"}},`+
			`"removed_from_attention_set":{"25":{"account":{"_account_id":25},"last_update":"2026-11-23 08:35:30.000000000","reason":"x"}}}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, User: "bridge", Password: "secret"}
	ctx := context.Background()
	if err := c.SetReview(ctx, 5, "abc", ReviewInput{Tag: "t", Reviewers: []ReviewerInput{{Reviewer: "jane"}}}); err != nil {
		t.Fatal(err)
	}
	if body != `{"tag":"t","reviewers":[{"reviewer":"jane"}]}` {
		t.Errorf("review: %s", body)
	}
	ci, err := c.GetChange(ctx, 5, "DETAILED_LABELS")
	if err != nil || len(ci.Reviewers["REVIEWER"]) != 1 || ci.Reviewers["REVIEWER"][0].Username != "mikebauer" || ci.Reviewers["CC"][0].AccountID != 21 {
		t.Errorf("reviewers: %+v %v", ci, err)
	}
	if u := ci.ReviewerUpdates; len(u) != 1 || u[0].UpdatedBy.Username != "austin" || u[0].Updated != "2026-11-23 08:35:29.000000000" || u[0].State != "REVIEWER" {
		t.Errorf("reviewer updates: %+v", u)
	}
	if a := ci.AttentionSet["20"]; a.Account.Username != "mikebauer" || a.LastUpdate != "2026-11-23 08:35:29.000000000" || a.Reason != "Reviewer was added" {
		t.Errorf("attention set: %+v", ci.AttentionSet)
	}
	if a := ci.RemovedFromAttentionSet["25"]; a.Account.AccountID != 25 {
		t.Errorf("removed from attention set: %+v", ci.RemovedFromAttentionSet)
	}
	posts = nil
	if err := c.AddToAttentionSet(ctx, 5, "mikebauer", "GitHub: asked"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveFromAttentionSet(ctx, 5, 20, "GitHub: ended"); err != nil {
		t.Fatal(err)
	}
	want := []string{`/a/changes/5/attention {"reason":"GitHub: asked","user":"mikebauer"}`, `/a/changes/5/attention/20/delete {"notify":"NONE","reason":"GitHub: ended"}`}
	if !slices.Equal(posts, want) {
		t.Errorf("attention set posts:\n%q\nwant\n%q", posts, want)
	}
}

// Gerrit's name for leaving everyone's turn alone, checked on Gerrit
// 3.14.4: a reply by someone else without it hands the owner the turn.
func TestReviewInputLeavesTurnsAlone(t *testing.T) {
	b, err := json.Marshal(ReviewInput{Message: "note", IgnoreAutomaticAttentionSetRules: true})
	if err != nil || !bytes.Contains(b, []byte(`"ignore_automatic_attention_set_rules":true`)) {
		t.Errorf("%s, %v", b, err)
	}
	if b, _ := json.Marshal(ReviewInput{Message: "note"}); bytes.Contains(b, []byte("attention")) {
		t.Errorf("%s: says something of turns", b)
	}
}
