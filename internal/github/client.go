package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the github.com REST API.
const DefaultBaseURL = "https://api.github.com"

// maxResponse bounds an API response the client reads.
const maxResponse = 64 << 20

// Client is a REST client for one repository.
type Client struct {
	BaseURL     string // defaults to DefaultBaseURL
	Owner, Repo string
	Tokens      TokenSource
	HTTP        *http.Client

	mu   sync.Mutex
	rate RateLimit
}

// RateLimit is the most recent core rate-limit state GitHub reported.
type RateLimit struct {
	Limit, Remaining int
	Reset            time.Time
}

// RateLimit returns the last observed rate limit, for metrics.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate
}

// APIError is a non-2xx response.
type APIError struct {
	Method, Path string
	Status       int
	Message      string          `json:"message"`
	Errors       json.RawMessage `json:"errors"`
	// RetryAfter is set when the request was rate limited.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
	if len(e.Errors) > 0 && string(e.Errors) != "null" {
		s += " " + string(e.Errors)
	}
	return s
}

// IsNotFound reports whether err is a 404 from GitHub.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// RepoPath returns "/repos/<owner>/<repo>/" + p, or the repository's own
// path if p is empty.
func (c *Client) RepoPath(p string) string {
	r := "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo)
	if p = strings.TrimPrefix(p, "/"); p != "" {
		r += "/" + p
	}
	return r
}

// Do sends a request to path (absolute, e.g. from RepoPath, or a full
// URL), JSON-encoding in and decoding the response into out when they are
// non-nil. It returns the response with its body consumed, for headers.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (*http.Response, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	u := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		base := c.BaseURL
		if base == "" {
			base = DefaultBaseURL
		}
		u = strings.TrimRight(base, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "github-gerrit-bridge")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.recordRate(resp.Header)
	// GitHub's own pages are far smaller; never read without bound.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return resp, err
	}
	if len(b) > maxResponse {
		return resp, fmt.Errorf("%s %s: response larger than %d bytes", method, path, maxResponse)
	}
	if resp.StatusCode/100 != 2 {
		ae := &APIError{Method: method, Path: path, Status: resp.StatusCode}
		if json.Unmarshal(b, ae) != nil || ae.Message == "" {
			ae.Message = strings.TrimSpace(string(b[:min(len(b), 500)]))
		}
		ae.RetryAfter = retryAfter(resp)
		if resp.StatusCode == http.StatusUnauthorized {
			if inv, ok := c.Tokens.(interface{ Invalidate() }); ok {
				inv.Invalidate()
			}
		}
		return resp, ae
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp, fmt.Errorf("github %s %s: decoding response: %w", method, path, err)
		}
	}
	return resp, nil
}

func (c *Client) recordRate(h http.Header) {
	if h.Get("X-RateLimit-Resource") != "" && h.Get("X-RateLimit-Resource") != "core" {
		return
	}
	limit, err1 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	rem, err2 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err3 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	c.mu.Lock()
	c.rate = RateLimit{Limit: limit, Remaining: rem, Reset: time.Unix(reset, 0)}
	c.mu.Unlock()
}

// retryAfter returns how long to wait before retrying a rate-limited
// request, or 0 if the response is not a rate limit.
func retryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(s) * time.Second
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Until(time.Unix(reset, 0)), time.Second)
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return time.Minute
	}
	return 0
}

// list fetches every page of a list endpoint, following Link: rel="next".
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := path + sep + "per_page=100"
	var all []T
	for next != "" {
		var page []T
		resp, err := c.Do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// nextLink extracts the rel="next" URL from a Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(segs[0]), "<>")
			}
		}
	}
	return ""
}
