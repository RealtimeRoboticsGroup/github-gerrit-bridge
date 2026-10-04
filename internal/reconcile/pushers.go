package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Pushers remembers who pushed each GitHub commit, from webhooks, so a
// new patch set imported from GitHub is uploaded as the person who pushed
// it (design §5.2), not the PR's author.
type Pushers struct {
	mu    sync.Mutex
	bySHA map[string]string
	order []string
}

const maxPushers = 10000

// Record notes that login pushed sha.
func (p *Pushers) Record(sha, login string) {
	if sha == "" || login == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bySHA == nil {
		p.bySHA = map[string]string{}
	}
	if _, ok := p.bySHA[sha]; !ok {
		p.order = append(p.order, sha)
		if len(p.order) > maxPushers {
			delete(p.bySHA, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.bySHA[sha] = login
}

// Lookup returns who pushed sha, if a webhook said.
func (p *Pushers) Lookup(sha string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bySHA[sha]
}

// FromDelivery records the pusher in pull_request (opened, reopened,
// synchronize) and push deliveries.
func (p *Pushers) FromDelivery(d *github.Delivery) {
	var payload struct {
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		After       string `json:"after"`
		PullRequest struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if json.Unmarshal(d.Payload, &payload) != nil {
		return
	}
	switch {
	case d.Event == "push":
		p.Record(payload.After, payload.Sender.Login)
	case d.Event == "pull_request" && (d.Action == "opened" || d.Action == "reopened" || d.Action == "synchronize"):
		p.Record(payload.PullRequest.Head.SHA, payload.Sender.Login)
	}
}

// commitPusher guesses who pushed sha when no webhook said: the GitHub
// user GitHub attributes the commit's committer email to (web edits are
// committed by "web-flow", so then the author).
func commitPusher(ctx context.Context, gh *github.Client, sha string) string {
	var c struct {
		Author, Committer *struct {
			Login string `json:"login"`
		}
	}
	if _, err := gh.Do(ctx, http.MethodGet, gh.RepoPath("commits/"+sha), nil, &c); err != nil {
		return ""
	}
	if c.Committer != nil && c.Committer.Login != "" && c.Committer.Login != "web-flow" {
		return c.Committer.Login
	}
	if c.Author != nil {
		return c.Author.Login
	}
	return ""
}
