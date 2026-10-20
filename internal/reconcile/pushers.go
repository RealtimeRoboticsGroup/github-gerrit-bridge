package reconcile

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Pushers remembers who pushed each GitHub commit to each branch, from
// webhooks, so a new patch set imported from GitHub is uploaded as the
// person who pushed it (design §5.2), not the PR's author. It backs up
// GitHub's activity log, which can lag a push by a little.
type Pushers struct {
	mu     sync.Mutex
	byPush map[push]string
	order  []push
}

// push is a commit pushed to a branch: whoever pushed it to one branch
// did not push it to any other.
type push struct{ repo, ref, sha string }

const maxPushers = 10000

// Record notes that login pushed sha to repo's ref. The latest push
// wins, as in GitHub's activity log. Forgetting the oldest only makes
// Lookup say nobody, which leaves the upload to the bridge.
func (p *Pushers) Record(repo, ref, sha, login string) {
	if repo == "" || ref == "" || sha == "" || login == "" {
		return
	}
	k := push{strings.ToLower(repo), ref, sha}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byPush == nil {
		p.byPush = map[push]string{}
	}
	if _, ok := p.byPush[k]; !ok {
		p.order = append(p.order, k)
		if len(p.order) > maxPushers {
			delete(p.byPush, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.byPush[k] = login
}

// Lookup returns who pushed sha to repo's ref, if a webhook said.
func (p *Pushers) Lookup(repo, ref, sha string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byPush[push{strings.ToLower(repo), ref, sha}]
}

// FromDelivery records the pusher in push and pull_request synchronize
// deliveries: nothing else says who pushed. Not "opened" or "reopened":
// anyone may open a PR from someone else's branch (fourth audit), and
// reopening pushes nothing.
func (p *Pushers) FromDelivery(d *github.Delivery) {
	var payload struct {
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Head struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if json.Unmarshal(d.Payload, &payload) != nil {
		return
	}
	switch {
	case d.Event == "push":
		p.Record(payload.Repository.FullName, payload.Ref, payload.After, payload.Sender.Login)
	case d.Event == "pull_request" && d.Action == "synchronize":
		// A fork's pushes send no push webhook here; this one does. What
		// was pushed is after: the PR's head, read when the delivery was
		// built, may already be a later push's (sixth audit).
		head := payload.PullRequest.Head
		if head.Ref != "" {
			p.Record(head.Repo.FullName, "refs/heads/"+head.Ref, payload.After, payload.Sender.Login)
		}
	}
}
