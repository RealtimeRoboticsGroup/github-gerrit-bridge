package usertokens

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Linker serves /github/link: a user authorizes the bridge's GitHub App,
// and their token is kept for the Gerrit account the bridge's identity
// mapping gives for their GitHub account. Whoever links, the token is
// their own and acts only for that Gerrit account's posts: someone tricked
// into following another's link links their own account, and nobody can
// link anyone else's.
type Linker struct {
	App    *github.UserApp
	Tokens *Tokens
	// PublicURL is the bridge's URL as browsers reach it
	// (https://bridge.example.org): the callback is PublicURL +
	// /github/link/callback, which must be one of the App's.
	PublicURL string
	// GerritUser maps a GitHub account to its Gerrit username (identity
	// mapping); GerritAccount gives that username's account id.
	GerritUser func(ctx context.Context, gh github.User) (string, bool, error)
	// InDirectory, if set, says who is whose comes from a directory
	// (identity.ldap), not from signing in to Gerrit with GitHub, and
	// whether the directory names gh at all: what a page says follows.
	// GerritUser answers nobody both for someone the directory does not
	// name and for someone it names who has no Gerrit account yet (the
	// account is made when they first sign in to Gerrit).
	InDirectory   func(gh github.User) bool
	GerritAccount func(ctx context.Context, user string) (int, error)
	// GerritURL is linked from the pages.
	GerritURL string
	// GitHubOwned says GitHub owns the branches (design §3.1): the App
	// then has Contents write, and /submit merges with the account.
	GitHubOwned bool
	Logger      *slog.Logger

	// A link in progress is kept by nobody but the browser: its state
	// says when it started, signed with key (made at start-up), and its
	// PKCE verifier is derived from it. Anyone can start one, so starts
	// cost the bridge nothing (tenth audit: a table of them could be
	// filled). used holds the states of links completed, for linkTTL.
	keyOnce sync.Once
	key     []byte
	mu      sync.Mutex
	used    map[string]time.Time
	now     func() time.Time
}

const (
	linkTTL    = 10 * time.Minute
	cookieName = "bridge_link"
	stateLen   = 8 + 16 + 16 // start time, random, MAC
)

