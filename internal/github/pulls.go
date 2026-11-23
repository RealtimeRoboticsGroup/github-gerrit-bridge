package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
	// The merge settings: which merges the button offers, and what
	// message squash and merge commits get ("PR_BODY": the PR's
	// description). GitHub says them only to those who may administer
	// the repository; unsaid, they are nil and "".
	AllowSquashMerge         *bool  `json:"allow_squash_merge"`
	AllowMergeCommit         *bool  `json:"allow_merge_commit"`
	AllowRebaseMerge         *bool  `json:"allow_rebase_merge"`
	SquashMergeCommitMessage string `json:"squash_merge_commit_message"`
	MergeCommitMessage       string `json:"merge_commit_message"`
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
	// MergeCommitSHA is, once merged, the commit the merge made on the
	// base branch; MergedBy (GetPull only) who merged it.
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergedBy       *User  `json:"merged_by"`
	// MergeableState (GetPull only) is what GitHub's merge box says:
	// clean, unstable, has_hooks (mergeable), blocked, behind, dirty,
	// draft, or unknown (not worked out yet).
	MergeableState string `json:"mergeable_state"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	HTMLURL        string `json:"html_url"`
	User           User   `json:"user"`
	Head           Ref    `json:"head"`
	Base           Ref    `json:"base"`
	// MaintainerCanModify is set when a fork PR's author lets people with
	// write access to the base repository push to the head branch. Only
	// GetPull returns it; listed PRs always say false.
	MaintainerCanModify bool `json:"maintainer_can_modify"`
	// RequestedReviewers are the users asked to review who have not yet.
	RequestedReviewers []User    `json:"requested_reviewers"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// Stack is set when the PR is in a native stack.
	Stack *PRStack `json:"stack"`
	// Commits (GetPull only) is how many commits the PR has; its commits
	// list stops at 250.
	Commits int `json:"commits"`
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

// MergeInput is how to merge a PR: Method is "squash" or "merge"; the
// merge happens only if the PR's head is still SHA.
type MergeInput struct {
	Title   string `json:"commit_title"`
	Message string `json:"commit_message"`
	SHA     string `json:"sha"`
	Method  string `json:"merge_method"`
	// Stacked is set for a PR in a native stack, which GitHub merges only
	// asynchronously: that merges it and the PRs below it in the stack.
	// Base is the branch it must still be based on then.
	Stacked bool   `json:"-"`
	Base    string `json:"-"`
}

