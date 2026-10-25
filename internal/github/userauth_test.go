package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestUserAppFlow(t *testing.T) {
	var got url.Values
	answer := `{"access_token":"ghu_a","expires_in":28800,"refresh_token":"ghr_r","refresh_token_expires_in":15897600,"token_type":"bearer","scope":""}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			r.ParseForm()
			got = r.PostForm
			fmt.Fprint(w, answer)
		case "/user":
			if r.Header.Get("Authorization") != "Bearer ghu_a" {
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"login":"alice","id":42}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := &UserApp{ClientID: "Iv1.c", ClientSecret: "s3cret", WebURL: srv.URL, APIURL: srv.URL}
	ctx := context.Background()

	v, ch, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(v))
	if len(v) != 43 || ch != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Errorf("verifier %q challenge %q", v, ch)
	}
	u, _ := url.Parse(a.AuthorizeURL("https://b/github/link/callback", "st", ch))
	q := u.Query()
	if u.Path != "/login/oauth/authorize" || q.Get("client_id") != "Iv1.c" || q.Get("state") != "st" || q.Get("code_challenge") != ch ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "https://b/github/link/callback" {
		t.Errorf("authorize URL %s", u)
	}

	before := time.Now()
	tok, err := a.Exchange(ctx, "code1", v, "https://b/github/link/callback")
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("code") != "code1" || got.Get("code_verifier") != v || got.Get("client_id") != "Iv1.c" || got.Get("client_secret") != "s3cret" ||
		got.Get("redirect_uri") != "https://b/github/link/callback" {
		t.Errorf("exchange sent %v", got)
	}
	if tok.Access != "ghu_a" || tok.Refresh != "ghr_r" || tok.AccessExpires.Before(before.Add(8*time.Hour)) || tok.AccessExpires.After(time.Now().Add(8*time.Hour)) ||
		tok.RefreshExpires.Before(before.Add(183*24*time.Hour)) {
		t.Errorf("token %+v", tok)
	}

	if _, err := a.Refresh(ctx, "ghr_r"); err != nil || got.Get("grant_type") != "refresh_token" || got.Get("refresh_token") != "ghr_r" {
		t.Errorf("refresh sent %v, err %v", got, err)
	}
	answer = `{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired."}`
	if _, err := a.Refresh(ctx, "ghr_r"); !errors.Is(err, ErrBadRefresh) {
		t.Errorf("bad refresh: %v", err)
	}
	answer = `{"error":"bad_verification_code"}`
	if _, err := a.Exchange(ctx, "code1", v, "x"); err == nil || errors.Is(err, ErrBadRefresh) {
		t.Errorf("bad code: %v", err)
	}
	// An App that opted out of expiry: no expiry, no refresh token.
	answer = `{"access_token":"ghu_a","token_type":"bearer","scope":""}`
	if tok, err := a.Exchange(ctx, "code1", v, "x"); err != nil || !tok.AccessExpires.IsZero() || tok.Refresh != "" {
		t.Errorf("non-expiring: %+v, %v", tok, err)
	}

	if u, err := a.TokenUser(ctx, "ghu_a"); err != nil || u.Login != "alice" || u.ID != 42 {
		t.Errorf("user %+v, err %v", u, err)
	}
	var ae *APIError
	if _, err := a.TokenUser(ctx, "ghu_other"); !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Errorf("revoked token: %v", err)
	}
}
