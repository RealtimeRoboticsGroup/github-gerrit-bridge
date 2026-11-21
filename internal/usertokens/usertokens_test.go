package usertokens

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

func testKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestStoreIsEncryptedAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	s := &Store{Path: path, Key: testKey()}
	if err := s.Load(); err != nil || len(s.All()) != 0 {
		t.Fatalf("empty: %v %v", s.All(), err)
	}
	r := Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_secretaccess", Refresh: "ghr_secretrefresh"}}
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("ghu_secret")) || bytes.Contains(raw, []byte("ghr_secret")) || bytes.Contains(raw, []byte("alice")) {
		t.Errorf("file holds plain text: %q", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("file mode %v", fi.Mode())
	}
	s2 := &Store{Path: path, Key: testKey()}
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get(42); !ok || got.Token.Access != "ghu_secretaccess" || got.GerritAccount != 1000 {
		t.Errorf("loaded %+v %v", got, ok)
	}
	if err := (&Store{Path: path, Key: bytes.Repeat([]byte{8}, 32)}).Load(); err == nil {
		t.Error("loaded with the wrong key")
	}
	raw[len(raw)-1] ^= 1
	os.WriteFile(path, raw, 0o600)
	if err := (&Store{Path: path, Key: testKey()}).Load(); err == nil {
		t.Error("loaded a tampered file")
	}

	// One GitHub account per Gerrit account: the last linked.
	r2 := r
	r2.GitHub = github.User{Login: "alice2", ID: 43}
	if err := s2.Put(r2); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get(42); ok {
		t.Error("an older link to the same Gerrit account kept")
	}
	if err := s2.Delete(43); err != nil || len(s2.All()) != 0 {
		t.Errorf("delete: %v %v", s2.All(), err)
	}
}

func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	hexPath, rawPath, bad := filepath.Join(dir, "hex"), filepath.Join(dir, "raw"), filepath.Join(dir, "bad")
	os.WriteFile(hexPath, []byte(strings.Repeat("07", 32)+"\n"), 0o600)
	os.WriteFile(rawPath, testKey(), 0o600)
	os.WriteFile(bad, []byte("short"), 0o600)
	for _, p := range []string{hexPath, rawPath} {
		if k, err := ReadKey(p); err != nil || !bytes.Equal(k, testKey()) {
			t.Errorf("%s: %x %v", p, k, err)
		}
	}
	if _, err := ReadKey(bad); err == nil {
		t.Error("short key read")
	}
}

// fakeGitHub answers the token endpoint and /user.
type fakeGitHub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	refresh  string                 // the refresh token it takes
	users    map[string]github.User // access token -> account
	next     int
	verifier string            // code_verifier last sent
	codes    map[string]string // code -> login
	rename   string            // if set, /user answers this login
	otherID  bool              // if set, /user answers another account
	// refreshes counts refresh requests.
	refreshes int
	// gate, if set, holds each refresh's answer: it receives once the
	// answer is made, and the answer goes once it receives again.
	gate chan struct{}
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{users: map[string]github.User{}, codes: map[string]string{}}
	var handle func(w http.ResponseWriter, r *http.Request)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.mu.Lock()
		handle(rec, r)
		gate := f.gate
		f.mu.Unlock()
		if gate != nil && r.PostForm.Get("grant_type") == "refresh_token" {
			gate <- struct{}{}
			<-gate
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	handle = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			login := ""
			switch {
			case r.PostForm.Get("grant_type") == "refresh_token":
				f.refreshes++
				if r.PostForm.Get("refresh_token") != f.refresh {
					fmt.Fprint(w, `{"error":"bad_refresh_token"}`)
					return
				}
				for _, u := range f.users {
					login = u.Login
				}
			default:
				f.verifier = r.PostForm.Get("code_verifier")
				login = f.codes[r.PostForm.Get("code")]
				if login == "" {
					fmt.Fprint(w, `{"error":"bad_verification_code"}`)
					return
				}
			}
			f.next++
			access, refresh := fmt.Sprintf("ghu_%d", f.next), fmt.Sprintf("ghr_%d", f.next)
			id := int64(42)
			if login == "bob" {
				id = 7
			}
			f.users[access] = github.User{Login: login, ID: id}
			f.refresh = refresh
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":28800,"refresh_token":%q,"refresh_token_expires_in":15897600}`, access, refresh)
		case "/user":
			u, ok := f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			if !ok {
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
				return
			}
			if f.rename != "" {
				u.Login = f.rename
			}
			if f.otherID {
				u.ID++
			}
			fmt.Fprintf(w, `{"login":%q,"id":%d}`, u.Login, u.ID)
		default:
			http.NotFound(w, r)
		}
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) app() *github.UserApp {
	return &github.UserApp{ClientID: "Iv1.c", ClientSecret: "s", WebURL: f.srv.URL, APIURL: f.srv.URL}
}

