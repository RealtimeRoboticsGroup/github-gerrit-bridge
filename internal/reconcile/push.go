package reconcile

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

// Uploader pushes a commit from the mirror to refs/for/<target>, as a
// Gerrit user ("" means the bridge account).
type Uploader interface {
	Upload(ctx context.Context, user, commit, target string, opts []string) error
	// CanActAs reports whether uploads can be made as other users.
	CanActAs() bool
}

// refsFor builds "refs/for/<target>%opt,opt". A target with a % in it
// would bring its own push options (submit, base=, ...), which Gerrit
// takes from any branch so named (eighth audit): refused, as is one git
// would read as something else.
func refsFor(target string, opts []string) (string, error) {
	if target == "" || strings.HasPrefix(target, "-") || strings.ContainsAny(target, "%:^~?*[\\ \t\n") {
		return "", fmt.Errorf("refusing to upload to branch %q", target)
	}
	r := "refs/for/" + target
	if len(opts) > 0 {
		r += "%" + strings.Join(opts, ",")
	}
	return r, nil
}

// uploadResult interprets git push output: "no new changes" means the
// commit is already a patch set, which is success for an idempotent upload.
func uploadResult(out []byte, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "no new changes") || strings.Contains(err.Error(), "no new changes") {
		return nil
	}
	return fmt.Errorf("%w\n%s", err, out)
}

// uploaded is uploadResult, noting an upload that made a patch set in
// the run's trace.
func uploaded(ctx context.Context, user, commit, ref string, out []byte, err error) error {
	if err == nil {
		what := "gerrit upload " + commit + " to " + ref
		if user != "" {
			what += " as " + user
		}
		trace.Wrote(ctx, what)
	}
	return uploadResult(out, err)
}

// HTTPUploader pushes as the bridge account through the mirror's origin.
type HTTPUploader struct {
	Mirror *gitmirror.Repo
}

// Upload implements Uploader. user must be "".
func (u HTTPUploader) Upload(ctx context.Context, user, commit, target string, opts []string) error {
	if user != "" {
		return fmt.Errorf("cannot upload as %s without a peer key", user)
	}
	ref, err := refsFor(target, opts)
	if err != nil {
		return err
	}
	out, err := u.Mirror.Git(ctx, nil, "push", "origin", commit+":"+ref)
	return uploaded(ctx, "", commit, ref, out, err)
}

// CanActAs implements Uploader.
func (HTTPUploader) CanActAs() bool { return false }

// SuexecUploader pushes over SSH as Gerrit's peer user, running
// git-receive-pack through `suexec --as <user>` (design §5.2, verified on
// 3.14.4). Uploads as the bridge itself go through Bridge.
type SuexecUploader struct {
	Mirror *gitmirror.Repo
	// Address is Gerrit's SSH host:port, Project the project name.
	Address, Project string
	PeerKeyFile      string
	KnownHostsFile   string
	Bridge           Uploader
}

var suexecUser = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// Upload implements Uploader.
func (u SuexecUploader) Upload(ctx context.Context, user, commit, target string, opts []string) error {
	if user == "" {
		return u.Bridge.Upload(ctx, "", commit, target, opts)
	}
	// The name goes on Gerrit's SSH command line, where Gerrit reads it as
	// an account: it must be a plain username.
	if gerrit.AmbiguousUsername(user) || !suexecUser.MatchString(user) {
		return fmt.Errorf("refusing to upload as %q: not a plain username", user)
	}
	remote := "ssh://" + url.PathEscape("Gerrit Code Review") + "@" + u.Address + "/" + u.Project
	// Git runs GIT_SSH_COMMAND through the shell: the paths are quoted.
	ssh := "ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=30 -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes" +
		" -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile=" + shellQuote(u.KnownHostsFile) + " -i " + shellQuote(u.PeerKeyFile)
	ref, err := refsFor(target, opts)
	if err != nil {
		return err
	}
	out, err := u.Mirror.GitEnv(ctx, []string{"GIT_SSH_COMMAND=" + ssh}, nil,
		"push", "--receive-pack=suexec --as "+user+" -- git-receive-pack", remote, commit+":"+ref)
	return uploaded(ctx, user, commit, ref, out, err)
}

// CanActAs implements Uploader.
func (SuexecUploader) CanActAs() bool { return true }

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
