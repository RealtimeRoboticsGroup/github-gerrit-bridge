package usertokens

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{users: map[string]github.User{}, codes: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			login := ""
			switch {
			case r.PostForm.Get("grant_type") == "refresh_token":
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
	}))
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
	maps := true
	tk := &Tokens{Store: s, App: f.app(), Check: func(_ context.Context, gh github.User, user string) bool {
		return maps && gh.ID == 42 && user == "alice"
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
	tk.Revoke(42, "ghu_y")
	if _, ok := s.Get(42); ok {
		t.Error("revoked token kept")
	}
}

func TestLink(t *testing.T) {
	f := newFakeGitHub(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "tokens"), Key: testKey()}
	l := &Linker{App: f.app(), Tokens: &Tokens{Store: s, App: f.app()}, PublicURL: "https://bridge.example",
		GerritUser: func(_ context.Context, gh github.User) (string, bool) {
			if gh.ID == 42 && gh.Login == "alice" {
				return "alice", true
			}
			return "", false
		},
		GerritAccount: func(_ context.Context, user string) (int, error) { return 1000, nil }}
	mux := http.NewServeMux()
	l.Register(mux)
	do := func(method, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
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

	// A callback without this browser's cookie (someone else's link) is
	// refused, and uses the link up.
	f.codes["c1"] = "alice"
	state, cookie := start()
	if w := do("GET", "/github/link/callback?code=c1&state="+state, &http.Cookie{Name: cookieName, Value: "other"}); w.Code != http.StatusBadRequest {
		t.Errorf("wrong cookie: %d", w.Code)
	}
	if len(s.All()) != 0 {
		t.Fatal("linked without the cookie")
	}

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

	// Pending links are bounded.
	l.mu.Lock()
	for i := len(l.pending); i < maxPending; i++ {
		l.pending[fmt.Sprint(i)] = pendingLink{at: time.Now()}
	}
	l.mu.Unlock()
	if w := do("POST", "/github/link", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("over the bound: %d", w.Code)
	}
}