func TestTokensRenewAndDrop(t *testing.T) {
	f := newFakeGitHub(t)
	ctx := context.Background()
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	now := time.Now()
	f.refresh = "ghr_0"
	f.users["ghu_0"] = github.User{Login: "alice", ID: 42}
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_0", AccessExpires: now.Add(time.Hour), Refresh: "ghr_0", RefreshExpires: now.Add(24 * time.Hour)}})
	maps, unknown := true, error(nil)
	unknownRenamed := false // the directory cannot say who the new login is
	tk := &Tokens{Store: s, App: f.app(), Check: func(_ context.Context, gh github.User, user string) (bool, error) {
		if unknownRenamed && gh.Login == "alice-renamed" {
			return false, errors.New("the directory cannot be read")
		}
		return maps && gh.ID == 42 && user == "alice", unknown
	}}

	if tok, gh, ok, err := tk.For(ctx, 1000); err != nil || !ok || tok != "ghu_0" || gh.Login != "alice" {
		t.Fatalf("fresh: %q %+v %v %v", tok, gh, ok, err)
	}
	if _, _, ok, _ := tk.For(ctx, 1001); ok {
		t.Error("an unlinked account has a token")
	}
	maps = false
	if _, _, ok, _ := tk.For(ctx, 1000); ok {
		t.Error("used though the account no longer maps")
	}
	maps = true

	// Close to expiry: renewed, and the new refresh token kept on disk.
	tk.now = func() time.Time { return now.Add(56 * time.Minute) }
	tok, _, ok, err := tk.For(ctx, 1000)
	if err != nil || !ok || tok != "ghu_1" {
		t.Fatalf("renewed: %q %v %v", tok, ok, err)
	}
	s2 := &Store{Path: s.Path, Key: testKey()}
	s2.Load()
	if r, _ := s2.Get(42); r.Token.Refresh != "ghr_1" || r.Token.Access != "ghu_1" {
		t.Errorf("saved %+v", r.Token)
	}

	// Renamed on GitHub: the new login is kept.
	f.mu.Lock()
	f.rename = "alice-renamed"
	f.mu.Unlock()
	r, _ := s.Get(42)
	r.Token.AccessExpires = tk.clock().Add(time.Minute)
	s.Put(r)
	// Who the new login is cannot be told for now: an error, not "not
	// linked" (sixteenth review: untested), and the link stays.
	unknownRenamed = true
	if _, _, ok, err := tk.For(ctx, 1000); ok || err == nil {
		t.Errorf("renamed, unknown: %v %v, want an error", ok, err)
	}
	if _, ok := s.Get(42); !ok {
		t.Fatal("renamed, unknown: link dropped")
	}
	unknownRenamed = false
	if _, gh, ok, err := tk.For(ctx, 1000); err != nil || !ok || gh.Login != "alice-renamed" {
		t.Errorf("renamed: %+v %v %v", gh, ok, err)
	}

	// A refresh token GitHub no longer takes drops the link.
	r, _ = s.Get(42)
	r.Token.AccessExpires, r.Token.Refresh = tk.clock().Add(time.Minute), "ghr_revoked"
	s.Put(r)
	if _, _, ok, err := tk.For(ctx, 1000); ok || err != nil {
		t.Errorf("bad refresh: %v %v", ok, err)
	}
	if _, ok := s.Get(42); ok {
		t.Error("link kept after its refresh token went bad")
	}

	// So does an expired refresh token (one GitHub would still take,
	// here), a token that acts as someone else, and a revoked one.
	f.mu.Lock()
	good := f.refresh
	f.mu.Unlock()
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_x", AccessExpires: now, Refresh: good, RefreshExpires: now.Add(time.Minute)}})
	if _, _, ok, _ := tk.For(ctx, 1000); ok {
		t.Error("used with an expired refresh token")
	}
	if _, ok := s.Get(42); ok {
		t.Error("link kept after its refresh token expired")
	}
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_x", AccessExpires: now, Refresh: good, RefreshExpires: now.Add(48 * time.Hour)}})
	f.mu.Lock()
	f.rename, f.otherID = "", true
	f.mu.Unlock()
	if _, _, ok, _ := tk.For(ctx, 1000); ok {
		t.Error("used a token that acts as another account")
	}
	if _, ok := s.Get(42); ok {
		t.Error("link kept after its token acted as another account")
	}
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice", Token: github.UserToken{Access: "ghu_y"}})
	tk.Revoke(42, "ghu_other")
	if _, ok := s.Get(42); !ok {
		t.Error("revoking another token dropped the link")
	}
	if gh, ok, err := tk.Linked(ctx, 1000); !ok || gh.ID != 42 || err != nil {
		t.Errorf("Linked = %+v %v %v", gh, ok, err)
	}
	if _, ok, _ := tk.Linked(ctx, 1001); ok {
		t.Error("an unlinked account is linked")
	}
	// Nor is one whose GitHub account no longer maps to it.
	maps = false
	if gh, ok, err := tk.Linked(ctx, 1000); ok || err != nil {
		t.Errorf("Linked = %+v %v though the account no longer maps", gh, err)
	}
	maps = true
	// While the mapping is unknown, neither says yes or no: an error
	// (fifteenth review: unknown was "not linked", and posted as the bot).
	unknown = errors.New("the directory cannot be read")
	if _, ok, err := tk.Linked(ctx, 1000); ok || err == nil {
		t.Errorf("Linked, mapping unknown: %v %v", ok, err)
	}
	if _, _, ok, err := tk.For(ctx, 1000); ok || err == nil {
		t.Errorf("For, mapping unknown: %v %v", ok, err)
	}
	if _, ok := s.Get(42); !ok {
		t.Error("an unknown mapping dropped the link")
	}
	unknown = nil
	// A 401 on the token GitHub was given drops the link.
	src := tk.Source(42, "ghu_y")
	if tok, _ := src.Token(ctx); tok != "ghu_y" {
		t.Errorf("source token %q", tok)
	}
	src.(interface{ Invalidate() }).Invalidate()
	if _, ok := s.Get(42); ok {
		t.Error("revoked token kept")
	}
}

