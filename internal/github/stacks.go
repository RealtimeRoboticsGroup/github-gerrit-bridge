package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Stack is a native stacked-PR stack (REST, public preview, July 2026).
//
// A stack has at least two PRs, bottom first. Each PR's base must be the
// previous PR's head branch; the bottom PR may target any branch.
type Stack struct {
	ID           int64     `json:"id"`
	Number       int       `json:"number"`
	NodeID       string    `json:"node_id"`
	Base         Ref       `json:"base"`
	Open         bool      `json:"open"`
	CreatedAt    time.Time `json:"created_at"`
	PullRequests []StackPR `json:"pull_requests"`
}

// StackPR is one layer of a stack.
type StackPR struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	Draft    bool       `json:"draft"`
	MergedAt *time.Time `json:"merged_at"`
	Head     Ref        `json:"head"`
}

// Numbers returns the stack's PR numbers, bottom first.
func (s *Stack) Numbers() []int {
	n := make([]int, len(s.PullRequests))
	for i, p := range s.PullRequests {
		n[i] = p.Number
	}
	return n
}

// ListStacks lists the repository's stacks.
func (c *Client) ListStacks(ctx context.Context) ([]Stack, error) {
	return list[Stack](ctx, c, c.RepoPath("stacks"))
}

// GetStack returns stack n.
func (c *Client) GetStack(ctx context.Context, n int) (*Stack, error) {
	var s Stack
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("stacks/"+strconv.Itoa(n)), nil, &s)
	return &s, err
}

type stackPRs struct {
	PullRequests []int `json:"pull_requests"`
}

// CreateStack stacks prs, bottom first. It needs at least two.
func (c *Client) CreateStack(ctx context.Context, prs []int) (*Stack, error) {
	var s Stack
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("stacks"), stackPRs{prs}, &s)
	return &s, err
}

// AddToStack appends prs to the top of stack n.
func (c *Client) AddToStack(ctx context.Context, n int, prs []int) (*Stack, error) {
	var s Stack
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("stacks/"+strconv.Itoa(n)+"/add"), stackPRs{prs}, &s)
	return &s, err
}

// Unstack dissolves stack n entirely. Its PRs stay open with their bases
// unchanged. Rebuilding is CreateStack with a new number (design §5.4).
func (c *Client) Unstack(ctx context.Context, n int) error {
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("stacks/"+strconv.Itoa(n)+"/unstack"), nil, nil)
	return err
}

// Status is a commit status. State is pending, success, failure or error.
type Status struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
	// UpdatedAt is when GitHub took it (read only).
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// CreateStatus sets the status for context on commit sha.
func (c *Client) CreateStatus(ctx context.Context, sha string, s Status) error {
	_, err := c.Do(ctx, http.MethodPost, c.RepoPath("statuses/"+sha), s, nil)
	return err
}

// CheckRun is a check run on a commit (GitHub Actions' jobs, and other
// apps' checks).
type CheckRun struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`     // queued, in_progress, completed, waiting, requested, pending
	Conclusion  string     `json:"conclusion"` // once completed: success, failure, neutral, cancelled, skipped, timed_out, action_required, stale
	HTMLURL     string     `json:"html_url"`
	DetailsURL  string     `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Output      struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
	App struct {
		Name string `json:"name"`
		Slug string `json:"slug"` // github-actions for GitHub Actions' jobs
	} `json:"app"`
}

// CheckRuns returns the check runs on commit sha (the first 100: every
// attempt of each, newest first).
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("commits/"+url.PathEscape(sha)+"/check-runs?filter=all&per_page=100"), nil, &out)
	return out.CheckRuns, err
}

// Statuses returns the latest status per context on commit sha.
func (c *Client) Statuses(ctx context.Context, sha string) ([]Status, error) {
	var out struct {
		Statuses []Status `json:"statuses"`
	}
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("commits/"+sha+"/status?per_page=100"), nil, &out)
	return out.Statuses, err
}
