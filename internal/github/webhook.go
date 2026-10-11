// Package github talks to GitHub: webhook ingest for now, REST and GraphQL
// clients later.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// maxPayload is GitHub's documented webhook payload cap.
const maxPayload = 25 << 20

// maxConcurrentReads bounds deliveries being read at once.
const maxConcurrentReads = 8

// Delivery is a verified webhook delivery, reduced to what routing needs.
// Like Gerrit events, it is a hint: the reconciler re-reads PR state.
type Delivery struct {
	ID     string // X-GitHub-Delivery
	Event  string // X-GitHub-Event
	Action string
	Repo   string // owner/name
	// PR is the pull request the delivery is about, or 0.
	PR      int
	Payload json.RawMessage
}

// WebhookHandler verifies and routes GitHub webhook deliveries.
type WebhookHandler struct {
	Secret []byte
	// Repo is the owner/name deliveries must be for; others are ignored.
	Repo string
	// Deliver is called for each new delivery. It must not block.
	Deliver func(*Delivery)
	Logger  *slog.Logger

	// busy bounds the payloads read at once (each up to 25 MB), so
	// unauthenticated senders cannot exhaust memory.
	busyOnce sync.Once
	busy     chan struct{}

	mu   sync.Mutex
	seen map[string]struct{}
	ring []string
	next int
}

// recentDeliveries is how many delivery IDs are remembered to drop
// redeliveries.
const recentDeliveries = 4096

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > maxPayload {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	h.busyOnce.Do(func() { h.busy = make(chan struct{}, maxConcurrentReads) })
	select {
	case h.busy <- struct{}{}:
		defer func() { <-h.busy }()
	default:
		// The periodic resync catches up on anything dropped here.
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxPayload {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		logger.Warn("webhook with bad signature", "remote", r.RemoteAddr)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	d := &Delivery{
		ID:      r.Header.Get("X-GitHub-Delivery"),
		Event:   r.Header.Get("X-GitHub-Event"),
		Payload: body,
	}
	if d.Event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var p struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest *struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Issue *struct {
			Number      int              `json:"number"`
			PullRequest *json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	d.Action = p.Action
	d.Repo = p.Repository.FullName
	switch {
	case p.PullRequest != nil:
		d.PR = p.PullRequest.Number
	case p.Issue != nil && p.Issue.PullRequest != nil:
		// issue_comment on a PR.
		d.PR = p.Issue.Number
	}

	if !strings.EqualFold(d.Repo, h.Repo) {
		logger.Debug("ignoring webhook for another repo", "repo", d.Repo, "event", d.Event)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if d.ID != "" && !h.firstSeen(d.ID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	h.Deliver(d)
	w.WriteHeader(http.StatusAccepted)
}

func (h *WebhookHandler) firstSeen(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[string]struct{}{}
		h.ring = make([]string, recentDeliveries)
	}
	if _, ok := h.seen[id]; ok {
		return false
	}
	delete(h.seen, h.ring[h.next])
	h.ring[h.next] = id
	h.next = (h.next + 1) % len(h.ring)
	h.seen[id] = struct{}{}
	return true
}

func validSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}