func TestLink(t *testing.T) {
	f := newFakeGitHub(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	unknown := false
	l := &Linker{App: f.app(), Tokens: &Tokens{Store: s, App: f.app()}, PublicURL: "https://bridge.example",
		GerritUser: func(_ context.Context, gh github.User) (string, bool, error) {
			if unknown {
				return "", false, errors.New("the directory cannot be read")
			}
			if gh.ID == 42 && gh.Login == "alice" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		GerritAccount: func(_ context.Context, user string) (int, error) { return 1000, nil }}
	mux := http.NewServeMux()
	l.Register(mux)
	do := func(method, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if method == "POST" {
			req.Header.Set("Origin", "https://bridge.example")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// Following the link only says what it does.
	if w := do("GET", "/github/link", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `method="post"`) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("intro: %d %s", w.Code, w.Body)
	}
	// It says what the token can do: where GitHub owns the branches,
	// push and merge as them too (thirteenth review).
	if w := do("GET", "/github/link", nil); !strings.Contains(w.Body.String(), "can only comment on and review") {
		t.Errorf("intro: %s", w.Body)
	}
	l.GitHubOwned = true
	if w := do("GET", "/github/link", nil); !strings.Contains(w.Body.String(), "push to and merge pull requests as you") ||
		strings.Contains(w.Body.String(), "can only") {
		t.Errorf("intro where GitHub owns the branches: %s", w.Body)
	}
	l.GitHubOwned = false
	start := func() (state string, cookie *http.Cookie) {
		w := do("POST", "/github/link", nil)
		if w.Code != http.StatusFound {
			t.Fatalf("start: %d", w.Code)
		}
		u, _ := url.Parse(w.Header().Get("Location"))
		if !strings.HasPrefix(u.String(), f.srv.URL+"/login/oauth/authorize?") || u.Query().Get("redirect_uri") != "https://bridge.example/github/link/callback" {
			t.Fatalf("redirect %s", u)
		}
		cs := w.Result().Cookies()
		if len(cs) != 1 || cs[0].Value != u.Query().Get("state") || !cs[0].HttpOnly || !cs[0].Secure {
			t.Fatalf("cookie %+v", cs)
		}
		return u.Query().Get("state"), cs[0]
	}

	// Only the bridge's own page starts a link: a form on another site
	// would link with no click where GitHub skips its consent screen.
	for _, h := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://bridge.example", "Sec-Fetch-Site": "same-site"},
		{"Origin": "null"},
		{},
	} {
		req := httptest.NewRequest("POST", "/github/link", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Errorf("POST with %v: %d, cookies %v", h, w.Code, w.Result().Cookies())
		}
	}
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "same-origin"}, {"Origin": "HTTPS://bridge.example"}} {
		req := httptest.NewRequest("POST", "/github/link", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Errorf("POST with %v: %d", h, w.Code)
		}
	}

	// GitHub's redirect after an install links nobody, and says how to.
	if w := do("GET", "/github/link/callback?code=c1&installation_id=5&setup_action=install", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "installed") || !strings.Contains(w.Body.String(), `method="post"`) || len(s.All()) != 0 {
		t.Errorf("after install: %d %s", w.Code, w.Body)
	}

	// A callback without this browser's cookie (someone else's link) is
	// refused.
	f.codes["c1"] = "alice"
	state, cookie := start()
	if w := do("GET", "/github/link/callback?code=c1&state="+state, &http.Cookie{Name: cookieName, Value: "other"}); w.Code != http.StatusBadRequest {
		t.Errorf("wrong cookie: %d", w.Code)
	}
	if len(s.All()) != 0 {
		t.Fatal("linked without the cookie")
	}
	// Whose account it is, unknown for now: try later, not "no Gerrit
	// account" (fifteenth review).
	unknown = true
	state, cookie = start()
	if w := do("GET", "/github/link/callback?code=c1&state="+state, cookie); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "Try again later") || len(s.All()) != 0 {
		t.Errorf("unknown: %d %s", w.Code, w.Body)
	}
	unknown = false
	// Nobody's, where a directory says who is whose: ask its administrators.
	l.Directory = true
	f.codes["c2"] = "stranger"
	state, cookie = start()
	if w := do("GET", "/github/link/callback?code=c2&state="+state, cookie); !strings.Contains(w.Body.String(), "Ask an administrator") ||
		strings.Contains(w.Body.String(), "Sign in to Gerrit with GitHub") || len(s.All()) != 0 {
		t.Errorf("not in the directory: %d %s", w.Code, w.Body)
	}
	l.Directory = false

	state, cookie = start()
	w := do("GET", "/github/link/callback?code=c1&state="+state, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Linked") {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	r, ok := s.Get(42)
	if !ok || r.GerritAccount != 1000 || r.GerritUser != "alice" || r.Token.Access == "" || r.Token.Refresh == "" {
		t.Fatalf("record %+v %v", r, ok)
	}
	if f.verifier == "" {
		t.Error("no PKCE verifier sent")
	}
	if strings.Contains(w.Body.String(), r.Token.Access) {
		t.Error("the page shows the token")
	}
	// Once only.
	if w := do("GET", "/github/link/callback?code=c1&state="+state, cookie); w.Code != http.StatusBadRequest {
		t.Errorf("replayed: %d", w.Code)
	}

	// Someone without a Gerrit account is not linked.
	f.codes["c2"] = "bob"
	state, cookie = start()
	if w := do("GET", "/github/link/callback?code=c2&state="+state, cookie); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Not linked") {
		t.Errorf("bob: %d %s", w.Code, w.Body)
	}
	if _, ok := s.Get(7); ok {
		t.Error("bob linked")
	}

	// Expired.
	state, cookie = start()
	l.now = func() time.Time { return time.Now().Add(linkTTL + time.Minute) }
	if w := do("GET", "/github/link/callback?code=c1&state="+state, cookie); w.Code != http.StatusBadRequest {
		t.Errorf("expired: %d", w.Code)
	}
	l.now = nil

	// A state the bridge did not make, or one altered, is refused, even
	// with a cookie to match.
	state, _ = start()
	b, _ := base64.RawURLEncoding.DecodeString(state)
	b[10] ^= 1
	for _, bad := range []string{base64.RawURLEncoding.EncodeToString(b), "x", "AAAA", strings.Repeat("A", 54)} {
		if w := do("GET", "/github/link/callback?code=c1&state="+bad, &http.Cookie{Name: cookieName, Value: bad}); w.Code != http.StatusBadRequest {
			t.Errorf("forged state %q: %d", bad, w.Code)
		}
	}
	if len(s.All()) != 1 {
		t.Errorf("linked by a forged state: %+v", s.All())
	}

	// Starting a link keeps nothing on the bridge: anyone can, as often
	// as they like, and still others can link.
	for range 5000 {
		do("POST", "/github/link", nil)
	}
	l.mu.Lock()
	n := len(l.used)
	l.mu.Unlock()
	if n != 4 { // alice's (and while unknown), the stranger's and bob's, which GitHub authorized
		t.Errorf("%d states kept", n)
	}
	s.Delete(42)
	state, cookie = start()
	if w := do("GET", "/github/link/callback?code=c1&state="+state, cookie); !strings.Contains(w.Body.String(), "Linked") {
		t.Errorf("after many starts: %d %s", w.Code, w.Body)
	}
}

