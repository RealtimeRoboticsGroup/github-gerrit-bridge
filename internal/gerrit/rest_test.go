package gerrit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
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
