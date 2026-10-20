package reconcile

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Pushers remembers who pushed each GitHub commit to each branch, and
// from what, from webhooks, so a new patch set imported from GitHub is
// uploaded as the person who pushed it (design §5.2), not the PR's
// author. It backs up GitHub's activity log, which can lag a push by a
// little.
type Pushers struct {
	mu     sync.Mutex
	byPush map[push]github.Push
	order  []push
}

// push is a commit pushed to a branch: whoever pushed it to one branch
// did not push it to any other.
type push struct{ repo, ref, sha string }

const maxPushers = 10000

// Record notes that actor moved repo's ref from before to after. The
// latest push wins, as in GitHub's activity log. Forgetting the oldest
// only makes Lookup say nobody, which leaves the upload to the bridge.
func (p *Pushers) Record(repo, ref, before, after string, actor github.User) {
	if repo == "" || ref == "" || after == "" || actor.Login == "" {
		return
	}
	k := push{strings.ToLower(repo), ref, after}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byPush == nil {
		p.byPush = map[push]github.Push{}
	}
	if _, ok := p.byPush[k]; !ok {
		p.order = append(p.order, k)
		if len(p.order) > maxPushers {
			delete(p.byPush, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.byPush[k] = github.Push{Before: before, After: after, Actor: actor}
}

// Lookup returns the push that moved repo's ref to sha, if a webhook
// said.
func (p *Pushers) Lookup(repo, ref, sha string) (github.Push, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.byPush[push{strings.ToLower(repo), ref, sha}]
	return r, ok
}

// FromDelivery records the push in push and pull_request synchronize
// deliveries: nothing else says who pushed. Not "opened" or "reopened":
// anyone may open a PR from someone else's branch (fourth audit), and
// reopening pushes nothing.
func (p *Pushers) FromDelivery(d *github.Delivery) {
	var payload struct {
		Sender     github.User `json:"sender"`
		Ref        string      `json:"ref"`
		Before     string      `json:"before"`
		After      string      `json:"after"`
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
		p.Record(payload.Repository.FullName, payload.Ref, payload.Before, payload.After, payload.Sender)
	case d.Event == "pull_request" && d.Action == "synchronize":
		// A fork's pushes send no push webhook here; this one does. What
		// was pushed is after: the PR's head, read when the delivery was
		// built, may already be a later push's (sixth audit).
		head := payload.PullRequest.Head
		if head.Ref != "" {
			p.Record(head.Repo.FullName, "refs/heads/"+head.Ref, payload.Before, payload.After, payload.Sender)
		}
	}
}
