package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ReviewComment is a line or file comment on a pull request.
type ReviewComment struct {
	ID               int64     `json:"id"`
	NodeID           string    `json:"node_id"`
	ReviewID         int64     `json:"pull_request_review_id"`
	InReplyTo        int64     `json:"in_reply_to_id"`
	Path             string    `json:"path"`
	CommitID         string    `json:"commit_id"`
	OriginalCommitID string    `json:"original_commit_id"`
	Line             int       `json:"line"`
	OriginalLine     int       `json:"original_line"`
	StartLine        int       `json:"start_line"`
	OriginalStart    int       `json:"original_start_line"`
	Side             string    `json:"side"`       // LEFT, RIGHT
	StartSide        string    `json:"start_side"` // LEFT, RIGHT
	SubjectType      string    `json:"subject_type"`
	Body             string    `json:"body"`
	User             User      `json:"user"`
	HTMLURL          string    `json:"html_url"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Review is a submitted review.
type Review struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	State       string    `json:"state"` // COMMENTED, APPROVED, CHANGES_REQUESTED, DISMISSED, PENDING
	Body        string    `json:"body"`
	CommitID    string    `json:"commit_id"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ListReviewComments lists every review comment on PR n.
func (c *Client) ListReviewComments(ctx context.Context, n int) ([]ReviewComment, error) {
	return list[ReviewComment](ctx, c, c.RepoPath("pulls/"+strconv.Itoa(n)+"/comments"))
}

// ListReviews lists PR n's reviews.
func (c *Client) ListReviews(ctx context.Context, n int) ([]Review, error) {
	return list[Review](ctx, c, c.RepoPath("pulls/"+strconv.Itoa(n)+"/reviews"))
}

// ListIssueComments lists PR (or issue) n's conversation comments.
func (c *Client) ListIssueComments(ctx context.Context, n int) ([]IssueComment, error) {
	return list[IssueComment](ctx, c, c.RepoPath("issues/"+strconv.Itoa(n)+"/comments"))
}

// DraftReviewComment is a comment inside a NewReview.
type DraftReviewComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

// NewReview is the body of a create-review call.
type NewReview struct {
	CommitID string               `json:"commit_id,omitempty"`
	Body     string               `json:"body,omitempty"`
	Event    string               `json:"event"` // COMMENT, APPROVE, REQUEST_CHANGES
	Comments []DraftReviewComment `json:"comments,omitempty"`
}

// CreateReview submits a review on PR n.
func (c *Client) CreateReview(ctx context.Context, n int, r NewReview) (*Review, error) {
	var out Review
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("pulls/"+strconv.Itoa(n)+"/reviews"), r, &out)
	return &out, err
}

// ListReviewCommentsOf lists the comments of one review.
func (c *Client) ListReviewCommentsOf(ctx context.Context, n int, review int64) ([]ReviewComment, error) {
	return list[ReviewComment](ctx, c, c.RepoPath("pulls/"+strconv.Itoa(n)+"/reviews/"+strconv.FormatInt(review, 10)+"/comments"))
}

