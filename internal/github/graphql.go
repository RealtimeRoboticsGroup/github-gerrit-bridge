package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

// GraphQLError is an error reported in a GraphQL response body.
type GraphQLError struct {
	Messages []string
}

func (e *GraphQLError) Error() string { return "github graphql: " + strings.Join(e.Messages, "; ") }

// GraphQL runs query with variables and decodes "data" into out.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.Do(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		ge := &GraphQLError{}
		for _, e := range resp.Errors {
			ge.Messages = append(ge.Messages, e.Message)
		}
		return ge
	}
	// A query that reads posts too; only a mutation writes.
	if m, ok := strings.CutPrefix(strings.TrimSpace(query), "mutation"); ok {
		if i := strings.Index(m, "{"); i >= 0 {
			m = strings.TrimSpace(m[i+1:])
		}
		if i := strings.IndexAny(m, "( {"); i >= 0 {
			m = m[:i]
		}
		trace.Wrote(ctx, "github graphql "+m)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("github graphql: decoding data: %w", err)
		}
	}
	return nil
}

// SetDraft converts a pull request (by node id) to a draft or marks it
// ready for review. REST cannot change draft state.
func (c *Client) SetDraft(ctx context.Context, prNodeID string, draft bool) error {
	m := "markPullRequestReadyForReview"
	if draft {
		m = "convertPullRequestToDraft"
	}
	q := "mutation($id: ID!) { " + m + "(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"
	return c.GraphQL(ctx, q, map[string]any{"id": prNodeID}, nil)
}

// IssueComment is a top-level PR conversation comment.
type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateIssueComment posts a conversation comment on PR (or issue) n.
func (c *Client) CreateIssueComment(ctx context.Context, n int, body string) (*IssueComment, error) {
	var ic IssueComment
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("issues/"+strconv.Itoa(n)+"/comments"), map[string]string{"body": body}, &ic)
	return &ic, err
}
