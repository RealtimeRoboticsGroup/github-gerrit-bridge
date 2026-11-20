package reconcile

import (
	"context"
	"fmt"
	"strings"
)

// Follow copies GitHub's copy of branch into Gerrit, where GitHub owns
// the target branches (design §3.1): PRs are merged on GitHub, and
// Gerrit's branch is whatever GitHub's is, force-pushed if GitHub's was.
// Gerrit itself then closes each open change of the branch whose Change-Id
// one of the new commits carries, as merged, with that commit as its last
// patch set. Only branches both sides have are followed: the bridge never
// creates or deletes one in Gerrit. The caller holds the branch.
func (c *Code) Follow(ctx context.Context, branch string) error {
	if !c.GitHubOwned || bridgeBranch(branch) {
		return nil
	}
	env, err := c.githubEnv(ctx)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	theirs, err := c.lsRemote(ctx, env, c.GitHubURL, ref)
	if err != nil {
		return fmt.Errorf("reading GitHub's %s: %w", branch, err)
	}
	ours, err := c.lsRemote(ctx, nil, "origin", ref)
	if err != nil {
		return fmt.Errorf("reading Gerrit's %s: %w", branch, err)
	}
	if theirs == "" || ours == "" || theirs == ours {
		return nil
	}
	local := "refs/remotes/github/heads/" + branch
	if _, err := c.Mirror.GitEnv(ctx, env, nil, "-c", fsckFetch, "fetch", "--quiet", c.GitHubURL, "+"+ref+":"+local); err != nil {
		return fmt.Errorf("fetching GitHub's %s: %w", branch, err)
	}
	// What was fetched, not what was listed: GitHub's may have moved on
	// since, and the push is of what the mirror has.
	if theirs, err = c.Mirror.RevParse(ctx, local); err != nil || theirs == "" {
		return fmt.Errorf("fetching GitHub's %s: %v", branch, err)
	}
	if theirs == ours {
		return nil
	}
	// Only for the log: a force-push is GitHub's to make, and followed.
	forced := false
	if have, err := c.Mirror.RevParse(ctx, ours); err == nil && have != "" {
		_, err := c.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", ours, theirs)
		forced = err != nil
	}
	if c.DryRun {
		c.Logger.Info("dry run: would copy GitHub's branch into Gerrit", "branch", branch, "from", short(ours), "to", short(theirs), "forced", forced)
		return nil
	}
	// Forced, leased on what Gerrit had: anything else writing it (nobody
	// should) is not overwritten unseen, but read again next time.
	res, err := c.Mirror.Push(ctx, "--force-with-lease="+ref+":"+ours, theirs+":"+ref)
	if err != nil {
		return fmt.Errorf("copying GitHub's %s into Gerrit: %w", branch, err)
	}
	for _, r := range res {
		if r.Rejected {
			return fmt.Errorf("copying GitHub's %s into Gerrit: Gerrit refused it: %s", branch, r.Reason)
		}
	}
	if forced {
		c.Logger.Warn("GitHub's branch was force-pushed; Gerrit's follows it", "branch", branch, "from", short(ours), "to", short(theirs))
	} else {
		c.Logger.Info("copied GitHub's branch into Gerrit", "branch", branch, "from", short(ours), "to", short(theirs))
	}
	return nil
}

// lsRemote is ref's SHA at remote, or "" if it has none.
func (c *Code) lsRemote(ctx context.Context, env []string, remote, ref string) (string, error) {
	out, err := c.Mirror.GitEnv(ctx, env, nil, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok && name == ref && fullSHA.MatchString(sha) {
			return sha, nil
		}
	}
	return "", nil
}

// bridgeBranch reports whether branch is one of the bridge's own on
// GitHub (a PR's head or a frozen base), never a target branch.
func bridgeBranch(branch string) bool {
	return strings.HasPrefix(branch, headPrefix) || strings.HasPrefix(branch, frozenPrefix)
}
