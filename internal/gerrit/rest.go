// Package gerrit is a minimal client for Gerrit's REST API.
package gerrit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls Gerrit's authenticated REST endpoints (/a/...).
type Client struct {
	// BaseURL is the Gerrit web URL without a trailing slash.
	BaseURL  string
	User     string
	Password string // HTTP password
	HTTP     *http.Client
	// RunAs makes requests on behalf of this account (X-Gerrit-RunAs);
	// the authenticated account needs the Run As capability.
	RunAs string
	// Anonymous makes unauthenticated requests, to see only what anyone
	// can see.
	Anonymous bool
}

// Anon returns a client that sees only what anonymous users can.
func (c *Client) Anon() *Client {
	return &Client{BaseURL: c.BaseURL, HTTP: c.HTTP, Anonymous: true}
}

// maxResponse bounds a REST response the client reads, and maxFileContent
// a file's content.
const (
	maxResponse    = 64 << 20
	maxFileContent = 8 << 20
)

// readLimited reads r whole, failing if it has more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		return nil, fmt.Errorf("response larger than %d bytes", limit)
	}
	return b, err
}

// AmbiguousUsername reports whether Gerrit would not read user as a
// username where it takes an account (X-Gerrit-RunAs, suexec --as, the
// accounts/ API): Gerrit tries "self"/"me" and numeric account ids first,
// so a user named "1000000" stands for account 1000000, and "me" for the
// caller.
func AmbiguousUsername(user string) bool {
	if strings.EqualFold(user, "self") || strings.EqualFold(user, "me") {
		return true
	}
	return strings.Trim(user, "0123456789") == ""
}

// As returns a client acting as user via Run-As. Requests fail if user is
// an AmbiguousUsername.
func (c *Client) As(user string) *Client {
	cp := *c
	cp.RunAs = user
	return &cp
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

	// With DETAILED_LABELS.
	Labels map[string]LabelInfo `json:"labels"`
	// With SUBMITTABLE.
	Submittable bool `json:"submittable"`
	// With SUBMIT_REQUIREMENTS.
	SubmitRequirements []SubmitRequirementResult `json:"submit_requirements"`
	// With MESSAGES.
	Messages []ChangeMessage `json:"messages"`
}

// LabelInfo is a label's votes (DETAILED_LABELS).
type LabelInfo struct {
	All []ApprovalInfo `json:"all"`
	// Values maps " 0", "+1", "-2" ... to descriptions.
	Values map[string]string `json:"values"`
}

// Range returns the label's lowest and highest values.
func (l LabelInfo) Range() (lo, hi int) {
	first := true
	for k := range l.Values {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(k), "%d", &v); err != nil {
			continue
		}
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	return lo, hi
}

// ApprovalInfo is one account's vote on a label.
type ApprovalInfo struct {
	AccountInfo
	Value int `json:"value"`
	// Date is when it was given, as Gerrit writes times.
	Date string `json:"date"`
	// Tag is the tag of the review that set the vote.
	Tag string `json:"tag"`
}

// SubmitRequirementResult is one submit requirement's status.
type SubmitRequirementResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // SATISFIED, UNSATISFIED, OVERRIDDEN, NOT_APPLICABLE, ERROR, FORCED
}

// ChangeMessage is an entry in a change's log.
type ChangeMessage struct {
	ID             string      `json:"id"`
	Author         AccountInfo `json:"author"`
	Message        string      `json:"message"`
	Tag            string      `json:"tag"`
	Date           string      `json:"date"`
	RevisionNumber int         `json:"_revision_number"`
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
	Inactive  bool   `json:"inactive,omitempty"`
}

