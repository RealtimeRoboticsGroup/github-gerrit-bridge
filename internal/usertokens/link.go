package usertokens

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"log/slog"
	"net/http"
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
	GerritUser    func(ctx context.Context, gh github.User) (string, bool)
	GerritAccount func(ctx context.Context, user string) (int, error)
	// GerritURL is linked from the pages.
	GerritURL string
	Logger    *slog.Logger

	mu      sync.Mutex
	pending map[string]pendingLink // by state
	now     func() time.Time
}

type pendingLink struct {
	verifier string
	at       time.Time
}

// Pending links expire, and are bounded: anyone can start one.
const (
	linkTTL    = 10 * time.Minute
	maxPending = 1000
	cookieName = "bridge_link"
)

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
	l.render(w, http.StatusOK, pageData{
		Text: "Link your GitHub account to post your Gerrit comments, replies and Code-Review votes on GitHub as yourself, " +
			"instead of as the bridge's bot. GitHub asks you to authorize the bridge's app, which can only comment on and review " +
			"pull requests, on repositories the app is installed on.",
		Button: true,
	})
}

func (l *Linker) start(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		l.page(w, http.StatusInternalServerError, "Something went wrong; try again.", "")
		return
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	verifier, challenge, err := github.NewVerifier()
	if err != nil {
		l.page(w, http.StatusInternalServerError, "Something went wrong; try again.", "")
		return
	}
	now := l.clock()
	l.mu.Lock()
	if l.pending == nil {
		l.pending = map[string]pendingLink{}
	}
	for s, p := range l.pending {
		if now.Sub(p.at) > linkTTL {
			delete(l.pending, s)
		}
	}
	full := len(l.pending) >= maxPending
	if !full {
		l.pending[state] = pendingLink{verifier: verifier, at: now}
	}
	l.mu.Unlock()
	if full {
		l.page(w, http.StatusServiceUnavailable, "Too many links are being made right now; try again in a few minutes.", "")
		return
	}
	// The state is also this browser's cookie: a callback made in
	// another browser (someone else's link) is refused.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: state, Path: "/github/link", MaxAge: int(linkTTL / time.Second),
		HttpOnly: true, Secure: strings.HasPrefix(l.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, l.App.AuthorizeURL(l.callback(), state, challenge), http.StatusFound)
}

func (l *Linker) finish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(cookieName)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/github/link", MaxAge: -1})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		l.page(w, http.StatusBadRequest, "This link was started in another browser, or has expired. Start again from the beginning.", "/github/link")
		return
	}
	l.mu.Lock()
	p, ok := l.pending[state]
	delete(l.pending, state) // once
	l.mu.Unlock()
	if !ok || l.clock().Sub(p.at) > linkTTL {
		l.page(w, http.StatusBadRequest, "This link has expired. Start again from the beginning.", "/github/link")
		return
	}
	if e := q.Get("error"); e != "" {
		l.page(w, http.StatusOK, "Not linked: you did not authorize the app on GitHub.", "/github/link")
		return
	}
	code := q.Get("code")
	if code == "" {
		l.page(w, http.StatusBadRequest, "GitHub sent no code. Start again from the beginning.", "/github/link")
		return
	}
	tok, err := l.App.Exchange(ctx, code, p.verifier, l.callback())
	if err != nil {
		l.logger().Warn("linking a GitHub account: exchanging the code", "err", err)
		l.page(w, http.StatusBadGateway, "GitHub would not finish the link. Start again from the beginning.", "/github/link")
		return
	}
	gh, err := l.App.TokenUser(ctx, tok.Access)
	if err != nil {
		l.logger().Warn("linking a GitHub account: reading who it is", "err", err)
		l.page(w, http.StatusBadGateway, "GitHub would not say who you are. Start again from the beginning.", "/github/link")
		return
	}
	user, ok := l.GerritUser(ctx, gh)
	if !ok {
		l.logger().Info("linking a GitHub account with no Gerrit account", "github_login", gh.Login, "github_id", gh.ID)
		l.page(w, http.StatusOK, "Not linked: @"+gh.Login+" has no Gerrit account the bridge knows. Sign in to Gerrit with GitHub first, then link again.", l.GerritURL)
		return
	}
	account, err := l.GerritAccount(ctx, user)
	if err != nil || account == 0 {
		l.logger().Warn("linking a GitHub account: looking up its Gerrit account", "github_login", gh.Login, "gerrit_user", user, "err", err)
		l.page(w, http.StatusBadGateway, "Gerrit would not say who you are there. Try again later.", "/github/link")
		return
	}
	rec := Record{GitHub: gh, GerritAccount: account, GerritUser: user, Token: tok, Linked: l.clock()}
	if err := l.Tokens.Store.Put(rec); err != nil {
		l.logger().Error("saving a linked GitHub account", "github_login", gh.Login, "err", err)
		l.page(w, http.StatusInternalServerError, "The bridge could not save the link. Try again later.", "")
		return
	}
	l.logger().Info("linked a GitHub account", "github_login", gh.Login, "github_id", gh.ID, "gerrit_user", user, "gerrit_account", account)
	l.page(w, http.StatusOK, "Linked: your comments and votes in Gerrit as "+user+" are now posted on GitHub as @"+gh.Login+
		". To stop, revoke the app under GitHub's Settings, Applications, Authorized GitHub Apps.", "")
}

type pageData struct {
	Text, Next string
	Button     bool // a form to start linking
}

var pageTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link GitHub</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 16px;color:#1f2328;background:#fff}
@media (prefers-color-scheme: dark){body{color:#e6edf3;background:#0d1117}a{color:#4493f8}}</style></head>
<body><h1>Link GitHub</h1><p>{{.Text}}</p>{{if .Button}}<form method="post" action="/github/link"><button type="submit">Link GitHub</button></form>{{end}}{{if .Next}}<p><a href="{{.Next}}">Continue</a></p>{{end}}</body></html>
`))

func (l *Linker) page(w http.ResponseWriter, status int, text, next string) {
	l.render(w, status, pageData{Text: text, Next: next})
}

func (l *Linker) render(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://github.com; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	pageTmpl.Execute(w, d)
}
