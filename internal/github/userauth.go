package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UserApp is a GitHub App's user authorization, GitHub's OAuth web flow
// (design §7.2): the tokens it gets act as the user who authorized it,
// limited to what both the App and the user may do, on repositories the
// App is installed on.
type UserApp struct {
	ClientID, ClientSecret string
	// WebURL is github.com (for tests); APIURL the REST API.
	WebURL, APIURL string
	HTTP           *http.Client
}

// UserToken is a user's token and the refresh token that renews it.
// Zero expiry times mean never (an App that opted out of expiry).
type UserToken struct {
	Access         string
	AccessExpires  time.Time
	Refresh        string
	RefreshExpires time.Time
}

// ErrBadRefresh means a refresh token is no longer good (expired, used
// already, or the user revoked the App): only linking again helps.
var ErrBadRefresh = errors.New("GitHub refresh token is no longer good")

func (a *UserApp) web() string {
	if a.WebURL != "" {
		return strings.TrimSuffix(a.WebURL, "/")
	}
	return "https://github.com"
}

func (a *UserApp) api() string {
	if a.APIURL != "" {
		return strings.TrimSuffix(a.APIURL, "/")
	}
	return DefaultBaseURL
}

func (a *UserApp) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// NewVerifier returns a PKCE code verifier and its S256 challenge.
func NewVerifier() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// AuthorizeURL is where to send the user to authorize the App, coming
// back to redirect with state and a code.
func (a *UserApp) AuthorizeURL(redirect, state, challenge string) string {
	q := url.Values{
		"client_id":             {a.ClientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"allow_signup":          {"false"},
	}
	return a.web() + "/login/oauth/authorize?" + q.Encode()
}

// Exchange trades an authorization code for the user's token.
func (a *UserApp) Exchange(ctx context.Context, code, verifier, redirect string) (UserToken, error) {
	return a.token(ctx, url.Values{"code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
}

// Refresh renews a token. GitHub voids the refresh token used, and the
// access token it came with: keep the new one before using it.
func (a *UserApp) Refresh(ctx context.Context, refresh string) (UserToken, error) {
	return a.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

func (a *UserApp) token(ctx context.Context, form url.Values) (UserToken, error) {
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", a.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return UserToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return UserToken{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return UserToken{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return UserToken{}, fmt.Errorf("GitHub token endpoint: HTTP %d", resp.StatusCode)
	}
	// Errors come back as 200 too, with "error" set.
	var out struct {
		Access         string `json:"access_token"`
		ExpiresIn      int64  `json:"expires_in"`
		Refresh        string `json:"refresh_token"`
		RefreshExpires int64  `json:"refresh_token_expires_in"`
		Error          string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return UserToken{}, fmt.Errorf("GitHub token endpoint: %w", err)
	}
	switch {
	case out.Error == "bad_refresh_token":
		return UserToken{}, ErrBadRefresh
	case out.Error != "":
		// The error code only: GitHub's description may quote the request.
		return UserToken{}, fmt.Errorf("GitHub token endpoint: %s", out.Error)
	case out.Access == "":
		return UserToken{}, errors.New("GitHub token endpoint: no access token")
	}
	now := time.Now()
	t := UserToken{Access: out.Access, Refresh: out.Refresh}
	if out.ExpiresIn > 0 {
		t.AccessExpires = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if out.RefreshExpires > 0 {
		t.RefreshExpires = now.Add(time.Duration(out.RefreshExpires) * time.Second)
	}
	return t, nil
}

// TokenUser returns the account a user token acts as.
func (a *UserApp) TokenUser(ctx context.Context, access string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.api()+"/user", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.client().Do(req)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return User{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return User{}, &APIError{Method: http.MethodGet, Path: "/user", Status: resp.StatusCode}
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return User{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return User{}, errors.New("GitHub /user: no account")
	}
	return u, nil
}