func (l *Linker) mac(parts ...[]byte) []byte {
	l.keyOnce.Do(func() {
		l.key = make([]byte, 32)
		if _, err := rand.Read(l.key); err != nil {
			panic(err)
		}
	})
	m := hmac.New(sha256.New, l.key)
	for _, p := range parts {
		m.Write(p)
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// newState is a state for a link starting at now.
func (l *Linker) newState(now time.Time) (string, error) {
	b := make([]byte, stateLen)
	binary.BigEndian.PutUint64(b, uint64(now.Unix()))
	if _, err := rand.Read(b[8:24]); err != nil {
		return "", err
	}
	copy(b[24:], l.mac([]byte("state"), b[:24]))
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// stateOK reports whether state is one the bridge made, less than
// linkTTL ago.
func (l *Linker) stateOK(state string) bool {
	b, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(b) != stateLen || !hmac.Equal(b[24:], l.mac([]byte("state"), b[:24])[:16]) {
		return false
	}
	age := l.clock().Sub(time.Unix(int64(binary.BigEndian.Uint64(b)), 0))
	return age >= -time.Minute && age <= linkTTL
}

// verifier is state's PKCE verifier, and its challenge.
func (l *Linker) verifier(state string) (verifier, challenge string) {
	verifier = base64.RawURLEncoding.EncodeToString(l.mac([]byte("pkce"), []byte(state)))
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (l *Linker) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *Linker) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

func (l *Linker) callback() string {
	return strings.TrimSuffix(l.PublicURL, "/") + "/github/link/callback"
}

// linkPath is the link page's path as browsers reach it: under
// PublicURL's path (a proxy's /bridge, say), though the bridge serves it
// at /github/link (fourteenth review: the form posted to, and the cookie
// was kept for, the bare path, so nobody behind such a proxy could link).
func (l *Linker) linkPath() string {
	p := ""
	if u, err := url.Parse(l.PublicURL); err == nil {
		p = strings.TrimSuffix(u.Path, "/")
	}
	return p + "/github/link"
}

// Register adds the routes to mux.
func (l *Linker) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /github/link", l.intro)
	mux.HandleFunc("POST /github/link", l.start)
	mux.HandleFunc("GET /github/link/callback", l.finish)
}

// intro says what linking does: GitHub skips its consent screen for
// anyone who authorized the App before, so following a link must not
// link by itself.
func (l *Linker) intro(w http.ResponseWriter, r *http.Request) {
	text := "Link your GitHub account to post your Gerrit comments, replies and Code-Review votes on GitHub as yourself, " +
		"instead of as the bridge's bot. GitHub asks you to authorize the bridge's app, which can only comment on and review " +
		"pull requests, on repositories the app is installed on."
	if l.GitHubOwned {
		// What the token can do, not less (thirteenth review).
		text = "Link your GitHub account to post your Gerrit comments, replies and Code-Review votes on GitHub as yourself, " +
			"to have your Gerrit changes mirrored to GitHub, and to merge with /submit. GitHub asks you to authorize the bridge's " +
			"app on repositories it is installed on. Here it can also push to and merge pull requests as you: the bridge uses that " +
			"only to merge a PR when you post /submit on it; it pushes code with its own bot."
	}
	l.render(w, http.StatusOK, pageData{Text: text, Button: true})
}

func (l *Linker) start(w http.ResponseWriter, r *http.Request) {
	if !l.sameOrigin(r) {
		// A form on another site: GitHub may skip its consent screen, so
		// it would link without the user asking to (tenth audit).
		l.page(w, http.StatusForbidden, "Start linking from this page.", l.linkPath())
		return
	}
	state, err := l.newState(l.clock())
	if err != nil {
		l.page(w, http.StatusInternalServerError, "Something went wrong; try again.", "")
		return
	}
	_, challenge := l.verifier(state)
	// The state is also this browser's cookie: a callback made in
	// another browser (someone else's link) is refused.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: state, Path: l.linkPath(), MaxAge: int(linkTTL / time.Second),
		HttpOnly: true, Secure: strings.HasPrefix(l.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, l.App.AuthorizeURL(l.callback(), state, challenge), http.StatusFound)
}

// sameOrigin reports whether r was sent by the bridge's own page, as the
// browser says: Sec-Fetch-Site, or for browsers without it, Origin.
// Neither (not a browser) is refused.
func (l *Linker) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
	default:
		return false
	}
	u, err := url.Parse(l.PublicURL)
	return err == nil && strings.EqualFold(r.Header.Get("Origin"), u.Scheme+"://"+u.Host)
}