// A link made while the same Gerrit account's token is being renewed
// stands: the renewal does not write back the old link over it (tenth
// audit).
func TestLinkDuringRenewalStands(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	now := time.Now()
	f.refresh = "ghr_0"
	f.users["ghu_0"] = github.User{Login: "alice", ID: 42}
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_0", AccessExpires: now, Refresh: "ghr_0", RefreshExpires: now.Add(time.Hour)}})
	tk := &Tokens{Store: s, App: f.app()}
	l := &Linker{App: f.app(), Tokens: tk, PublicURL: "https://bridge.example",
		GerritUser:    func(_ context.Context, gh github.User) (string, bool, error) { return gh.Login, true, nil },
		GerritAccount: func(context.Context, string) (int, error) { return 1000, nil }}
	mux := http.NewServeMux()
	l.Register(mux)

	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		tk.For(ctx, 1000)
	}()
	<-gate // the refresh is answered, not yet received

	req := httptest.NewRequest("POST", "/github/link", nil)
	req.Header.Set("Origin", "https://bridge.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	u, _ := url.Parse(w.Header().Get("Location"))
	f.mu.Lock()
	f.codes["c"] = "bob"
	f.mu.Unlock()
	linked := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("GET", "/github/link/callback?code=c&state="+u.Query().Get("state"), nil)
		req.AddCookie(w.Result().Cookies()[0])
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		linked <- w
	}()
	time.Sleep(200 * time.Millisecond) // the link saves now, unless it waits
	gate <- struct{}{}
	<-renewed
	if w := <-linked; !strings.Contains(w.Body.String(), "Linked") {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	if r, ok := s.Get(7); !ok || r.GerritAccount != 1000 {
		t.Errorf("bob's new link is gone: %+v", s.All())
	}
	if _, ok := s.Get(42); ok {
		t.Errorf("alice's old link came back: %+v", s.All())
	}
}

