// Package gitmirror runs git against a local bare mirror of the Gerrit
// project.
//
// It shells out to git rather than using a Go implementation: git is what
// Gerrit's users run, so its object, pack and push behaviour is the
// behaviour that matters.
package gitmirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Repo is a bare repository with one remote, "origin".
type Repo struct {
	Dir    string
	Remote string
	// Config are -c key=value settings for every command, e.g. from
	// HTTPCredentials.
	Config []string
	// Env is extra environment for every command.
	Env []string
	// Ident is the author and committer of commits the bridge writes.
	Name, Email string
	// Logger, if set, logs each command and how long it took (debug).
	Logger *slog.Logger
}

// HTTPCredentials returns Config and Env entries that answer git's
// credential prompts for URLs under baseURL with user and password. The
// helper is scoped to baseURL, so credentials for Gerrit are never offered
// to GitHub or vice versa. The password is passed in the environment, not
// on the command line, so it does not show up in ps.
func HTTPCredentials(baseURL, user, password string) (config, env []string) {
	config = []string{CredentialHelper(baseURL)}
	return config, CredentialEnv(baseURL, user, password)
}

// CredentialEnv is the environment half of HTTPCredentials. Use it alone
// to supply a password that changes between commands (a refreshed token)
// while the helper itself stays in Repo.Config.
func CredentialEnv(baseURL, user, password string) []string {
	v := credentialVar(baseURL)
	return []string{v + "_USER=" + user, v + "_PASSWORD=" + password}
}

// CredentialHelper returns the -c setting for a helper scoped to baseURL
// that reads CredentialEnv's variables.
func CredentialHelper(baseURL string) string {
	v := credentialVar(baseURL)
	return fmt.Sprintf(`credential.%s.helper=!f() { test "$1" = get && printf 'username=%%s\npassword=%%s\n' "$%s_USER" "$%s_PASSWORD"; }; f`,
		strings.TrimRight(baseURL, "/"), v, v)
}

func credentialVar(baseURL string) string {
	h := sha256.Sum256([]byte(strings.TrimRight(baseURL, "/")))
	return fmt.Sprintf("BRIDGE_CRED_%X", h[:4])
}

// Error is a failed git command.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Init creates the bare repository if needed and points origin at Remote.
func (r *Repo) Init(ctx context.Context) error {
	if _, err := os.Stat(r.Dir + "/HEAD"); errors.Is(err, os.ErrNotExist) {
		// Only the bridge's: it holds refs/bridge/*, the identity pins
		// included, and every change's code, private ones too.
		if err := os.MkdirAll(r.Dir, 0o700); err != nil {
			return err
		}
		if _, err := r.Git(ctx, nil, "init", "--bare", "--quiet"); err != nil {
			return err
		}
	}
	if err := os.Chmod(r.Dir, 0o700); err != nil {
		return err
	}
	// Keep a commit-graph as fetches bring history in: ancestry checks
	// (one per open change per reconcile) then stay cheap on a long one.
	if _, err := r.Git(ctx, nil, "config", "fetch.writeCommitGraph", "true"); err != nil {
		return err
	}
	// Credentials go only where the bridge sends them, as with its HTTP
	// clients.
	if _, err := r.Git(ctx, nil, "config", "http.followRedirects", "false"); err != nil {
		return err
	}
	if _, err := r.Git(ctx, nil, "remote", "get-url", "origin"); err != nil {
		_, err = r.Git(ctx, nil, "remote", "add", "origin", r.Remote)
		return err
	}
	_, err := r.Git(ctx, nil, "remote", "set-url", "origin", r.Remote)
	return err
}

// Git runs git in the repository with stdin and returns stdout.
func (r *Repo) Git(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return r.GitEnv(ctx, nil, stdin, args...)
}

// GitEnv is Git with extra environment for this one command.
func (r *Repo) GitEnv(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
	full := make([]string, 0, 2*len(r.Config)+len(args))
	for _, c := range r.Config {
		full = append(full, "-c", c)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(),
		// Only our settings: no user or system config, no prompts.
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_DIR="+r.Dir,
	)
	if r.Name != "" {
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME="+r.Name, "GIT_AUTHOR_EMAIL="+r.Email,
			"GIT_COMMITTER_NAME="+r.Name, "GIT_COMMITTER_EMAIL="+r.Email)
	}
	cmd.Env = append(cmd.Env, r.Env...)
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	if r.Logger != nil {
		// Arguments only: config and environment carry credentials.
		r.Logger.Debug("git", "args", strings.Join(args, " "), "took", time.Since(start).Round(time.Millisecond), "err", err)
	}
	if err != nil {
		return stdout.Bytes(), &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// RevParse resolves rev to an object id, or returns "" if it does not exist.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.Git(ctx, nil, "rev-parse", "--verify", "--quiet", rev)
	var ge *Error
	if errors.As(err, &ge) && ge.Stderr == "" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PushResult is the outcome of one ref in a push.
type PushResult struct {
	Ref      string
	Rejected bool
	Reason   string
}

// Push runs git push --porcelain with refspecs and leases, and reports the
// per-ref outcome. A rejected ref is not an error; a failure to talk to
// the remote is.
func (r *Repo) Push(ctx context.Context, args ...string) ([]PushResult, error) {
	out, err := r.Git(ctx, nil, append([]string{"push", "--porcelain", "origin"}, args...)...)
	results := parsePorcelain(out)
	if err != nil && len(results) == 0 {
		return nil, err
	}
	return results, nil
}

// parsePorcelain parses git push --porcelain output:
//
//	<flag>\t<from>:<to>\t<summary> (<reason>)
func parsePorcelain(out []byte) []PushResult {
	var res []PushResult
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 || len(f[0]) != 1 {
			continue
		}
		_, to, _ := strings.Cut(f[1], ":")
		pr := PushResult{Ref: to}
		if f[0] == "!" {
			pr.Rejected = true
			pr.Reason = f[2]
		}
		res = append(res, pr)
	}
	return res
}
