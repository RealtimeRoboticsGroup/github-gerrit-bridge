package reconcile

import (
	"encoding/json"
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
	if _, ok := p.bySHA[sha]; ok {
		// The first push of a commit wins: pushing someone else's commit
		// again (say, to test it) does not make it yours.
		return
	}
	p.order = append(p.order, sha)
	if len(p.order) > maxPushers {
		delete(p.bySHA, p.order[0])
		p.order = p.order[1:]
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
	case d.Event == "pull_request" && (d.Action == "opened" || d.Action == "synchronize"):
		// Not "reopened": its sender reopened the PR, and need not have
		// pushed anything.
		p.Record(payload.PullRequest.Head.SHA, payload.Sender.Login)
	}
}