// Renewals of one token at once make one refresh: GitHub voids a refresh
// token on use, so a second with it would be refused and drop the link.
func TestConcurrentRenewalsRefreshOnce(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	now := time.Now()
	f.refresh = "ghr_0"
	f.users["ghu_0"] = github.User{Login: "alice", ID: 42}
	s.Put(Record{GitHub: github.User{Login: "alice", ID: 42}, GerritAccount: 1000, GerritUser: "alice",
		Token: github.UserToken{Access: "ghu_0", AccessExpires: now, Refresh: "ghr_0", RefreshExpires: now.Add(time.Hour)}})
	tk := &Tokens{Store: s, App: f.app()}
	var wg sync.WaitGroup
	oks := make([]bool, 8)
	for i := range oks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, oks[i], _ = tk.For(ctx, 1000)
		}()
	}
	wg.Wait()
	f.mu.Lock()
	n := f.refreshes
	f.mu.Unlock()
	if _, ok := s.Get(42); !ok || n != 1 || slices.Contains(oks, false) {
		t.Errorf("link kept %v, %d refreshes, ok %v", ok, n, oks)
	}
}

// Behind a proxy at a path (public_url https://gerrit.example/bridge), the
// form posts, and the cookie is kept, under that path, as browsers reach
// the page; the bridge still serves it at /github/link (fourteenth
// review: nobody behind such a proxy could link).
func TestLinkUnderAPath(t *testing.T) {
	f := newFakeGitHub(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	l := &Linker{App: f.app(), Tokens: &Tokens{Store: s, App: f.app()}, PublicURL: "https://gerrit.example/bridge/",
		GerritUser:    func(context.Context, github.User) (string, bool, error) { return "", false, nil },
		GerritAccount: func(context.Context, string) (int, error) { return 1000, nil }}
	mux := http.NewServeMux()
	l.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/github/link", nil))
	if !strings.Contains(w.Body.String(), `action="/bridge/github/link"`) {
		t.Errorf("form: %s", w.Body)
	}
	req := httptest.NewRequest("POST", "/github/link", nil)
	req.Header.Set("Origin", "https://gerrit.example")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	cs := w.Result().Cookies()
	if w.Code != http.StatusFound || len(cs) != 1 || cs[0].Path != "/bridge/github/link" {
		t.Errorf("start: %d, cookies %+v", w.Code, cs)
	}
	if !strings.Contains(w.Header().Get("Location"), url.QueryEscape("https://gerrit.example/bridge/github/link/callback")) {
		t.Errorf("callback in %s", w.Header().Get("Location"))
	}
	// The callback clears the cookie where it was kept, and its pages
	// lead back under the path (fifteenth review, tests).
	req = httptest.NewRequest("GET", "/github/link/callback?code=c&state=x", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "other"})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if cs := w.Result().Cookies(); len(cs) != 1 || cs[0].Path != "/bridge/github/link" || cs[0].MaxAge >= 0 {
		t.Errorf("callback cookies %+v", cs)
	}
	if !strings.Contains(w.Body.String(), `href="/bridge/github/link"`) {
		t.Errorf("other browser's page: %s", w.Body)
	}
	req = httptest.NewRequest("POST", "/github/link", nil)
	req.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `href="/bridge/github/link"`) {
		t.Errorf("forbidden page: %d %s", w.Code, w.Body)
	}
}
