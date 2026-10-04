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
	ID              string                  `json:"id"`
	Project         string                  `json:"project"`
	Branch          string                  `json:"branch"`
	Topic           string                  `json:"topic"`
	ChangeID        string                  `json:"change_id"`
	Subject         string                  `json:"subject"`
	Status          string                  `json:"status"` // NEW, MERGED, ABANDONED
	Updated         string                  `json:"updated"`
	Number          int                     `json:"_number"`
	Owner           AccountInfo             `json:"owner"`
	CurrentRevision string                  `json:"current_revision"`
	Revisions       map[string]RevisionInfo `json:"revisions"`
	WorkInProgress  bool                    `json:"work_in_progress"`
	IsPrivate       bool                    `json:"is_private"`
	MoreChanges     bool                    `json:"_more_changes"`
}

// Current returns the current revision, if it was requested.
func (c *ChangeInfo) Current() (RevisionInfo, bool) {
	r, ok := c.Revisions[c.CurrentRevision]
	return r, ok
}

// AccountInfo identifies a user. Which fields are set depends on the
// query options (DETAILED_ACCOUNTS fills them all).
type AccountInfo struct {
	AccountID int    `json:"_account_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Username  string `json:"username"`
}

// RevisionInfo is a patch set.
type RevisionInfo struct {
	Number   int         `json:"_number"`
	Ref      string      `json:"ref"`
	Uploader AccountInfo `json:"uploader"`
	Commit   CommitInfo  `json:"commit"`
}

// CommitInfo is a commit's metadata (CURRENT_COMMIT / ALL_COMMITS).
type CommitInfo struct {
	Parents []struct {
		Commit string `json:"commit"`
	} `json:"parents"`
	Author struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// Parent is the first parent's SHA, or "" for a root commit.
func (c CommitInfo) Parent() string {
	if len(c.Parents) == 0 {
		return ""
	}
	return c.Parents[0].Commit
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

// ReviewInput is the body of a set-review call.
type ReviewInput struct {
	Message string         `json:"message,omitempty"`
	Tag     string         `json:"tag,omitempty"`
	Labels  map[string]int `json:"labels,omitempty"`
	// Notify is NONE, OWNER, OWNER_REVIEWERS or ALL.
	Notify string `json:"notify,omitempty"`
}

// SetReview posts a review on a revision ("current" for the latest).
func (c *Client) SetReview(ctx context.Context, change int, revision string, in ReviewInput) error {
	return c.Do(ctx, http.MethodPost, fmt.Sprintf("changes/%d/revisions/%s/review", change, url.PathEscape(revision)), in, nil)
}

// GetChange returns one change with the given o= options.
func (c *Client) GetChange(ctx context.Context, change int, opts ...string) (*ChangeInfo, error) {
	v := url.Values{}
	for _, o := range opts {
		v.Add("o", o)
	}
	var ci ChangeInfo
	err := c.Do(ctx, http.MethodGet, fmt.Sprintf("changes/%d?%s", change, v.Encode()), nil, &ci)
	return &ci, err
}

// ChangeURL is the web URL of a change.
func (c *Client) ChangeURL(project string, number int) string {
	return fmt.Sprintf("%s/c/%s/+/%d", c.BaseURL, project, number)
}
