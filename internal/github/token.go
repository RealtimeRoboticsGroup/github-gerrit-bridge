package github

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies the token for API calls and git pushes. v1 uses a
// personal token; a GitHub App installation token source, or per-user
// tokens (design §7), can implement the same interface later.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed token.
type StaticToken string

// Token returns t.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// CommandToken runs a command, such as `gh auth token`, and uses its
// output as the token. The result is cached for TTL so a token rotated
// outside the bridge (gh auth refresh) is picked up without a restart.
type CommandToken struct {
	Command []string
	TTL     time.Duration

	mu      sync.Mutex
	token   string
	fetched time.Time
}

// Token returns the cached token, rerunning the command when it expires.
func (c *CommandToken) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if c.token != "" && time.Since(c.fetched) < ttl {
		return c.token, nil
	}
	if len(c.Command) == 0 {
		return "", fmt.Errorf("empty token command")
	}
	out, err := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s: %w: %s", strings.Join(c.Command, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", strings.Join(c.Command, " "), err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("%s printed no token", strings.Join(c.Command, " "))
	}
	c.token, c.fetched = tok, time.Now()
	return tok, nil
}

// Invalidate drops the cached token, e.g. after a 401.
func (c *CommandToken) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
}
