package reconcile

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// Importer turns pull requests opened on GitHub into Gerrit changes, one
// squashed change per stack layer (design §5.2), and imports later pushes
// to them as new patch sets.
type Importer struct {
	Project   string
	Gerrit    *gerrit.Client
	GitHub    *github.Client
	GitHubURL string
	Mirror    *gitmirror.Repo
	Store     *state.Store
	Uploader  Uploader
	// Identity maps GitHub logins to Gerrit usernames.
	Identity map[string]string
	// Bridge is the bridge's own Gerrit account (committer of imports it
	// owns).
	Bridge        gerrit.AccountInfo
	DefaultBranch string
	// CreatedAfter skips PRs opened before it (zero: import all).
	CreatedAfter time.Time
	// Pushers says who pushed which commit (from webhooks).
	Pushers *Pushers
	DryRun  bool
	Logger  *slog.Logger

	mu       sync.Mutex
	lastSync map[int]string // PR -> last gerrit/sync status set

	// onImported runs after a patch set is imported (Code.OnExported).
	onImported func(ctx context.Context, n int) error
	// link records a new PR <-> change link (Code.link).
	link func(ctx context.Context, n int, origin, branch, changeID string, pr *github.PullRequest) error
}

// layer is one GitHub-born PR being imported.
type layer struct {
	pr     *github.PullRequest
	parent *layer // GitHub-born parent layer, if any
	// gerritParentChange is set when the parent is a Gerrit-born PR.
	gerritParentChange int
}

// importPR's outcome, for its children.
type imported struct {
	// heads maps GitHub commits of this layer to the Gerrit patch set
	// commits they correspond to.
	heads map[string]string
	// change and patchSet are where the PR's head lives in Gerrit.
	change, patchSet int
	// bridgeOwned is set when the uploader could not be the user.
	bridgeOwned bool
	// noSignOff is set when the imported commit has no Signed-off-by.
	noSignOff bool
}

// syncError is an import problem to show on the PR as gerrit/sync.
type syncError struct {
	state string // pending (the user can fix it by pushing), failure
	msg   string
}

func (e *syncError) Error() string { return e.msg }

var changeIDTrailer = regexp.MustCompile(`(?m)^Change-Id: (I[0-9a-f]{40})\s*$`)

