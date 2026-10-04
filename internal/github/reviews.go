package github

import (
	"context"
	"net/http"
	"strconv"
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
}

// ReviewThreads lists PR n's review threads.
func (c *Client) ReviewThreads(ctx context.Context, n int) ([]ReviewThread, error) {
	const q = `query($o: String!, $r: String!, $n: Int!, $after: String) {
  repository(owner: $o, name: $r) { pullRequest(number: $n) {
    reviewThreads(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id isResolved comments(first: 1) { nodes { databaseId } } }
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
							Comments   struct {
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