// NewReviewComment is a standalone review comment. SubjectType "file"
// comments on the whole file (no line).
type NewReviewComment struct {
	CommitID    string `json:"commit_id"`
	Path        string `json:"path"`
	Body        string `json:"body"`
	Line        int    `json:"line,omitempty"`
	Side        string `json:"side,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	StartSide   string `json:"start_side,omitempty"`
	SubjectType string `json:"subject_type,omitempty"`
}

// CreateReviewComment posts a standalone review comment on PR n.
func (c *Client) CreateReviewComment(ctx context.Context, n int, rc NewReviewComment) (*ReviewComment, error) {
	var out ReviewComment
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("pulls/"+strconv.Itoa(n)+"/comments"), rc, &out)
	return &out, err
}

// ReplyToReviewComment replies in the thread of review comment id.
func (c *Client) ReplyToReviewComment(ctx context.Context, n int, id int64, body string) (*ReviewComment, error) {
	var out ReviewComment
	_, err := c.Do(ctx, http.MethodPost,
		c.RepoPath("pulls/"+strconv.Itoa(n)+"/comments/"+strconv.FormatInt(id, 10)+"/replies"),
		map[string]string{"body": body}, &out)
	return &out, err
}

// ReviewThread is a review thread's identity and resolution, which only
// GraphQL exposes.
type ReviewThread struct {
	ID         string
	IsResolved bool
	// RootID is the database id of the thread's first comment.
	RootID int64
	// ResolvedBy is the login of whoever resolved it, if resolved.
	ResolvedBy string
}

// ReviewThreads lists PR n's review threads.
func (c *Client) queryReviewThreads(ctx context.Context, n int) ([]ReviewThread, error) {
	const q = `query($o: String!, $r: String!, $n: Int!, $after: String) {
  repository(owner: $o, name: $r) { pullRequest(number: $n) {
    reviewThreads(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id isResolved resolvedBy { login } comments(first: 1) { nodes { databaseId } } }
    } } } }`
	var out []ReviewThread
	var after any
	for {
		var data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							ResolvedBy *struct {
								Login string `json:"login"`
							} `json:"resolvedBy"`
							Comments struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		err := c.GraphQL(ctx, q, map[string]any{"o": c.Owner, "r": c.Repo, "n": n, "after": after}, &data)
		if err != nil {
			return nil, err
		}
		rt := data.Repository.PullRequest.ReviewThreads
		for _, t := range rt.Nodes {
			th := ReviewThread{ID: t.ID, IsResolved: t.IsResolved}
			if t.ResolvedBy != nil {
				th.ResolvedBy = t.ResolvedBy.Login
			}
			if len(t.Comments.Nodes) > 0 {
				th.RootID = t.Comments.Nodes[0].DatabaseID
			}
			out = append(out, th)
		}
		if !rt.PageInfo.HasNextPage {
			return out, nil
		}
		after = rt.PageInfo.EndCursor
	}
}

// SetThreadResolved resolves or unresolves a review thread.
func (c *Client) SetThreadResolved(ctx context.Context, threadID string, resolved bool) error {
	m := "unresolveReviewThread"
	if resolved {
		m = "resolveReviewThread"
	}
	q := "mutation($id: ID!) { " + m + "(input: {threadId: $id}) { thread { isResolved } } }"
	return c.GraphQL(ctx, q, map[string]any{"id": threadID}, nil)
}

// Login returns the authenticated user's login.
func (c *Client) Login(ctx context.Context) (string, error) {
	var u User
	_, err := c.Do(ctx, http.MethodGet, "/user", nil, &u)
	return u.Login, err
}

// UserID returns the numeric id of the account that has login now, and
// when it was created. Logins can change hands (after a rename); ids never
// do.
func (c *Client) UserID(ctx context.Context, login string) (int64, time.Time, error) {
	var u struct {
		ID      int64     `json:"id"`
		Created time.Time `json:"created_at"`
	}
	_, err := c.Do(ctx, http.MethodGet, "/users/"+url.PathEscape(login), nil, &u)
	return u.ID, u.Created, err
}

// UpdateReviewComment replaces a review comment's body.
func (c *Client) UpdateReviewComment(ctx context.Context, id int64, body string) error {
	_, err := c.Do(ctx, http.MethodPatch, c.RepoPath("pulls/comments/"+strconv.FormatInt(id, 10)), map[string]string{"body": body}, nil)
	return err
}

// UpdateIssueComment replaces an issue comment's body.
func (c *Client) UpdateIssueComment(ctx context.Context, id int64, body string) error {
	_, err := c.Do(ctx, http.MethodPatch, c.RepoPath("issues/comments/"+strconv.FormatInt(id, 10)), map[string]string{"body": body}, nil)
	return err
}

// FileAt returns a file's content at ref (a commit SHA or branch).
func (c *Client) FileAt(ctx context.Context, ref, path string) ([]byte, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	u := c.RepoPath("contents/"+escapePath(path)) + "?ref=" + url.QueryEscape(ref)
	if _, err := c.Do(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("contents of %s@%s: unexpected encoding %q", path, ref, out.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (c *Client) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	var out struct {
		Status string `json:"status"` // ahead, behind, diverged, identical
	}
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("compare/"+a+"..."+b+"?per_page=1"), nil, &out)
	if err != nil {
		return false, err
	}
	return out.Status == "ahead" || out.Status == "identical", nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// ReviewEdits is Edits for PR n's review bodies, by review database id
// (kept apart: reviews and comments are numbered separately).
func (c *Client) queryReviewEdits(ctx context.Context, n int) (map[int64]string, error) {
	const q = `query($o: String!, $r: String!, $n: Int!, $after: String) {
  repository(owner: $o, name: $r) { pullRequest(number: $n) {
    reviews(first: 100, after: $after) { pageInfo { hasNextPage endCursor }
      nodes { databaseId lastEditedAt editor { login } } } } } }`
	out := map[int64]string{}
	for after := any(nil); ; {
		var data struct {
			Repository struct {
				PullRequest struct {
					Reviews struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							DatabaseID   int64      `json:"databaseId"`
							LastEditedAt *time.Time `json:"lastEditedAt"`
							Editor       *User      `json:"editor"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.GraphQL(ctx, q, map[string]any{"o": c.Owner, "r": c.Repo, "n": n, "after": after}, &data); err != nil {
			return nil, err
		}
		rs := data.Repository.PullRequest.Reviews
		for _, r := range rs.Nodes {
			if r.LastEditedAt != nil {
				out[r.DatabaseID] = ""
				if r.Editor != nil {
					out[r.DatabaseID] = r.Editor.Login
				}
			}
		}
		if !rs.PageInfo.HasNextPage {
			return out, nil
		}
		after = rs.PageInfo.EndCursor
	}
}

