package reconcile

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// Uploader pushes a commit from the mirror to refs/for/<target>, as a
// Gerrit user ("" means the bridge account).
type Uploader interface {
	Upload(ctx context.Context, user, commit, target string, opts []string) error
	// CanActAs reports whether uploads can be made as other users.
	CanActAs() bool
}

// refsFor builds "refs/for/<target>%opt,opt".
func refsFor(target string, opts []string) string {
	r := "refs/for/" + target
	if len(opts) > 0 {
		r += "%" + strings.Join(opts, ",")
	}
	return r
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

// HTTPUploader pushes as the bridge account through the mirror's origin.
type HTTPUploader struct {
	Mirror *gitmirror.Repo
}

// Upload implements Uploader. user must be "".
func (u HTTPUploader) Upload(ctx context.Context, user, commit, target string, opts []string) error {
	if user != "" {
		return fmt.Errorf("cannot upload as %s without a peer key", user)
	}
	out, err := u.Mirror.Git(ctx, nil, "push", "origin", commit+":"+refsFor(target, opts))
	return uploadResult(out, err)
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

// Upload implements Uploader.
func (u SuexecUploader) Upload(ctx context.Context, user, commit, target string, opts []string) error {
	if user == "" {
		return u.Bridge.Upload(ctx, "", commit, target, opts)
	}
	remote := "ssh://" + url.PathEscape("Gerrit Code Review") + "@" + u.Address + "/" + u.Project
	ssh := "ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=30 -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes" +
		" -o UserKnownHostsFile=" + u.KnownHostsFile + " -i " + u.PeerKeyFile
	out, err := u.Mirror.GitEnv(ctx, []string{"GIT_SSH_COMMAND=" + ssh}, nil,
		"push", "--receive-pack=suexec --as "+user+" -- git-receive-pack", remote, commit+":"+refsFor(target, opts))
	return uploadResult(out, err)
}

// CanActAs implements Uploader.
func (SuexecUploader) CanActAs() bool { return true }