// RevisionInfo is a patch set.
type RevisionInfo struct {
	Number   int         `json:"_number"`
	Ref      string      `json:"ref"`
	Uploader AccountInfo `json:"uploader"`
	// RealUploader is who really uploaded it, when that was on the
	// uploader's behalf (a rebase, say).
	RealUploader *AccountInfo `json:"real_uploader,omitempty"`
	Commit       CommitInfo   `json:"commit"`
	Created      string       `json:"created"` // "2006-01-02 15:04:05.000000000", UTC
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

// Do makes a request to path (relative to /a/, or to / if Anonymous) and
// decodes the JSON response into out, if out is non-nil.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	prefix := "/a/"
	if c.Anonymous {
		prefix = "/"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+prefix+strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return err
	}
	if !c.Anonymous {
		req.SetBasicAuth(c.User, c.Password)
	}
	req.Header.Set("Accept", "application/json")
	if c.RunAs != "" {
		if AmbiguousUsername(c.RunAs) {
			return fmt.Errorf("refusing to run as %q: Gerrit would not read it as a username", c.RunAs)
		}
		req.Header.Set("X-Gerrit-RunAs", c.RunAs)
	}
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
	b, err := readLimited(resp.Body, maxResponse)
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
	Message  string                    `json:"message,omitempty"`
	Tag      string                    `json:"tag,omitempty"`
	Labels   map[string]int            `json:"labels,omitempty"`
	Comments map[string][]CommentInput `json:"comments,omitempty"`
	// Notify is NONE, OWNER, OWNER_REVIEWERS or ALL.
	Notify string `json:"notify,omitempty"`
	// OmitDuplicateComments drops comments identical to existing ones,
	// which makes a retried post idempotent.
	OmitDuplicateComments bool `json:"omit_duplicate_comments,omitempty"`
}

// Range is a character range in a file. Lines are 1-based, characters
// 0-based; end_character is exclusive.
type Range struct {
	StartLine      int `json:"start_line"`
	StartCharacter int `json:"start_character"`
	EndLine        int `json:"end_line"`
	EndCharacter   int `json:"end_character"`
}

// CommentInput is a comment in a ReviewInput.
type CommentInput struct {
	Side       string `json:"side,omitempty"` // "PARENT" or empty for REVISION
	Line       int    `json:"line,omitempty"`
	Range      *Range `json:"range,omitempty"`
	InReplyTo  string `json:"in_reply_to,omitempty"`
	Message    string `json:"message"`
	Unresolved *bool  `json:"unresolved,omitempty"`
	// FixSuggestions are suggested edits ("Apply fix" in Gerrit).
	FixSuggestions []FixSuggestion `json:"fix_suggestions,omitempty"`
}

// FixSuggestion is a suggested edit attached to a comment.
type FixSuggestion struct {
	FixID        string           `json:"fix_id,omitempty"`
	Description  string           `json:"description"`
	Replacements []FixReplacement `json:"replacements"`
}

// FixReplacement replaces Range of Path with Replacement.
type FixReplacement struct {
	Path        string `json:"path"`
	Range       Range  `json:"range"`
	Replacement string `json:"replacement"`
}

// CommentInfo is a published comment.
type CommentInfo struct {
	ID             string          `json:"id"`
	Path           string          `json:"path"`
	PatchSet       int             `json:"patch_set"`
	CommitID       string          `json:"commit_id"`
	Side           string          `json:"side"`
	Line           int             `json:"line"`
	Range          *Range          `json:"range"`
	InReplyTo      string          `json:"in_reply_to"`
	Message        string          `json:"message"`
	Updated        string          `json:"updated"`
	Author         AccountInfo     `json:"author"`
	Tag            string          `json:"tag"`
	Unresolved     bool            `json:"unresolved"`
	FixSuggestions []FixSuggestion `json:"fix_suggestions"`
}

// Special file paths.
const (
	PatchSetLevel = "/PATCHSET_LEVEL"
	CommitMsg     = "/COMMIT_MSG"
)

// ListComments returns every published comment on a change, with Path
// filled in.
func (c *Client) ListComments(ctx context.Context, change int) ([]CommentInfo, error) {
	var byPath map[string][]CommentInfo
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("changes/%d/comments", change), nil, &byPath); err != nil {
		return nil, err
	}
	var all []CommentInfo
	for path, cs := range byPath {
		for _, ci := range cs {
			ci.Path = path
			all = append(all, ci)
		}
	}
	return all, nil
}