// Import imports every GitHub-born PR whose chain targets branch.
func (im *Importer) Import(ctx context.Context, branch string) error {
	prs, err := im.GitHub.ListPulls(ctx, "open")
	if err != nil {
		return err
	}
	repo := im.GitHub.Owner + "/" + im.GitHub.Repo
	byHead := map[string]*github.PullRequest{}
	for i := range prs {
		pr := &prs[i]
		if !strings.EqualFold(pr.HeadRepo(), repo) || strings.HasPrefix(pr.Head.Ref, headPrefix) || strings.HasPrefix(pr.Head.Ref, frozenPrefix) {
			continue
		}
		if pr.CreatedAt.Before(im.CreatedAfter) && im.Store.ChangeForPR(pr.Number) == 0 {
			continue // predates the bridge; leave it alone
		}
		byHead[pr.Head.Ref] = pr
	}
	if len(byHead) == 0 {
		return nil
	}
	// Gerrit-born PR heads on this branch, which GitHub PRs may stack on.
	gerritHeads := map[string]int{}
	for _, n := range im.Store.ChangesOnBranch(branch) {
		st, err := im.Store.Load(ctx, n)
		if err != nil {
			return err
		}
		if st.State.Origin == "gerrit" && st.State.Lifecycle == "NEW" {
			gerritHeads[st.State.HeadBranch] = n
		}
	}
	// A PR the bridge moved to a frozen base (rule 2) still logically
	// sits on the layer its Gerrit parent belongs to.
	baseOf := map[int]string{}
	for _, pr := range byHead {
		baseOf[pr.Number] = pr.Base.Ref
		if !strings.HasPrefix(pr.Base.Ref, frozenPrefix) {
			continue
		}
		b, err := im.logicalBase(ctx, branch, pr.Number)
		if err != nil {
			return err
		}
		baseOf[pr.Number] = b
	}
	// Order parents before children; keep only chains rooted on branch.
	var order []*layer
	placed := map[string]*layer{}
	var place func(pr *github.PullRequest, depth int) *layer
	place = func(pr *github.PullRequest, depth int) *layer {
		if l, ok := placed[pr.Head.Ref]; ok {
			return l
		}
		if depth > 50 {
			return nil
		}
		l := &layer{pr: pr}
		base := baseOf[pr.Number]
		switch {
		case base == branch:
		case gerritHeads[base] != 0:
			l.gerritParentChange = gerritHeads[base]
		case byHead[base] != nil:
			if l.parent = place(byHead[base], depth+1); l.parent == nil {
				return nil
			}
		default:
			return nil // another target branch, or a branch nobody mirrors
		}
		placed[pr.Head.Ref] = l
		order = append(order, l)
		return l
	}
	for _, ref := range sortedStringKeys(byHead) {
		place(byHead[ref], 0)
	}
	if len(order) == 0 {
		return nil
	}

	// Fetch the PR heads and the target branch.
	env, err := githubCredEnv(ctx, im.GitHub)
	if err != nil {
		return err
	}
	specs := []string{}
	for _, l := range order {
		specs = append(specs, "+refs/heads/"+l.pr.Head.Ref+":refs/remotes/github/"+l.pr.Head.Ref)
	}
	if _, err := im.Mirror.GitEnv(ctx, env, nil, append([]string{"fetch", "--quiet", im.GitHubURL}, specs...)...); err != nil {
		return err
	}
	if _, err := im.Mirror.Git(ctx, nil, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":refs/remotes/gerrit/heads/"+branch); err != nil {
		return err
	}

	done := map[*layer]*imported{}
	for _, l := range order {
		if l.parent != nil && done[l.parent] == nil {
			im.setSync(ctx, l.pr, "pending", fmt.Sprintf("Waiting for PR #%d to import", l.parent.pr.Number), "")
			continue
		}
		res, err := im.importLayer(ctx, branch, l, done)
		if err != nil {
			var se *syncError
			changed := false
			if errors.As(err, &se) {
				changed = im.setSync(ctx, l.pr, se.state, se.msg, "")
			} else {
				changed = im.setSync(ctx, l.pr, "error", "Import failed: "+err.Error(), "")
			}
			if changed {
				// Logged once per new problem, not on every resync.
				im.Logger.Warn("import failed", "pr", l.pr.Number, "err", err)
			}
			continue
		}
		done[l] = res
		desc := fmt.Sprintf("In sync with Gerrit change %d, patch set %d", res.change, res.patchSet)
		if res.bridgeOwned {
			desc += "; owned by the bridge (link your GitHub account to own it)"
		}
		if res.noSignOff {
			desc += "; no Signed-off-by: use git commit -s if the project requires it"
		}
		im.setSync(ctx, l.pr, "success", desc, im.Gerrit.ChangeURL(im.Project, res.change))
	}
	return nil
}

// SyncStatus is the commit status context reporting a GitHub-born PR's
// import (design §5.2).
const SyncStatus = "gerrit/sync"

// setSync sets gerrit/sync on the PR's head, unless it already says that,
// and reports whether it changed.
func (im *Importer) setSync(ctx context.Context, pr *github.PullRequest, state, desc, url string) bool {
	desc = truncateDesc(desc)
	key := strings.Join([]string{pr.Head.SHA, state, desc, url}, "\x00")
	im.mu.Lock()
	if im.lastSync == nil {
		im.lastSync = map[int]string{}
	}
	same := im.lastSync[pr.Number] == key
	im.mu.Unlock()
	if same {
		return false
	}
	if !im.DryRun {
		err := im.GitHub.CreateStatus(ctx, pr.Head.SHA, github.Status{State: state, Description: desc, TargetURL: url, Context: SyncStatus})
		if err != nil {
			im.Logger.Warn("setting gerrit/sync", "pr", pr.Number, "err", err)
			return true
		}
	}
	im.mu.Lock()
	im.lastSync[pr.Number] = key
	im.mu.Unlock()
	return true
}

// knownHeads maps GitHub commits of change n's revisions to Gerrit commits.
func knownHeads(st *state.Change) map[string]string {
	m := map[string]string{}
	for _, r := range st.Revisions {
		if r.GitHubSHA != "" {
			m[r.GitHubSHA] = r.GerritSHA
		}
		m[r.GerritSHA] = r.GerritSHA
	}
	return m
}

