package github

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repository is the subset of a repository the bridge reads.
type Repository struct {
	ID            int64  `json:"id"`
	NodeID        string `json:"node_id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// GetRepo returns the client's repository.
func (c *Client) GetRepo(ctx context.Context) (*Repository, error) {
	var r Repository
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath(""), nil, &r)
	return &r, err
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"` // "User", "Bot"
}

// Ref is a branch at a commit.
type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	// Repo is the repository the branch is in (a fork for fork PRs).
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo,omitempty"`
}

// PullRequest is the subset of a pull request the bridge reads.
type PullRequest struct {
	Number   int        `json:"number"`
	NodeID   string     `json:"node_id"`
	State    string     `json:"state"` // open, closed
	Draft    bool       `json:"draft"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	HTMLURL  string     `json:"html_url"`
	User     User       `json:"user"`
	Head     Ref        `json:"head"`
	Base     Ref        `json:"base"`
	// MaintainerCanModify is set when a fork PR's author lets people with
	// write access to the base repository push to the head branch. Only
	// GetPull returns it; listed PRs always say false.
	MaintainerCanModify bool      `json:"maintainer_can_modify"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// Stack is set when the PR is in a native stack.
	Stack *PRStack `json:"stack"`
}

// PRStack is the stack summary embedded in a pull request.
type PRStack struct {
	ID       int64 `json:"id"`
	Number   int   `json:"number"`
	Base     Ref   `json:"base"`
	Position int   `json:"position"` // 1 is the bottom
	Size     int   `json:"size"`
}

// GetPull returns pull request n.
func (c *Client) GetPull(ctx context.Context, n int) (*PullRequest, error) {
	var pr PullRequest
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("pulls/"+strconv.Itoa(n)), nil, &pr)
	return &pr, err
}

// ListPulls lists pull requests in state "open", "closed" or "all".
func (c *Client) ListPulls(ctx context.Context, state string) ([]PullRequest, error) {
	return list[PullRequest](ctx, c, c.RepoPath("pulls?state="+state))
}

// NewPull is the body of a create-pull-request call.
type NewPull struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// CreatePull opens a pull request.
func (c *Client) CreatePull(ctx context.Context, p NewPull) (*PullRequest, error) {
	var pr PullRequest
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("pulls"), p, &pr)
	return &pr, err
}

// PullUpdate changes fields of a pull request; nil fields are left alone.
type PullUpdate struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	Base  *string `json:"base,omitempty"`
	State *string `json:"state,omitempty"` // open, closed
}

// UpdatePull edits pull request n.
func (c *Client) UpdatePull(ctx context.Context, n int, u PullUpdate) (*PullRequest, error) {
	var pr PullRequest
	_, err := c.Do(ctx, http.MethodPatch, c.RepoPath("pulls/"+strconv.Itoa(n)), u, &pr)
	return &pr, err
}

// HeadRepo is the owner/name of the repository holding the PR's head
// branch, or "" if GitHub did not say (deleted fork).
func (pr *PullRequest) HeadRepo() string {
	if pr.Head.Repo == nil {
		return ""
	}
	return pr.Head.Repo.FullName
}

// HeadKey names the PR's head branch uniquely among the PRs of repo
// (owner/name): the branch name for a branch of repo itself, else
// "<fork owner>/<fork name>:<branch>". A ':' cannot occur in a git branch
// name, so a fork's head never collides with a branch of repo.
func (pr *PullRequest) HeadKey(repo string) string {
	if strings.EqualFold(pr.HeadRepo(), repo) {
		return pr.Head.Ref
	}
	return pr.HeadRepo() + ":" + pr.Head.Ref
}

// SplitHeadKey splits a HeadKey into the fork's owner/name and branch;
// ok is false for a branch of the base repository itself.
func SplitHeadKey(key string) (repo, branch string, ok bool) {
	return strings.Cut(key, ":")
}