func (l *Linker) finish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" && q.Get("setup_action") != "" {
		// GitHub's own redirect after someone installs the App, which
		// links nobody.
		l.render(w, http.StatusOK, pageData{Text: "The app is installed. Now link your own GitHub account.", Button: true})
		return
	}
	cookie, err := r.Cookie(cookieName)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: l.linkPath(), MaxAge: -1})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		l.page(w, http.StatusBadRequest, "This link was started in another browser, or has expired. Start again from the beginning.", l.linkPath())
		return
	}
	if !l.stateOK(state) {
		l.page(w, http.StatusBadRequest, "This link has expired. Start again from the beginning.", l.linkPath())
		return
	}
	if e := q.Get("error"); e != "" {
		l.page(w, http.StatusOK, "Not linked: you did not authorize the app on GitHub.", l.linkPath())
		return
	}
	code := q.Get("code")
	if code == "" {
		l.page(w, http.StatusBadRequest, "GitHub sent no code. Start again from the beginning.", l.linkPath())
		return
	}
	verifier, _ := l.verifier(state)
	tok, err := l.App.Exchange(ctx, code, verifier, l.callback())
	if err != nil {
		l.logger().Warn("linking a GitHub account: exchanging the code", "err", err)
		l.page(w, http.StatusBadGateway, "GitHub would not finish the link. Start again from the beginning.", l.linkPath())
		return
	}
	// Once: only after GitHub took the code, so only links GitHub
	// authorized are kept.
	if !l.use(state) {
		l.page(w, http.StatusBadRequest, "This link has expired. Start again from the beginning.", l.linkPath())
		return
	}
	gh, err := l.App.TokenUser(ctx, tok.Access)
	if err != nil {
		l.logger().Warn("linking a GitHub account: reading who it is", "err", err)
		l.page(w, http.StatusBadGateway, "GitHub would not say who you are. Start again from the beginning.", l.linkPath())
		return
	}
	user, ok, err := l.GerritUser(ctx, gh)
	if err != nil {
		l.logger().Warn("linking a GitHub account: who is who is unknown for now", "github_login", gh.Login, "github_id", gh.ID, "err", err)
		l.page(w, http.StatusServiceUnavailable, "The bridge cannot tell whose GitHub account this is right now. Try again later.", l.linkPath())
		return
	}
	if !ok {
		l.logger().Info("linking a GitHub account with no Gerrit account", "github_login", gh.Login, "github_id", gh.ID)
		msg := "Not linked: @" + gh.Login + " has no Gerrit account the bridge knows. Sign in to Gerrit with GitHub first, then link again."
		if l.InDirectory != nil {
			msg = "Not linked: @" + gh.Login + " is not in the directory as anyone's GitHub account. Ask an administrator to add it to your account there, then link again."
			if l.InDirectory(gh) {
				msg = "Not linked: the directory gives @" + gh.Login + " a Gerrit account the bridge cannot act as yet. If you have never signed in to Gerrit, sign in once, then link again; otherwise ask an administrator."
			}
		}
		l.page(w, http.StatusOK, msg, l.GerritURL)
		return
	}
	account, err := l.GerritAccount(ctx, user)
	if err != nil || account == 0 {
		l.logger().Warn("linking a GitHub account: looking up its Gerrit account", "github_login", gh.Login, "gerrit_user", user, "err", err)
		l.page(w, http.StatusBadGateway, "Gerrit would not say who you are there. Try again later.", l.linkPath())
		return
	}
	rec := Record{GitHub: gh, GerritAccount: account, GerritUser: user, Token: tok, Linked: l.clock()}
	if err := l.Tokens.Link(rec); err != nil {
		l.logger().Error("saving a linked GitHub account", "github_login", gh.Login, "err", err)
		l.page(w, http.StatusInternalServerError, "The bridge could not save the link. Try again later.", "")
		return
	}
	l.logger().Info("linked a GitHub account", "github_login", gh.Login, "github_id", gh.ID, "gerrit_user", user, "gerrit_account", account)
	l.render(w, http.StatusOK, pageData{Heading: "GitHub account linked", Text: "Your comments and votes in Gerrit as " + user + " are now posted on GitHub as @" + gh.Login +
		". To stop, revoke the app under GitHub's Settings, Applications, Authorized GitHub Apps."})
}

// use marks state used, reporting whether it was not already.
func (l *Linker) use(state string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	for s, at := range l.used {
		if now.Sub(at) > 2*linkTTL {
			delete(l.used, s)
		}
	}
	if _, ok := l.used[state]; ok {
		return false
	}
	if l.used == nil {
		l.used = map[string]time.Time{}
	}
	l.used[state] = now
	return true
}

type pageData struct {
	Heading    string // the page's title; "Link GitHub" if empty
	Text, Next string
	Button     bool   // a form to start linking
	Action     string // where it posts (linkPath)
}

var pageTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Heading}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 16px;color:#1f2328;background:#fff}
@media (prefers-color-scheme: dark){body{color:#e6edf3;background:#0d1117}a{color:#4493f8}}</style></head>
<body><h1>{{.Heading}}</h1><p>{{.Text}}</p>{{if .Button}}<form method="post" action="{{.Action}}"><button type="submit">Link GitHub</button></form>{{end}}{{if .Next}}<p><a href="{{.Next}}">Continue</a></p>{{end}}</body></html>
`))

func (l *Linker) page(w http.ResponseWriter, status int, text, next string) {
	l.render(w, status, pageData{Text: text, Next: next})
}

func (l *Linker) render(w http.ResponseWriter, status int, d pageData) {
	d.Action = l.linkPath()
	if d.Heading == "" {
		d.Heading = "Link GitHub"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://github.com; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	pageTmpl.Execute(w, d)
}