// FileContent returns a file's content in a revision. Gerrit serves it
// base64-encoded.
func (c *Client) FileContent(ctx context.Context, change int, revision, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/a/changes/%d/revisions/%s/files/%s/content", c.BaseURL, change, url.PathEscape(revision), url.PathEscape(path)), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.User, c.Password)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Base64, so a third bigger than the file. Fix suggestions are for
	// source files; anything bigger is not read whole into memory.
	b, err := readLimited(resp.Body, maxFileContent*4/3+4)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Method: http.MethodGet, Path: req.URL.Path, Status: resp.StatusCode, Body: string(b)}
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
}

// CommentURL links to a comment in the Gerrit UI.
func (c *Client) CommentURL(project string, change int, id string) string {
	return fmt.Sprintf("%s/c/%s/+/%d/comment/%s/", c.BaseURL, project, change, id)
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

// GroupMembers returns a group's members, including those of included
// groups.
func (c *Client) GroupMembers(ctx context.Context, group string) ([]AccountInfo, error) {
	var out []AccountInfo
	err := c.Do(ctx, http.MethodGet, "groups/"+url.PathEscape(group)+"/members/?recursive", nil, &out)
	return out, err
}

// GroupInfo is a group as Gerrit lists it.
type GroupInfo struct {
	ID   string `json:"id"` // its UUID: 40 hex digits for an internal group
	Name string `json:"name"`
}

// IncludedGroups lists the groups directly included in group.
func (c *Client) IncludedGroups(ctx context.Context, group string) ([]GroupInfo, error) {
	var out []GroupInfo
	err := c.Do(ctx, http.MethodGet, "groups/"+url.PathEscape(group)+"/groups/", nil, &out)
	return out, err
}

// Submit submits revision rev of a change (with its unsubmitted
// ancestors). Gerrit refuses if rev is no longer current.
func (c *Client) Submit(ctx context.Context, change int, rev string) (*ChangeInfo, error) {
	var ci ChangeInfo
	err := c.Do(ctx, http.MethodPost, fmt.Sprintf("changes/%d/revisions/%s/submit", change, url.PathEscape(rev)), map[string]any{}, &ci)
	return &ci, err
}

// ActionInfo is one action the caller may take on a change.
type ActionInfo struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`
}

// RevisionActions lists what the caller may do with change's current
// patch set ("submit" among them when it is submittable by them).
func (c *Client) RevisionActions(ctx context.Context, change int, rev string) (map[string]ActionInfo, error) {
	out := map[string]ActionInfo{}
	err := c.Do(ctx, http.MethodGet, fmt.Sprintf("changes/%d/revisions/%s/actions", change, url.PathEscape(rev)), nil, &out)
	return out, err
}

// Registered returns when account was registered.
func (c *Client) Registered(ctx context.Context, account int) (time.Time, error) {
	var out struct {
		RegisteredOn string `json:"registered_on"`
	}
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("accounts/%d/detail", account), nil, &out); err != nil {
		return time.Time{}, err
	}
	// Gerrit's timestamps are UTC: "2006-01-02 15:04:05.000000000".
	return time.Parse("2006-01-02 15:04:05.999999999", out.RegisteredOn)
}

// QueryAccounts returns the accounts matching query, with details.
func (c *Client) QueryAccounts(ctx context.Context, query string) ([]AccountInfo, error) {
	var out []AccountInfo
	err := c.Do(ctx, http.MethodGet, "accounts/?o=DETAILS&q="+url.QueryEscape(query), nil, &out)
	return out, err
}

// SubmittedTogether lists the changes a submit of change would merge,
// including change itself.
func (c *Client) SubmittedTogether(ctx context.Context, change int) ([]ChangeInfo, error) {
	var out []ChangeInfo
	err := c.Do(ctx, http.MethodGet, fmt.Sprintf("changes/%d/submitted_together?o=CURRENT_REVISION", change), nil, &out)
	return out, err
}

// BranchExists reports whether project has branch.
func (c *Client) BranchExists(ctx context.Context, project, branch string) (bool, error) {
	err := c.Do(ctx, http.MethodGet, "projects/"+url.PathEscape(project)+"/branches/"+url.PathEscape("refs/heads/"+branch), nil, nil)
	var ge *Error
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}
