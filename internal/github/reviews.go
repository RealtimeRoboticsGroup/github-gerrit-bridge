package github

import (
	"context"
	"encoding/base64"
	"fmt"
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
func (c *Client) ReviewThreads(ctx context.Context, n int) ([]ReviewThread, error) {
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

// UserID returns the numeric id of the account that has login now. Logins
// can change hands (after a rename); ids never do.
func (c *Client) UserID(ctx context.Context, login string) (int64, error) {
	var u User
	_, err := c.Do(ctx, http.MethodGet, "/users/"+url.PathEscape(login), nil, &u)
	return u.ID, err
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

// Edits returns who last edited each edited comment on PR n, issue and
// review comments alike, by database id; comments never edited are
// absent. The login is "" if GitHub does not say (a deleted account).
//
// Unlike updated_at, which GitHub also moves without an edit, GraphQL's
// lastEditedAt is set only by an edit, and editor says whose.
func (c *Client) Edits(ctx context.Context, n int) (map[int64]string, error) {
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
