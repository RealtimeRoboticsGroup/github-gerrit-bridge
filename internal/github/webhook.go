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
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxPayload is GitHub's documented webhook payload cap.
const maxPayload = 25 << 20

// maxConcurrentReads bounds deliveries being read at once, and
// maxReadsPerAddress those from one address, so a few slow senders cannot
// take every slot. bodyReadTimeout bounds reading one body.
const (
	maxConcurrentReads = 32
	maxReadsPerAddress = 4
	bodyReadTimeout    = 10 * time.Second
)

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
	// Allowed, if set, are the networks deliveries may come from (GitHub's
	// hook ranges, from api.github.com/meta); others are refused before
	// anything is read.
	Allowed []*net.IPNet
	// TrustedProxies are the reverse proxies in front of the bridge, whose
	// X-Forwarded-For says who sent a request. Without them, everything
	// behind a proxy is one address: the allowlist cannot tell GitHub
	// from anyone, and the per-address limit is shared by all.
	TrustedProxies []*net.IPNet

	// busy bounds the payloads read at once (each up to 25 MB), so
	// unauthenticated senders cannot exhaust memory.
	busyOnce sync.Once
	busy     chan struct{}
	byAddr   map[string]int

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
	addr := h.clientAddr(r)
	if !h.allowed(addr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h.busyOnce.Do(func() { h.busy = make(chan struct{}, maxConcurrentReads) })
	if !h.acquire(addr) {
		// The periodic resync catches up on anything dropped here.
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer h.release(addr)
	// Not supported by every ResponseWriter (tests); the server's
	// ReadTimeout still applies then.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
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

// clientAddr is who sent r (ClientAddr).
func (h *WebhookHandler) clientAddr(r *http.Request) string { return ClientAddr(r, h.TrustedProxies) }

// ClientAddr is who sent r: its peer, or, from a proxy in proxies, the
// nearest address in X-Forwarded-For that is not one of them (the ones
// further left are the client's to write).
func ClientAddr(r *http.Request, proxies []*net.IPNet) string {
	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		addr = r.RemoteAddr
	}
	trusted := func(a string) bool {
		ip := net.ParseIP(a)
		for _, n := range proxies {
			if ip != nil && n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(addr) {
		return addr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !trusted(hop) {
			return hop
		}
		addr = hop
	}
	return addr
}

func (h *WebhookHandler) allowed(addr string) bool {
	if len(h.Allowed) == 0 {
		return true
	}
	ip := net.ParseIP(addr)
	for _, n := range h.Allowed {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// acquire takes a read slot for addr, if one is free.
func (h *WebhookHandler) acquire(addr string) bool {
	h.mu.Lock()
	if h.byAddr == nil {
		h.byAddr = map[string]int{}
	}
	if h.byAddr[addr] >= maxReadsPerAddress {
		h.mu.Unlock()
		return false
	}
	h.byAddr[addr]++
	h.mu.Unlock()
	select {
	case h.busy <- struct{}{}:
		return true
	default:
		h.mu.Lock()
		h.byAddr[addr]--
		h.mu.Unlock()
		return false
	}
}

func (h *WebhookHandler) release(addr string) {
	<-h.busy
	h.mu.Lock()
	if h.byAddr[addr]--; h.byAddr[addr] <= 0 {
		delete(h.byAddr, addr)
	}
	h.mu.Unlock()
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