func (im *Importer) importLayer(ctx context.Context, branch string, l *layer, done map[*layer]*imported) (*imported, error) {
	pr := l.pr
	h := pr.Head.SHA
	n := im.Store.ChangeForPR(pr.Number)
	var st *state.Change
	if n != 0 {
		var err error
		if st, err = im.Store.Load(ctx, n); err != nil {
			return nil, err
		}
		if st.State.Origin != "github" {
			return nil, fmt.Errorf("PR #%d is linked to Gerrit-born change %d", pr.Number, n)
		}
	}

	// The layer's real parent (design §5.2 step 1).
	var parentHeads map[string]string
	switch {
	case l.parent != nil:
		parentHeads = done[l.parent].heads
	case l.gerritParentChange != 0:
		pst, err := im.Store.Load(ctx, l.gerritParentChange)
		if err != nil {
			return nil, err
		}
		parentHeads = knownHeads(pst)
	}
	own, gerritParent, err := im.realParent(ctx, branch, h, parentHeads)
	if err != nil {
		return nil, err
	}
	res := &imported{heads: map[string]string{}}
	if st != nil {
		for gh, ge := range knownHeads(st) {
			res.heads[gh] = ge
		}
	}
	importHash := prTextHash(pr)
	if st != nil && res.heads[h] != "" && st.State.ImportHash == importHash {
		// Already imported and the PR text is unchanged. A draft toggle
		// is not a new patch set: mirror it to Gerrit's WIP flag.
		res.change = st.State.Change
		for _, r := range st.Revisions {
			if r.GitHubSHA == h {
				res.patchSet = r.PatchSet
			}
		}
		var err error
		res.bridgeOwned, err = im.syncWIP(ctx, st.State.Change, pr)
		if ge := res.heads[h]; ge != "" {
			res.noSignOff = im.lacksSignOff(ctx, ge)
		}
		return res, err
	}
	if len(own) == 0 {
		return nil, &syncError{"pending", "No commits of its own yet"}
	}

	// The change's identity.
	changeID := ""
	if st != nil {
		changeID = st.State.ChangeID
	}
	if changeID == "" {
		for i := len(own) - 1; i >= 0 && changeID == ""; i-- {
			msg, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%B", own[i])
			if err != nil {
				return nil, err
			}
			if sm := changeIDTrailer.FindStringSubmatch(string(msg)); sm != nil {
				changeID = sm[1]
			}
		}
	}
	if changeID == "" {
		changeID = fmt.Sprintf("I%x", sha1.Sum([]byte(im.GitHub.Owner+"/"+im.GitHub.Repo+"\x00"+pr.NodeID)))
	}
	existing, err := im.Gerrit.QueryChanges(ctx, fmt.Sprintf("project:%s branch:%s change:%s",
		gerrit.QueryString(im.Project), gerrit.QueryString(branch), changeID), "CURRENT_REVISION", "CURRENT_COMMIT")
	if err != nil {
		return nil, err
	}
	var cur *gerrit.ChangeInfo
	if len(existing) > 0 {
		cur = &existing[0]
		if st == nil || st.State.Change != cur.Number {
			// Never let two PRs fight over one change.
			return nil, &syncError{"failure", fmt.Sprintf("Change-Id %s already belongs to Gerrit change %d; give this PR's commits a new Change-Id", changeID, cur.Number)}
		}
	}

	// Who uploads. The first upload decides the owner, so it is the PR's
	// author; later patch sets are uploaded as whoever pushed them.
	login := pr.User.Login
	if st != nil {
		if p := im.pusher(ctx, h); p != "" {
			login = p
		}
	}
	user := im.Identity[login]
	if user != "" && !im.Uploader.CanActAs() {
		user = ""
	}
	committer := im.Bridge
	if user != "" {
		var acct gerrit.AccountInfo
		if err := im.Gerrit.Do(ctx, http.MethodGet, "accounts/"+url.PathEscape(user), nil, &acct); err != nil {
			return nil, fmt.Errorf("Gerrit account %s for @%s: %w", user, login, err)
		}
		committer = acct
	}

	// Both moved since the last sync? Gerrit wins (design §5.3).
	gerritMoved := false
	if st != nil && cur != nil && len(st.Revisions) > 0 && cur.CurrentRevision != st.Revisions[len(st.Revisions)-1].GerritSHA && res.heads[h] == "" {
		gerritMoved = true
	}

	s, err := im.squash(ctx, pr, own, h, gerritParent, changeID, committer)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.CurrentRevision == s {
		res.heads[h] = s
		res.change, res.patchSet = cur.Number, cur.Revisions[s].Number
		return res, nil
	}
	if im.DryRun {
		im.Logger.Info("dry run: would import PR", "pr", pr.Number, "change_id", changeID, "commit", short(s), "as", user)
		res.heads[h] = s
		return res, nil
	}
	opts := []string{"ready"}
	if pr.Draft {
		opts = []string{"wip"}
	}
	if err := im.Uploader.Upload(ctx, user, s, branch, opts); err != nil {
		return nil, fmt.Errorf("uploading PR #%d: %w", pr.Number, err)
	}
	got, err := im.Gerrit.QueryChanges(ctx, fmt.Sprintf("project:%s branch:%s change:%s",
		gerritQuery(im.Project), gerritQuery(branch), changeID), "CURRENT_REVISION", "ALL_REVISIONS")
	if err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("uploaded PR #%d but change %s is missing", pr.Number, changeID)
	}
	ch := got[0]
	ps := ch.Revisions[s].Number
	if ps == 0 {
		return nil, fmt.Errorf("uploaded PR #%d but %s is not a patch set of change %d", pr.Number, short(s), ch.Number)
	}
	im.Logger.Info("imported PR", "pr", pr.Number, "change", ch.Number, "patch_set", ps, "as", or(user, "bridge"), "commit", short(s), "github_head", short(h))
	if st == nil {
		if err := im.link(ctx, ch.Number, "github", branch, changeID, pr); err != nil {
			return nil, err
		}
	}
	tree, parentTree := revisionTrees(ctx, im.Mirror, s)
	err = updateStateFn(ctx, im.Store, ch.Number, fmt.Sprintf("Import PR #%d head %s as patch set %d", pr.Number, short(h), ps), func(st *state.Change) {
		st.State.ImportHash = importHash
		st.State.HeadBranch = pr.Head.Ref
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: s, GitHubSHA: h, Tree: tree, ParentTree: parentTree})
	})
	if err != nil {
		return nil, err
	}
	res.heads[h] = s
	res.change, res.patchSet, res.bridgeOwned = ch.Number, ps, user == ""
	res.noSignOff = im.lacksSignOff(ctx, s)
	if im.onImported != nil {
		if err := im.onImported(ctx, ch.Number); err != nil {
			im.Logger.Warn("post-import sync failed", "change", ch.Number, "err", err)
		}
	}

	if gerritMoved {
		// Re-upload Gerrit's newer patch set on top, so it stays current;
		// the code mirror then pushes it back to the PR.
		if err := im.restoreGerrit(ctx, ch.Number, branch, cur, pr); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func gerritQuery(s string) string { return gerrit.QueryString(s) }

// realParent walks h's first parents to the layer's real parent: a known
// head of the parent layer, or for a bottom layer the target branch. It
// returns the layer's own commits (newest first) and the Gerrit commit to
// parent the squash on.
func (im *Importer) realParent(ctx context.Context, branch, h string, parentHeads map[string]string) ([]string, string, error) {
	out, err := im.Mirror.Git(ctx, nil, "rev-list", "--first-parent", "--max-count=1000", h, "--not", "refs/remotes/gerrit/heads/"+branch)
	if err != nil {
		return nil, "", err
	}
	commits := strings.Fields(string(out))
	if parentHeads == nil {
		if len(commits) == 0 {
			return nil, "", nil
		}
		p, err := im.Mirror.Git(ctx, nil, "rev-parse", commits[len(commits)-1]+"^")
		if err != nil {
			return nil, "", err
		}
		return commits, strings.TrimSpace(string(p)), nil
	}
	for i, c := range commits {
		if ge, ok := parentHeads[c]; ok {
			return commits[:i], ge, nil
		}
	}
	return nil, "", &syncError{"pending", "Restack needed: this PR is not on top of any version of the PR below it that Gerrit knows"}
}

// prBodyForMessage is the PR body without the bridge's links block and
// without Change-Id lines (the bridge adds its own).
func prBodyForMessage(body string) string {
	user, _ := splitBody(body)
	user = changeIDTrailer.ReplaceAllString(user, "")
	user = signedOffBy.ReplaceAllString(user, "")
	return strings.TrimSpace(user)
}

// signedOffBy matches DCO sign-off trailers. Projects like AOS require one
// (a Signed-off-by submit requirement), so a squash must keep them.
var signedOffBy = regexp.MustCompile(`(?m)^Signed-off-by: .+$`)

// signOffs collects the distinct Signed-off-by lines of texts, in order.
func signOffs(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range texts {
		for _, l := range signedOffBy.FindAllString(strings.ReplaceAll(t, "\r\n", "\n"), -1) {
			l = strings.TrimSpace(l)
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// squash builds the deterministic commit for a layer (design §5.2), or
// reuses h itself when nothing needs rewriting.
func (im *Importer) squash(ctx context.Context, pr *github.PullRequest, own []string, h, parent, changeID string, committer gerrit.AccountInfo) (string, error) {
	info, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%P%x00%an%x00%ae%x00%ad%x00%ce%x00%cd%x00%B", "--date=raw", h)
	if err != nil {
		return "", err
	}
	f := strings.SplitN(string(info), "\x00", 7)
	if len(f) != 7 {
		return "", fmt.Errorf("parsing commit %s", h)
	}
	hParents, an, ae, ad, ce, cd, hMsg := f[0], f[1], f[2], f[3], f[4], f[5], f[6]
	if len(own) == 1 && strings.TrimSpace(hParents) == parent && strings.EqualFold(ce, committer.Email) {
		if sm := changeIDTrailer.FindStringSubmatch(hMsg); sm != nil && sm[1] == changeID {
			return h, nil // no-op squash: identical SHAs on both sides
		}
	}
	// Sign-offs from every commit in the layer (oldest first) and from the
	// PR description are kept; the squashed commit represents them all.
	texts := []string{}
	for i := len(own) - 1; i >= 0; i-- {
		m, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%B", own[i])
		if err != nil {
			return "", err
		}
		texts = append(texts, string(m))
	}
	user, _ := splitBody(pr.Body)
	texts = append(texts, user)
	var msg strings.Builder
	msg.WriteString(strings.TrimSpace(pr.Title))
	if body := prBodyForMessage(pr.Body); body != "" {
		msg.WriteString("\n\n" + body)
	}
	msg.WriteString("\n\n")
	for _, so := range signOffs(texts...) {
		msg.WriteString(so + "\n")
	}
	fmt.Fprintf(&msg, "GitHub-PR: %s\nChange-Id: %s\n", pr.HTMLURL, changeID)
	tree, err := im.Mirror.Git(ctx, nil, "rev-parse", h+"^{tree}")
	if err != nil {
		return "", err
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + an, "GIT_AUTHOR_EMAIL=" + ae, "GIT_AUTHOR_DATE=" + ad,
		"GIT_COMMITTER_NAME=" + committer.Name, "GIT_COMMITTER_EMAIL=" + committer.Email, "GIT_COMMITTER_DATE=" + cd,
	}
	out, err := im.Mirror.GitEnv(ctx, env, []byte(msg.String()), "commit-tree", strings.TrimSpace(string(tree)), "-p", parent, "-F", "-")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// restoreGerrit re-uploads the tree of Gerrit's newer patch set (cur) as
// a new patch set after a GitHub push was imported over it, and says so
// on both sides.
func (im *Importer) restoreGerrit(ctx context.Context, n int, branch string, cur *gerrit.ChangeInfo, pr *github.PullRequest) error {
	rev, _ := cur.Current()
	if _, err := im.Mirror.Git(ctx, nil, "fetch", "--quiet", "origin", rev.Ref); err != nil {
		return err
	}
	out, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%T%x00%P%x00%B", cur.CurrentRevision)
	if err != nil {
		return err
	}
	f := strings.SplitN(string(out), "\x00", 3)
	if len(f) != 3 {
		return fmt.Errorf("parsing %s", cur.CurrentRevision)
	}
	env := []string{"GIT_COMMITTER_NAME=" + im.Bridge.Name, "GIT_COMMITTER_EMAIL=" + im.Bridge.Email}
	c, err := im.Mirror.GitEnv(ctx, env, []byte(f[2]), "commit-tree", f[0], "-p", strings.Fields(f[1])[0], "-F", "-")
	if err != nil {
		return err
	}
	if err := im.Uploader.Upload(ctx, "", strings.TrimSpace(string(c)), branch, nil); err != nil {
		return err
	}
	note := fmt.Sprintf("Both GitHub (PR #%d) and Gerrit (patch set %d) changed this at the same time. "+
		"The GitHub version was uploaded as a patch set, then Gerrit's version was restored on top; "+
		"Gerrit wins conflicts. Compare the last two patch sets to merge the GitHub changes.", pr.Number, rev.Number)
	if err := im.Gerrit.SetReview(ctx, n, "current", gerrit.ReviewInput{Message: note, Tag: commentTag + ":conflict"}); err != nil {
		return err
	}
	_, err = im.GitHub.CreateIssueComment(ctx, pr.Number, note+"\n\n"+noticeMarker)
	im.Logger.Warn("both sides changed; Gerrit wins", "change", n, "pr", pr.Number)
	return err
}

// revisionTrees returns tree(rev) and tree(rev^).
func revisionTrees(ctx context.Context, m *gitmirror.Repo, rev string) (tree, parent string) {
	if out, err := m.Git(ctx, nil, "rev-parse", rev+"^{tree}", rev+"^^{tree}"); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			return f[0], f[1]
		}
	}
	return "", ""
}

func githubCredEnv(ctx context.Context, gh *github.Client) ([]string, error) {
	tok, err := gh.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	return gitmirror.CredentialEnv("https://github.com", "x-access-token", tok), nil
}

// logicalBase is the branch a PR on a frozen base belongs on: the head of
// the change that its last imported Gerrit commit's parent belongs to, or
// the target branch.
func (im *Importer) logicalBase(ctx context.Context, branch string, pr int) (string, error) {
	n := im.Store.ChangeForPR(pr)
	if n == 0 {
		return branch, nil
	}
	st, err := im.Store.Load(ctx, n)
	if err != nil || len(st.Revisions) == 0 {
		return branch, err
	}
	out, err := im.Mirror.Git(ctx, nil, "rev-parse", st.Revisions[len(st.Revisions)-1].GerritSHA+"^")
	if err != nil {
		return branch, nil // objects not here yet; treat as on the branch
	}
	parent := strings.TrimSpace(string(out))
	for _, m := range im.Store.ChangesOnBranch(branch) {
		pst, err := im.Store.Load(ctx, m)
		if err != nil {
			return "", err
		}
		for _, r := range pst.Revisions {
			if r.GerritSHA == parent && pst.State.Lifecycle == "NEW" {
				return pst.State.HeadBranch, nil
			}
		}
	}
	return branch, nil
}

// pusher is who pushed GitHub commit sha: from a webhook if one said, else
// the commit's GitHub committer.
func (im *Importer) pusher(ctx context.Context, sha string) string {
	if im.Pushers != nil {
		if p := im.Pushers.Lookup(sha); p != "" {
			return p
		}
	}
	return commitPusher(ctx, im.GitHub, sha)
}

// prTextHash identifies the PR text a commit message is built from.
func prTextHash(pr *github.PullRequest) string {
	return hashString(pr.Title + "\x00" + prBodyForMessage(pr.Body))
}

// syncWIP makes Gerrit's work-in-progress flag follow the PR's draft
// state, acting as the change owner (Run-As) since only the owner may
// toggle it.
// It reports whether the bridge owns the change.
func (im *Importer) syncWIP(ctx context.Context, n int, pr *github.PullRequest) (bool, error) {
	ch, err := im.Gerrit.GetChange(ctx, n, "DETAILED_ACCOUNTS")
	if err != nil {
		return false, err
	}
	bridgeOwned := ch.Owner.AccountID == im.Bridge.AccountID
	if ch.Status != "NEW" || ch.WorkInProgress == pr.Draft {
		return bridgeOwned, nil
	}
	action := "ready"
	if pr.Draft {
		action = "wip"
	}
	if im.DryRun {
		im.Logger.Info("dry run: would mark change "+action, "change", n, "pr", pr.Number)
		return bridgeOwned, nil
	}
	gc := im.Gerrit
	if !bridgeOwned && ch.Owner.Username != "" {
		gc = gc.As(ch.Owner.Username)
	}
	msg := fmt.Sprintf("PR #%d marked %s on GitHub.", pr.Number, map[bool]string{true: "draft", false: "ready for review"}[pr.Draft])
	if err := gc.Do(ctx, http.MethodPost, fmt.Sprintf("changes/%d/%s", n, action), map[string]string{"message": msg}, nil); err != nil {
		return bridgeOwned, fmt.Errorf("marking change %d %s: %w", n, action, err)
	}
	im.Logger.Info("mirrored draft state to Gerrit", "change", n, "pr", pr.Number, "wip", pr.Draft)
	return bridgeOwned, nil
}

// lacksSignOff reports whether commit's message has no Signed-off-by.
func (im *Importer) lacksSignOff(ctx context.Context, commit string) bool {
	m, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%B", commit)
	return err == nil && !signedOffBy.Match(m)
}
