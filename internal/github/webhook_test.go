package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secret = "s3cret"

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type deliveryRecorder struct{ got []*Delivery }

func (r *deliveryRecorder) handler() *WebhookHandler {
	return &WebhookHandler{
		Secret:  []byte(secret),
		Repo:    "AustinSchuh/bridge-scratch",
		Deliver: func(d *Delivery) { r.got = append(r.got, d) },
	}
}

func post(h http.Handler, event, id, body, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestWebhookRouting(t *testing.T) {
	for _, tc := range []struct {
		event, body string
		wantPR      int
	}{
		{"pull_request", `{"action":"synchronize","number":4,"pull_request":{"number":4},"repository":{"full_name":"austinschuh/Bridge-Scratch"}}`, 4},
		{"pull_request_review_comment", `{"action":"created","pull_request":{"number":2},"repository":{"full_name":"AustinSchuh/bridge-scratch"}}`, 2},
		{"issue_comment", `{"action":"created","issue":{"number":7,"pull_request":{"url":"x"}},"repository":{"full_name":"AustinSchuh/bridge-scratch"}}`, 7},
		{"issue_comment", `{"action":"created","issue":{"number":8},"repository":{"full_name":"AustinSchuh/bridge-scratch"}}`, 0},
	} {
		rec := &deliveryRecorder{}
		h := rec.handler()
		if code := post(h, tc.event, "id-1", tc.body, sign(tc.body)); code != http.StatusAccepted {
			t.Errorf("%s: status %d", tc.event, code)
		}
		if len(rec.got) != 1 || rec.got[0].PR != tc.wantPR || rec.got[0].Event != tc.event {
			t.Errorf("%s: deliveries %+v, want PR %d", tc.event, rec.got, tc.wantPR)
		}
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	rec := &deliveryRecorder{}
	h := rec.handler()
	body := `{"repository":{"full_name":"AustinSchuh/bridge-scratch"}}`
	for _, sig := range []string{"", "sha1=abc", "sha256=zz", sign(body + " ")} {
		if code := post(h, "pull_request", "id", body, sig); code != http.StatusUnauthorized {
			t.Errorf("sig %q: status %d, want 401", sig, code)
		}
	}
	// An empty configured secret must never verify.
	h.Secret = nil
	m := hmac.New(sha256.New, nil)
	m.Write([]byte(body))
	if code := post(h, "pull_request", "id", body, "sha256="+hex.EncodeToString(m.Sum(nil))); code != http.StatusUnauthorized {
		t.Errorf("empty secret: status %d", code)
	}
	if len(rec.got) != 0 {
		t.Errorf("delivered %d unverified payloads", len(rec.got))
	}
}

func TestWebhookDedupAndFilter(t *testing.T) {
	rec := &deliveryRecorder{}
	h := rec.handler()
	body := `{"pull_request":{"number":1},"repository":{"full_name":"AustinSchuh/bridge-scratch"}}`
	post(h, "pull_request", "dup", body, sign(body))
	if code := post(h, "pull_request", "dup", body, sign(body)); code != http.StatusOK {
		t.Errorf("redelivery status %d", code)
	}
	other := `{"pull_request":{"number":1},"repository":{"full_name":"someone/else"}}`
	post(h, "pull_request", "other", other, sign(other))
	post(h, "ping", "ping", `{}`, sign(`{}`))
	if len(rec.got) != 1 {
		t.Errorf("got %d deliveries, want 1", len(rec.got))
	}
}

func TestFirstSeenEvicts(t *testing.T) {
	h := &WebhookHandler{}
	h.firstSeen("a")
	for i := range recentDeliveries {
		h.firstSeen(strings.Repeat("x", i+1))
	}
	if !h.firstSeen("a") {
		t.Error("oldest id was not evicted")
	}
	if len(h.seen) != recentDeliveries {
		t.Errorf("seen has %d entries, want %d", len(h.seen), recentDeliveries)
	}
}

// A few slow senders cannot take every read slot: each address gets a
// few, and other addresses still get theirs.
func TestWebhookReadSlotsPerAddress(t *testing.T) {
	h := (&deliveryRecorder{}).handler()
	h.busy = make(chan struct{}, maxConcurrentReads)
	h.busyOnce.Do(func() {})
	for i := 0; i < maxReadsPerAddress; i++ {
		if !h.acquire("203.0.113.9") {
			t.Fatalf("slot %d refused", i)
		}
	}
	if h.acquire("203.0.113.9") {
		t.Error("one address took more than its share")
	}
	if !h.acquire("192.0.2.1") {
		t.Error("another address was refused")
	}
	h.release("203.0.113.9")
	if !h.acquire("203.0.113.9") {
		t.Error("a released slot was not reusable")
	}
}

func TestWebhookAllowedNetworks(t *testing.T) {
	r := &deliveryRecorder{}
	h := r.handler()
	_, gh, _ := net.ParseCIDR("140.82.112.0/20")
	h.Allowed = []*net.IPNet{gh}
	body := `{"action":"opened","repository":{"full_name":"AustinSchuh/bridge-scratch"},"pull_request":{"number":1}}`
	if code := post(h, "pull_request", "d1", body, sign(body)); code != http.StatusForbidden {
		t.Errorf("from 192.0.2.1 (httptest's address): %d", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.RemoteAddr = "140.82.115.1:443"
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || len(r.got) != 1 {
		t.Errorf("from GitHub: %d, %d deliveries", w.Code, len(r.got))
	}
}

// Behind a trusted proxy, the sender is the nearest untrusted hop of
// X-Forwarded-For; from anyone else, the header is ignored.
func TestWebhookClientAddrBehindProxy(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("10.0.0.0/8")
	h := &WebhookHandler{TrustedProxies: []*net.IPNet{proxy}}
	for _, tc := range []struct{ remote, xff, want string }{
		{"10.0.0.5:443", "140.82.115.1", "140.82.115.1"},
		{"10.0.0.5:443", "1.2.3.4, 140.82.115.1", "140.82.115.1"}, // the client wrote 1.2.3.4
		{"10.0.0.5:443", "140.82.115.1, 10.0.0.9", "140.82.115.1"},
		{"10.0.0.5:443", "", "10.0.0.5"},
		{"203.0.113.9:443", "140.82.115.1", "203.0.113.9"}, // not a proxy: ignored
	} {
		r := httptest.NewRequest(http.MethodPost, "/webhook", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := h.clientAddr(r); got != tc.want {
			t.Errorf("%s via %q = %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}

// The allowlist judges the sender, not the proxy in front of the bridge.
func TestWebhookAllowlistBehindProxy(t *testing.T) {
	r := &deliveryRecorder{}
	h := r.handler()
	_, gh, _ := net.ParseCIDR("140.82.112.0/20")
	_, proxy, _ := net.ParseCIDR("10.0.0.0/8")
	h.Allowed, h.TrustedProxies = []*net.IPNet{gh}, []*net.IPNet{proxy}
	body := `{"action":"opened","repository":{"full_name":"AustinSchuh/bridge-scratch"},"pull_request":{"number":1}}`
	for xff, want := range map[string]int{"140.82.115.1": http.StatusAccepted, "203.0.113.9": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.5:443"
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", xff)
		req.Header.Set("X-Hub-Signature-256", sign(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("forwarded for %s: %d, want %d", xff, w.Code, want)
		}
	}
}