// MergePull merges PR n as the client's token's user, and returns the
// commit the merge made.
func (c *Client) MergePull(ctx context.Context, n int, m MergeInput) (string, error) {
	if m.Stacked {
		return c.mergeAsync(ctx, n, m)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	_, err := c.Do(ctx, http.MethodPut, c.RepoPath("pulls/"+strconv.Itoa(n)+"/merge"), m, &out)
	return out.SHA, err
}

// asyncMergePoll is how often, and asyncMergeTries how many times, an
// asynchronous merge's result is asked for.
var (
	asyncMergePoll  = 2 * time.Second
	asyncMergeTries = 30
)

// mergeAsync is MergePull for a stacked PR: GitHub's merge-async, merging
// directly (never queued, and no rule bypassed: the button's default),
// then its result. GitHub's refusals, then or while it runs, are
// APIErrors with its own message.
func (c *Client) mergeAsync(ctx context.Context, n int, m MergeInput) (string, error) {
	type result struct {
		Status  string `json:"status"` // pending, merged, enqueued, failed
		Details struct {
			UUID    string `json:"uuid"`
			SHA     string `json:"sha"`
			Message string `json:"message"`
		} `json:"details"`
	}
	in := struct {
		MergeInput
		Action string `json:"merge_action"`
	}{m, "direct_merge"}
	// GitHub merges the PRs below it too, and its sha guards only this
	// one's head: read again, as whoever merges, it must still be the
	// stack's bottom, on the branch, at the head they saw. (A restack in
	// the moment between this read and the merge is not caught: GitHub
	// has no guard for a stack's shape.)
	pr, err := c.GetPull(ctx, n)
	if err != nil {
		return "", err
	}
	if pr.Head.SHA != m.SHA || m.Base == "" || pr.Base.Ref != m.Base || (pr.Stack != nil && pr.Stack.Position != 1) {
		return "", &APIError{Method: http.MethodGet, Path: c.RepoPath("pulls/" + strconv.Itoa(n)), Status: http.StatusConflict,
			Message: "The PR or its stack changed (it must be the bottom of its stack, on " + m.Base + ", at the head you saw): look again, then post /submit again."}
	}
	path := c.RepoPath("pulls/" + strconv.Itoa(n) + "/merge-async")
	var r result
	// A refusal (409: another merge of it is pending) is GitHub's answer.
	if _, err := c.Do(ctx, http.MethodPut, path, in, &r); err != nil {
		return "", err
	}
	uuid := r.Details.UUID // what is polled for
	for try := 0; ; try++ {
		switch r.Status {
		case "merged":
			if r.Details.SHA == "" {
				return "", &APIError{Method: http.MethodPut, Path: path, Status: http.StatusAccepted, Message: "GitHub merged it but did not say as which commit. Gerrit shows the change merged once it has it."}
			}
			return r.Details.SHA, nil
		case "failed":
			return "", &APIError{Method: http.MethodPut, Path: path, Status: http.StatusMethodNotAllowed, Message: cmp.Or(r.Details.Message, "The merge failed.")}
		case "pending":
		default: // enqueued, or anything new: not merged
			return "", &APIError{Method: http.MethodPut, Path: path, Status: http.StatusAccepted, Message: "GitHub did not merge it directly (status " + r.Status + ")."}
		}
		if uuid == "" || try >= asyncMergeTries {
			return "", &APIError{Method: http.MethodPut, Path: path, Status: http.StatusAccepted, Message: "GitHub is still merging it. Gerrit shows the change merged once it is."}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(asyncMergePoll):
		}
		r = result{}
		if _, err := c.Do(ctx, http.MethodGet, path+"/"+url.PathEscape(uuid), nil, &r); err != nil {
			return "", err
		}
	}
}

// PullCommit is one of a PR's own commits.
type PullCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// PullCommits lists PR n's own commits (GitHub lists at most 250).
func (c *Client) PullCommits(ctx context.Context, n int) ([]PullCommit, error) {
	return list[PullCommit](ctx, c, c.RepoPath("pulls/"+strconv.Itoa(n)+"/commits"))
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

// Push is a push to a branch: Actor moved it from Before to After (a
// creation is from 000…, a deletion to it).
type Push struct {
	Before, After string
	Actor         User
	At            time.Time // when, as GitHub says
}

// Pushes lists the newest few hundred pushes to branch ref
// (refs/heads/...) of repo (owner/name), newest first, from GitHub's
// activity log: pushes, force pushes and branch creations and deletions,
// by whoever did them, kept by GitHub across the bridge's restarts. It
// can lag a push by a little.
func (c *Client) Pushes(ctx context.Context, repo, ref string) ([]Push, error) {
	var pushes []Push
	next := "/repos/" + repo + "/activity?per_page=100&ref=" + url.QueryEscape(ref)
	for page := 0; page < 3 && next != ""; page++ {
		var acts []struct {
			Before string    `json:"before"`
			After  string    `json:"after"`
			Ref    string    `json:"ref"`
			Actor  *User     `json:"actor"`
			At     time.Time `json:"timestamp"`
		}
		resp, err := c.Do(ctx, http.MethodGet, next, nil, &acts)
		if err != nil {
			return nil, err
		}
		for _, a := range acts {
			if a.Ref == ref {
				p := Push{Before: a.Before, After: a.After, At: a.At}
				if a.Actor != nil {
					p.Actor = *a.Actor
				}
				pushes = append(pushes, p)
			}
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return pushes, nil
}

// BranchProtected reports whether GitHub protects branch: classic branch
// protection, or any rule of a ruleset that applies to it. A branch that
// does not exist yet may still be covered by a ruleset.
func (c *Client) BranchProtected(ctx context.Context, branch string) (bool, error) {
	var b struct {
		Protected bool `json:"protected"`
	}
	_, err := c.Do(ctx, http.MethodGet, c.RepoPath("branches/"+escapePath(branch)), nil, &b)
	var ae *APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
	case err != nil:
		return false, err
	case b.Protected:
		return true, nil
	}
	var rules []json.RawMessage
	_, err = c.Do(ctx, http.MethodGet, c.RepoPath("rules/branches/"+escapePath(branch)), nil, &rules)
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return false, nil
	}
	return len(rules) > 0, err
}
