// Package gerrit is a minimal client for Gerrit's REST API.
package gerrit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client calls Gerrit's authenticated REST endpoints (/a/...).
type Client struct {
	// BaseURL is the Gerrit web URL without a trailing slash.
	BaseURL  string
	User     string
	Password string // HTTP password
	HTTP     *http.Client
}

// ChangeInfo is the subset of Gerrit's ChangeInfo the bridge reads.
type ChangeInfo struct {
	ID              string `json:"id"`
	Project         string `json:"project"`
	Branch          string `json:"branch"`
	Topic           string `json:"topic"`
	ChangeID        string `json:"change_id"`
	Subject         string `json:"subject"`
	Status          string `json:"status"`
	Updated         string `json:"updated"`
	Number          int    `json:"_number"`
	CurrentRevision string `json:"current_revision"`
	WorkInProgress  bool   `json:"work_in_progress"`
	IsPrivate       bool   `json:"is_private"`
	MoreChanges     bool   `json:"_more_changes"`
}

// Error is a non-2xx response.
type Error struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("gerrit %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// xssiPrefix is prepended to every JSON response.
const xssiPrefix = ")]}'"

// Do makes an authenticated request to path (relative to /a/) and decodes
// the JSON response into out, if out is non-nil.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/a/"+strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(b[:min(len(b), 1000)]))}
	}
	if out == nil {
		return nil
	}
	b = bytes.TrimPrefix(b, []byte(xssiPrefix))
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("gerrit %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// QueryChanges runs a change query and returns every result, following
// Gerrit's _more_changes paging. opts are o= options such as
// CURRENT_REVISION.
func (c *Client) QueryChanges(ctx context.Context, query string, opts ...string) ([]ChangeInfo, error) {
	var all []ChangeInfo
	for {
		v := url.Values{"q": {query}}
		for _, o := range opts {
			v.Add("o", o)
		}
		if len(all) > 0 {
			v.Set("S", strconv.Itoa(len(all)))
		}
		var page []ChangeInfo
		if err := c.Do(ctx, http.MethodGet, "changes/?"+v.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) == 0 || !page[len(page)-1].MoreChanges {
			for i := range all {
				all[i].MoreChanges = false
			}
			return all, nil
		}
	}
}

// QueryString quotes s for use as a Gerrit query operand.
func QueryString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