// Edits returns who last edited each edited comment on PR n, issue and
// review comments alike, by database id; comments never edited are
// absent. The login is "" if GitHub does not say (a deleted account).
//
// Unlike updated_at, which GitHub also moves without an edit, GraphQL's
// lastEditedAt is set only by an edit, and editor says whose.
func (c *Client) queryEdits(ctx context.Context, n int) (map[int64]string, error) {
	type comment struct {
		DatabaseID   int64      `json:"databaseId"`
		LastEditedAt *time.Time `json:"lastEditedAt"`
		Editor       *User      `json:"editor"`
	}
	type page struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	}
	out := map[int64]string{}
	note := func(cs []comment) {
		for _, c := range cs {
			if c.LastEditedAt != nil {
				out[c.DatabaseID] = ""
				if c.Editor != nil {
					out[c.DatabaseID] = c.Editor.Login
				}
			}
		}
	}
	const fields = `databaseId lastEditedAt editor { login }`
	const issueQ = `query($o: String!, $r: String!, $n: Int!, $after: String) {
  repository(owner: $o, name: $r) { pullRequest(number: $n) {
    comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ` + fields + ` } } } } }`
	const threadQ = `query($o: String!, $r: String!, $n: Int!, $after: String) {
  repository(owner: $o, name: $r) { pullRequest(number: $n) {
    reviewThreads(first: 50, after: $after) { pageInfo { hasNextPage endCursor }
      nodes { comments(first: 100) { nodes { ` + fields + ` } } } } } } }`
	for after := any(nil); ; {
		var data struct {
			Repository struct {
				PullRequest struct {
					Comments struct {
						PageInfo page      `json:"pageInfo"`
						Nodes    []comment `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.GraphQL(ctx, issueQ, map[string]any{"o": c.Owner, "r": c.Repo, "n": n, "after": after}, &data); err != nil {
			return nil, err
		}
		cs := data.Repository.PullRequest.Comments
		note(cs.Nodes)
		if !cs.PageInfo.HasNextPage {
			break
		}
		after = cs.PageInfo.EndCursor
	}
	for after := any(nil); ; {
		var data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo page `json:"pageInfo"`
						Nodes    []struct {
							Comments struct {
								Nodes []comment `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.GraphQL(ctx, threadQ, map[string]any{"o": c.Owner, "r": c.Repo, "n": n, "after": after}, &data); err != nil {
			return nil, err
		}
		ts := data.Repository.PullRequest.ReviewThreads
		for _, t := range ts.Nodes {
			note(t.Comments.Nodes)
		}
		if !ts.PageInfo.HasNextPage {
			return out, nil
		}
		after = ts.PageInfo.EndCursor
	}
}

// DismissReview dismisses approval or change request id on PR n, saying
// why.
func (c *Client) DismissReview(ctx context.Context, n int, id int64, message string) error {
	path := c.RepoPath(fmt.Sprintf("pulls/%d/reviews/%d/dismissals", n, id))
	_, err := c.Do(ctx, http.MethodPut, path, map[string]string{"message": message, "event": "DISMISS"}, nil)
	return err
}

// IssueEvent is one event in an issue's (or PR's) history.
type IssueEvent struct {
	Event     string    `json:"event"` // closed, reopened, merged, ...
	Actor     User      `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// LastStateEvent returns the most recent "closed" or "reopened" event of
// issue or PR n, or nil if there is none.
func (c *Client) LastStateEvent(ctx context.Context, n int) (*IssueEvent, error) {
	evs, err := list[IssueEvent](ctx, c, c.RepoPath(fmt.Sprintf("issues/%d/events", n)))
	if err != nil {
		return nil, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Event == "closed" || evs[i].Event == "reopened" {
			return &evs[i], nil
		}
	}
	return nil, nil
}

// PRActivity says who last changed a PR's text and its draft state, for
// mirroring those changes as the person who made them.
type PRActivity struct {
	// TitleEditor last renamed the PR, and BodyEditor last edited its
	// description ("" if nobody has since the PR was opened).
	TitleEditor, BodyEditor string
	// DraftActor last converted the PR to a draft or marked it ready.
	DraftActor string
}

// Activity returns PR n's PRActivity.
func (c *Client) Activity(ctx context.Context, n int) (*PRActivity, error) {
	const q = `query($o: String!, $r: String!, $n: Int!) { repository(owner: $o, name: $r) { pullRequest(number: $n) {
  lastEditedAt editor { login }
  timelineItems(last: 50, itemTypes: [RENAMED_TITLE_EVENT, CONVERT_TO_DRAFT_EVENT, READY_FOR_REVIEW_EVENT]) {
    nodes { __typename
      ... on RenamedTitleEvent { createdAt actor { login } }
      ... on ConvertToDraftEvent { createdAt actor { login } }
      ... on ReadyForReviewEvent { createdAt actor { login } } } } } } }`
	type event struct {
		Type      string    `json:"__typename"`
		CreatedAt time.Time `json:"createdAt"`
		Actor     *User     `json:"actor"`
	}
	var data struct {
		Repository struct {
			PullRequest struct {
				LastEditedAt  *time.Time `json:"lastEditedAt"`
				Editor        *User      `json:"editor"`
				TimelineItems struct {
					Nodes []event `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.GraphQL(ctx, q, map[string]any{"o": c.Owner, "r": c.Repo, "n": n}, &data); err != nil {
		return nil, err
	}
	pr := data.Repository.PullRequest
	a := &PRActivity{}
	if pr.LastEditedAt != nil && pr.Editor != nil {
		a.BodyEditor = pr.Editor.Login
	}
	for _, e := range pr.TimelineItems.Nodes { // oldest first
		if e.Actor == nil {
			continue
		}
		switch e.Type {
		case "RenamedTitleEvent":
			a.TitleEditor = e.Actor.Login
		case "ConvertToDraftEvent", "ReadyForReviewEvent":
			a.DraftActor = e.Actor.Login
		}
	}
	return a, nil
}

// GraphQL has no conditional requests, so a PR's GraphQL answers that REST
// can vouch for are kept and reused while the REST lists behind them are
// unchanged: every page comes back 304, which costs no rate limit (design
// §9). What REST cannot see is always asked: a thread's resolution changes
// no REST answer (checked live, 2026-10-19).

// Edits returns who last edited each edited comment on PR n, issue and
// review comments alike, by database id (see queryEdits). An edit changes
// the comment's body and updated_at, so while both comment lists are
// unchanged the last answer stands.
func (c *Client) Edits(ctx context.Context, n int) (map[int64]string, error) {
	return c.vouched(ctx, "edits/"+strconv.Itoa(n), []string{
		c.RepoPath("issues/" + strconv.Itoa(n) + "/comments"),
		c.RepoPath("pulls/" + strconv.Itoa(n) + "/comments"),
	}, func() (map[int64]string, error) { return c.queryEdits(ctx, n) })
}

// ReviewEdits is Edits for review bodies, vouched for by the review list.
func (c *Client) ReviewEdits(ctx context.Context, n int) (map[int64]string, error) {
	return c.vouched(ctx, "review-edits/"+strconv.Itoa(n), []string{
		c.RepoPath("pulls/" + strconv.Itoa(n) + "/reviews"),
	}, func() (map[int64]string, error) { return c.queryReviewEdits(ctx, n) })
}

// ReviewThreads lists PR n's review threads (see queryReviewThreads). A PR
// without review comments has none, which REST says for free; otherwise
// it is always asked, since resolving a thread changes nothing in REST.
func (c *Client) ReviewThreads(ctx context.Context, n int) ([]ReviewThread, error) {
	_, count, err := c.listPrint(ctx, c.RepoPath("pulls/"+strconv.Itoa(n)+"/comments"))
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return c.queryReviewThreads(ctx, n)
}

type vouchedAnswer struct {
	print string
	value map[int64]string
	// confirmed is set once the same answer came twice for print.
	confirmed bool
}

// vouched returns the answer cached under key if the lists at paths are
// unchanged since it was computed, and computes (and caches) it if not.
// An answer is cached only if the lists were the same before and after
// computing it, so one that raced a change is not kept; and reused only
// once computed twice alike for them: GraphQL can lag REST a little, and
// the first answer after an edit, not showing it yet, would otherwise
// stand until the lists next changed (fourth audit).
// maxAnswers bounds the GraphQL answers kept (a var for tests).
var maxAnswers = 4096

func (c *Client) vouched(ctx context.Context, key string, paths []string, compute func() (map[int64]string, error)) (map[int64]string, error) {
	before, err := c.listsPrint(ctx, paths)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached, ok := c.answers[key]
	c.mu.Unlock()
	if ok && before != "" && cached.print == before && cached.confirmed {
		return maps.Clone(cached.value), nil
	}
	v, err := compute()
	if err != nil {
		return nil, err
	}
	after, err := c.listsPrint(ctx, paths)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.answers == nil || len(c.answers) >= maxAnswers {
		// Bounded (one entry or two per PR ever synced, forks' included):
		// forgetting costs a GraphQL query each.
		c.answers = map[string]vouchedAnswer{}
	}
	if before != "" && after == before {
		same := ok && cached.print == before && maps.Equal(cached.value, v)
		c.answers[key] = vouchedAnswer{print: before, value: maps.Clone(v), confirmed: same}
	} else {
		delete(c.answers, key)
	}
	c.mu.Unlock()
	return v, nil
}

// listsPrint is listPrint over several lists, "" if any has a page
// without an ETag.
func (c *Client) listsPrint(ctx context.Context, paths []string) (string, error) {
	var print strings.Builder
	for _, p := range paths {
		pp, _, err := c.listPrint(ctx, p)
		if err != nil {
			return "", err
		}
		if pp == "" {
			return "", nil // nothing to vouch with
		}
		print.WriteString(pp + "\n")
	}
	return print.String(), nil
}

// listPrint fetches every page of the list at path (free when unchanged,
// through the ETag cache) and returns the pages' ETags together, "" if a
// page had none, and how many items there are.
func (c *Client) listPrint(ctx context.Context, path string) (string, int, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := path + sep + "per_page=100"
	var print strings.Builder
	count := 0
	for next != "" {
		var page []json.RawMessage
		resp, err := c.Do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return "", 0, err
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return "", len(page), nil
		}
		print.WriteString(etag + " ")
		count += len(page)
		next = nextLink(resp.Header.Get("Link"))
	}
	return print.String(), count, nil
}
