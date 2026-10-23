package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
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
	Identity Identity
	// Bridge is the bridge's own Gerrit account (committer of imports it
	// owns).
	Bridge        gerrit.AccountInfo
	DefaultBranch string
	// CreatedAfter skips PRs opened before it (zero: import all).
	CreatedAfter time.Time
	// Pushers says who pushed which commit (from webhooks).
	Pushers *Pushers
	// Trusted reports whether a Gerrit account is in CI's trusted group
	// (see Code.Trusted).
	Trusted func(ctx context.Context, account int) (trusted, known bool)
	// ChangeIDKey keys the Change-Ids derived for PRs (derivedChangeID):
	// a secret, so they cannot be worked out in advance.
	ChangeIDKey []byte
	DryRun      bool
	Logger      *slog.Logger

	mu       sync.Mutex
	lastSync map[int]string // PR -> last gerrit/sync status set
	// refused are PR heads refused for good (too big, or failing git's
	// object checks), by SHA: they are not fetched again.
	refused map[string]string
	// slow are PRs whose head took too long to fetch, until when none of
	// their heads is fetched: a fetch cut off keeps nothing, so each new
	// head of a huge branch would cost the full deadline, under the
	// branch's lock, every time (fourth audit).
	slow map[int]time.Time

	// onImported runs after a patch set is imported (Code.OnExported).
	onImported func(ctx context.Context, n int) error
	// link records a new PR <-> change link (Code.link).
	link func(ctx context.Context, n int, origin, branch, changeID string, pr *github.PullRequest) error
}

// layer is one GitHub-born PR being imported.
type layer struct {
	pr     *github.PullRequest
	listed time.Time // when GitHub said pr's head is what it is
	parent *layer    // GitHub-born parent layer, if any
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
	// gerritAhead is Gerrit's newer patch set, for a fork the bridge may
	// not push it to.
	gerritAhead int
}

// syncError is an import problem to show on the PR as gerrit/sync.
type syncError struct {
	state string // pending (the user can fix it by pushing), failure
	msg   string
	retry bool // worth trying again soon, without a push
}

func (e *syncError) Error() string { return e.msg }

var changeIDTrailer = regexp.MustCompile(`(?m)^Change-Id: (I[0-9a-f]{40})\s*$`)

// changeIDLine matches any line that might name a Change-Id, however it
// is spelled or spaced (Gerrit's keys are case-insensitive, and JGit's
// footer parsing has its own rules about spacing).
var changeIDLine = regexp.MustCompile(`(?im)^[ \t]*change-id[ \t]*:[ \t]*(\S*).*$`)

// changeLinkLine matches a Link footer naming a change, which Gerrit also
// reads as a Change-Id (receive.enableChangeIdLinkFooters).
var changeLinkLine = regexp.MustCompile(`(?im)^[ \t]*link[ \t]*:.*/id/I[0-9a-f].*$`)

// canonicalChangeID is the one spelling the bridge accepts.
var canonicalChangeID = regexp.MustCompile(`^Change-Id: (I[0-9a-f]{40})$`)

// footerChangeID returns the Change-Id a PR commit names: its one
// Change-Id footer, in its last paragraph, spelled exactly "Change-Id:
// I<40 hex>". ok is false for anything else that might name a change
// (another Change-Id line anywhere, any other spelling, a Link footer to
// a change): the bridge must never pick a different change than Gerrit
// would. The commit it uploads carries its own message anyway.
func footerChangeID(msg string) (id string, ok bool) {
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "\r\n", "\n"))
	if changeLinkLine.MatchString(msg) {
		return "", false
	}
	all := changeIDLine.FindAllString(msg, -1)
	if len(all) == 0 {
		return "", true
	}
	last := msg
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		last = msg[i+2:]
	}
	footer := changeIDLine.FindAllString(last, -1)
	if len(footer) != 1 || len(all) != 1 {
		return "", false
	}
	sm := canonicalChangeID.FindStringSubmatch(strings.TrimRight(footer[0], " \t\r"))
	if sm == nil {
		return "", false
	}
	return sm[1], true
}

// refuse remembers why PR pr's head sha is not imported. Keyed by PR too:
// anyone can open a PR listing someone else's head, then move it.
func (im *Importer) refuse(pr int, sha, why string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.refused == nil || len(im.refused) > 10000 {
		im.refused = map[string]string{} // bounded: forgetting costs a refetch
	}
	im.refused[fmt.Sprintf("%d %s", pr, sha)] = why
}

// slowFor is how long a PR whose head was too slow to fetch is not
// fetched again, whatever its head.
const slowFor = time.Hour

func (im *Importer) coolOff(pr int) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.slow == nil || len(im.slow) > 10000 {
		im.slow = map[int]time.Time{}
	}
	im.slow[pr] = time.Now().Add(slowFor)
}

func (im *Importer) slowUntil(pr int) time.Time {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.slow[pr]
}

func (im *Importer) refusedWhy(pr int, sha string) string {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.refused[fmt.Sprintf("%d %s", pr, sha)]
}

// Import imports every GitHub-born PR whose chain targets branch.
func (im *Importer) Import(ctx context.Context, branch string) error {
	prs, err := listOpenPulls(ctx, im.GitHub)
	listed := time.Now() // when GitHub said what prs' heads are
	if err == nil {
		err = im.prunePullRefs(ctx, prs)
	}
	if err != nil {
		return err
	}
	// Every linked PR's head, as GitHub has it now: what comments and
	// reviews from now on are about (state.Head).
	for i := range prs {
		if n := im.Store.ChangeForPR(prs[i].Number); n != 0 && !im.DryRun {
			if err := noteHead(ctx, im.Store, n, prs[i].Head.SHA, listed); err != nil {
				return err
			}
		}
	}
	repo := im.GitHub.Owner + "/" + im.GitHub.Repo
	// Gerrit-born PR heads on this branch, which GitHub PRs may stack on,
	// and which people may push to like any PR branch.
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
	// Where a GitHub-born layer may sit: this branch, or another open PR's
	// head (a stack, a Gerrit-born change's PR's too).
	bases := map[string]bool{branch: true}
	for i := range prs {
		if prs[i].HeadRepo() == repo {
			bases[prs[i].Head.Ref] = true
		}
	}
	byHead := map[string]*github.PullRequest{}
	for i := range prs {
		pr := &prs[i]
		// Fork PRs are imported too (their authors may not be able to
		// push to the repository), keyed by fork and branch.
		if pr.HeadRepo() == "" {
			continue // the fork was deleted
		}
		key := pr.HeadKey(repo)
		if key == pr.Head.Ref && (strings.HasPrefix(key, headPrefix) || strings.HasPrefix(key, frozenPrefix)) {
			// The bridge's branch for a Gerrit-born change: imported only
			// when someone else pushed to it (design §5.2.2).
			if n := gerritHeads[key]; n != 0 && im.Store.ChangeForPR(pr.Number) == n {
				st, err := im.Store.Load(ctx, n)
				if err != nil {
					return err
				}
				if knownHeads(st)[pr.Head.SHA] == "" {
					byHead[key] = pr
				}
			}
			continue
		}
		if pr.CreatedAt.Before(im.CreatedAfter) && im.Store.ChangeForPR(pr.Number) == 0 {
			continue // predates the bridge; leave it alone
		}
		if key == pr.Head.Ref {
			// The bridge pushes Gerrit's patch sets to the PR's branch: never
			// accept a branch Gerrit owns (replicated from Gerrit).
			owned := key == im.DefaultBranch
			if !owned {
				var err error
				if owned, err = im.Gerrit.BranchExists(ctx, im.Project, key); err != nil {
					return err
				}
			}
			if owned {
				// Not said on the PR's head, which is that branch's tip, for
				// everyone: anyone may open a PR from main (eighth audit).
				im.Logger.Info("not importing a PR from a branch Gerrit owns", "pr", pr.Number, "branch", key)
				continue
			}
		}
		if other := byHead[key]; other != nil && !im.rather(pr, other, bases) {
			continue
		}
		byHead[key] = pr
	}
	if len(byHead) == 0 {
		return nil
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
		if l, ok := placed[pr.HeadKey(repo)]; ok {
			return l
		}
		if depth > 50 {
			return nil
		}
		l := &layer{pr: pr, listed: listed}
		base := baseOf[pr.Number]
		switch {
		case base == branch:
		case byHead[base] != nil: // being imported too (say, a restack)
			if l.parent = place(byHead[base], depth+1); l.parent == nil {
				return nil
			}
		case gerritHeads[base] != 0:
			l.gerritParentChange = gerritHeads[base]
		default:
			return nil // another target branch, or a branch nobody mirrors
		}
		placed[pr.HeadKey(repo)] = l
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
	specs := map[*layer]string{}
	var all []string
	failed := map[*layer]string{}
	for _, l := range order {
		if why := im.refusedWhy(l.pr.Number, l.pr.Head.SHA); why != "" {
			failed[l] = why // not fetched again
			continue
		}
		if until := im.slowUntil(l.pr.Number); time.Now().Before(until) {
			failed[l] = fmt.Sprintf("Fetching it took more than %v: too big to import; not tried again before %s", fetchOneTimeout, until.UTC().Format(time.RFC3339))
			continue
		}
		spec := "+refs/heads/" + l.pr.Head.Ref + ":refs/remotes/github/heads/" + l.pr.Head.Ref
		if _, _, fork := github.SplitHeadKey(l.pr.HeadKey(repo)); fork {
			// GitHub keeps every PR's head in the base repository.
			spec = fmt.Sprintf("+refs/pull/%d/head:refs/remotes/github/pull/%d", l.pr.Number, l.pr.Number)
		}
		specs[l] = spec
		all = append(all, spec)
	}
	// Size limits apply once a head is fetched: a deadline bounds what a
	// huge one costs first (git has no fetch-side size limit).
	fetch := func(timeout time.Duration, specs ...string) error {
		fctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := im.Mirror.GitEnv(fctx, env, nil, append([]string{"-c", fsckFetch, "fetch", "--quiet", im.GitHubURL}, specs...)...)
		if fctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return errFetchTooSlow
		}
		return err
	}
	var retry error // a PR head that could not be fetched for now
	if len(all) > 0 {
		if err := fetch(fetchAllTimeout, all...); err != nil {
			// One PR's bad objects must not stop the others: fetch each
			// alone. A head that fails git's object checks is refused for
			// good; one that fails otherwise (a new PR's head GitHub has
			// not published yet, the network) waits for a retry.
			for _, l := range order {
				if specs[l] == "" {
					continue
				}
				ferr := fetch(fetchOneTimeout, specs[l])
				var ge *gitmirror.Error
				switch {
				case ferr == nil:
				case errors.Is(ferr, errFetchTooSlow):
					failed[l] = fmt.Sprintf("Fetching it took more than %v: too big to import", fetchOneTimeout)
					im.refuse(l.pr.Number, l.pr.Head.SHA, failed[l])
					im.coolOff(l.pr.Number)
					im.Logger.Warn("PR head too slow to fetch", "pr", l.pr.Number)
				case errors.As(ferr, &ge) && strings.Contains(ge.Stderr, "fsck error"):
					failed[l] = "Its commits fail git's object checks (fsck), so they are not imported"
					im.refuse(l.pr.Number, l.pr.Head.SHA, failed[l])
					im.Logger.Warn("PR head fails git's object checks", "pr", l.pr.Number, "err", ferr)
				default:
					failed[l] = "" // skipped this time
					retry = cmp.Or(retry, fmt.Errorf("fetching PR #%d: %w", l.pr.Number, ferr))
				}
			}
		}
	}
	if _, err := im.Mirror.Git(ctx, nil, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":refs/remotes/gerrit/heads/"+branch); err != nil {
		return err
	}

	done := map[*layer]*imported{}
	for _, l := range order {
		if why, ok := failed[l]; ok {
			if why != "" {
				im.setSync(ctx, l.pr, "failure", why, "")
			}
			continue
		}
		if l.parent != nil && done[l.parent] == nil {
			im.setSync(ctx, l.pr, "pending", fmt.Sprintf("Waiting for PR #%d to import", l.parent.pr.Number), "")
			continue
		}
		end := trace.Region(ctx, fmt.Sprintf("import PR #%d", l.pr.Number))
		res, err := im.importLayer(ctx, branch, l, done)
		end()
		if err != nil {
			var se *syncError
			changed := false
			if errors.As(err, &se) {
				changed = im.setSync(ctx, l.pr, se.state, se.msg, "")
				if se.retry {
					retry = cmp.Or(retry, err)
				}
			} else {
				// Details (hosts, git output) stay in the log: the
				// status is public.
				changed = im.setSync(ctx, l.pr, "error", "Import failed; the bridge's maintainers can see why in its log", "")
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
		if res.gerritAhead != 0 {
			desc += fmt.Sprintf("; newer patch set %d is only in Gerrit: allow maintainer edits to sync it", res.gerritAhead)
		}
		if res.noSignOff {
			desc += "; no Signed-off-by: use git commit -s if the project requires it"
		}
		im.setSync(ctx, l.pr, "success", desc, im.Gerrit.ChangeURL(im.Project, res.change))
	}
	// The others are imported; a retry fetches what could not be.
	return retry
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
	if out, _ := im.Mirror.Git(ctx, nil, "for-each-ref", "--count=1", "--contains", pr.Head.SHA, "refs/remotes/gerrit/heads/"); len(out) > 0 {
		// A commit on Gerrit's branch (anyone may open a PR from main):
		// a status on it is on everyone's, not this PR's (eighth audit).
		im.Logger.Info("not setting gerrit/sync on a commit on Gerrit's branch", "pr", pr.Number, "state", state, "description", desc)
		return false
	}
	if im.hasSync(ctx, pr.Head.SHA, state, desc, url) {
		// Said already (before a restart, which forgets lastSync): each
		// post is another line in the commit's public status history,
		// and the slowest call an import makes.
		im.mu.Lock()
		im.lastSync[pr.Number] = key
		im.mu.Unlock()
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

// hasSync reports whether commit sha's gerrit/sync status says state,
// desc and url already.
func (im *Importer) hasSync(ctx context.Context, sha, state, desc, url string) bool {
	sts, err := im.GitHub.Statuses(ctx, sha)
	if err != nil {
		return false
	}
	for _, st := range sts { // the latest of each context
		if st.Context == SyncStatus {
			return st.State == state && st.Description == desc && st.TargetURL == url
		}
	}
	return false
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
	}
	// A push to an existing change's PR, wherever the change started, is a
	// new patch set: the change keeps its commit message, owner and WIP
	// state, which PR edits reach separately (Code.mirrorPREdits).
	later := st != nil

	// The layer's real parent (design §5.2 step 1).
	var parentHeads map[string]string
	parentTip := "" // the parent layer's GitHub head
	switch {
	case l.parent != nil:
		parentHeads = done[l.parent].heads
		parentTip = l.parent.pr.Head.SHA
	case l.gerritParentChange != 0:
		pst, err := im.Store.Load(ctx, l.gerritParentChange)
		if err != nil {
			return nil, err
		}
		parentHeads = knownHeads(pst)
		if len(pst.Revisions) > 0 {
			parentTip = pst.Revisions[len(pst.Revisions)-1].GitHubSHA
		}
	}
	own, gerritParent, side, err := im.realParent(ctx, branch, h, parentHeads, parentTip)
	if err != nil {
		return nil, err
	}
	res := &imported{heads: map[string]string{}}
	if st != nil {
		for gh, ge := range knownHeads(st) {
			res.heads[gh] = ge
		}
	}
	if later && res.heads[h] != "" {
		// Already imported.
		res.change = st.State.Change
		for _, r := range st.Revisions {
			if r.GitHubSHA == h {
				res.patchSet = r.PatchSet
			}
		}
		res.noSignOff = im.lacksSignOff(ctx, res.heads[h])
		ci, err := im.Gerrit.GetChange(ctx, res.change, "CURRENT_REVISION", "DETAILED_ACCOUNTS")
		if err != nil {
			return nil, err
		}
		res.bridgeOwned = ci.Owner.AccountID == im.Bridge.AccountID
		if _, _, fork := github.SplitHeadKey(pr.HeadKey(im.GitHub.Owner + "/" + im.GitHub.Repo)); fork {
			// Listed PRs lack maintainer_can_modify.
			full, err := im.GitHub.GetPull(ctx, pr.Number)
			if err != nil {
				return nil, err
			}
			if cur, ok := ci.Current(); ok && !full.MaintainerCanModify && cur.Number > res.patchSet {
				res.gerritAhead = cur.Number
			}
		}
		return res, nil
	}
	if len(own) == 0 {
		return nil, &syncError{state: "pending", msg: "No commits of its own yet"}
	}
	// A stack's lower layers are counted in their own PRs.
	if err := im.checkSize(ctx, h, gerritParent, parentTip); err != nil {
		var se *syncError
		if errors.As(err, &se) {
			im.refuse(pr.Number, h, se.msg) // not fetched again
		}
		return nil, err
	}

	// The change's identity.
	changeID, fromCommits := "", false
	if st != nil {
		changeID = st.State.ChangeID
	}
	if changeID == "" {
		for i := len(own) - 1; i >= 0 && changeID == ""; i-- {
			msg, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%B", own[i])
			if err != nil {
				return nil, err
			}
			id, ok := footerChangeID(string(msg))
			if !ok {
				return nil, &syncError{state: "pending", msg: fmt.Sprintf("Commit %s has Change-Id lines Gerrit could read differently; keep at most one, in the last paragraph", own[i])}
			}
			changeID, fromCommits = id, id != ""
		}
	}
	if changeID == "" {
		changeID = im.derivedChangeID(pr)
	}
	find := func(id string) (*gerrit.ChangeInfo, error) {
		existing, err := im.Gerrit.QueryChanges(ctx, fmt.Sprintf("project:%s branch:%s change:%s",
			gerrit.QueryString(im.Project), gerrit.QueryString(branch), id), "ALL_REVISIONS", "ALL_COMMITS", "DETAILED_ACCOUNTS")
		if err != nil || len(existing) == 0 {
			return nil, err
		}
		return &existing[0], nil
	}
	cur, err := find(changeID)
	if err != nil {
		return nil, err
	}
	for cur != nil && st == nil {
		adopt, err := im.ownUpload(ctx, cur, pr, h)
		if err != nil {
			return nil, err
		}
		if adopt {
			// Uploaded for this PR, but the link was never saved (a
			// crash, a refused state push): link it, and import next
			// time.
			if err := im.link(ctx, cur.Number, "github", branch, changeID, pr); err != nil {
				return nil, err
			}
			im.Logger.Warn("adopted a change uploaded for a PR whose link was lost", "pr", pr.Number, "change", cur.Number)
			return nil, &syncError{state: "pending", msg: fmt.Sprintf("Linked to Gerrit change %d", cur.Number), retry: true}
		}
		if author, ok := im.Identity.GerritUserFor(ctx, pr.User); !fromCommits || (ok && strings.EqualFold(cur.Owner.Username, author) && cur.Owner.AccountID != im.Bridge.AccountID) {
			break // the author's own change: theirs to sort out
		}
		// Someone else's change has the Change-Id of this PR's commits,
		// which anyone can read once they are pushed (fourth audit): the
		// PR gets its own, which nobody can work out.
		im.Logger.Warn("the PR's Change-Id is another change's; using its own", "pr", pr.Number, "change", cur.Number, "change_id", changeID)
		changeID, fromCommits = im.derivedChangeID(pr), false
		if cur, err = find(changeID); err != nil {
			return nil, err
		}
	}
	if cur != nil {
		if st == nil || st.State.Change != cur.Number {
			// Never let two PRs fight over one change.
			return nil, &syncError{state: "failure", msg: fmt.Sprintf("Change-Id %s already belongs to Gerrit change %d; give this PR's commits a new Change-Id", changeID, cur.Number)}
		}
	}
	message := "" // the commit message, if not built from the PR
	base := ""    // the patch set GitHub last had: the push builds on it
	baseGH := ""  // the GitHub commit it was
	foreign := false
	if later {
		if cur == nil {
			return nil, fmt.Errorf("change %d (%s) not found", st.State.Change, changeID)
		}
		if rev, ok := cur.Revisions[h]; ok {
			// Gerrit's own commit (an export the bridge did not get to
			// record, a patch set uploaded meanwhile being current now):
			// nothing to import, and nobody's push. Recorded, so the code
			// mirror adds Gerrit's newer patch set on top.
			if err := im.recordGerritHead(ctx, cur.Number, rev.Number, h); err != nil {
				return nil, err
			}
			res.heads[h] = h
			res.change, res.patchSet = cur.Number, rev.Number
			return res, nil
		}
		base = cur.CurrentRevision
		for sha, r := range cur.Revisions {
			if baseGH == "" && r.Number == 1 && st.State.Origin == "github" {
				// None recorded (a lost link adopted, ownUpload): the
				// adopted upload, not whatever is current, someone else's
				// patch set perhaps, whose message would become the PR's
				// (seventh audit).
				base = sha
			}
		}
		for _, r := range st.Revisions {
			if _, ok := cur.Revisions[r.GerritSHA]; ok && r.GitHubSHA != "" {
				base, baseGH = r.GerritSHA, r.GitHubSHA
			}
		}
		if st.State.Origin == "github" {
			// A patch set someone else uploaded in Gerrit, or whose code
			// is someone else's below it, never reached the PR
			// (Code.markForeign): the push is not racing it, and it is
			// not restored over the push either (fifth audit: restored as
			// the bridge's, it reached the author's branch).
			rev, _ := cur.Current()
			ok, _ := authorsCode(ctx, im.Trusted, im.Bridge.AccountID, cur.Owner.AccountID, revisionsByNumber(cur), rev.Number)
			foreign = !ok
		}
		message = cur.Revisions[base].Commit.Message
		if message == "" {
			return nil, fmt.Errorf("change %d: no commit message for %s", cur.Number, short(base))
		}
	}

	user, login, err := im.uploader(ctx, pr, cur, base, baseGH, h, gerritParent, parentTip, branch)
	if err != nil {
		return nil, err
	}
	if side && user != "" {
		im.Logger.Info("the push merges in commits from beside the PR's own line: the bridge uploads it", "pr", pr.Number, "pusher", login, "uploader", user)
		user = ""
	}
	committer := im.Bridge
	var author github.User // as the bridge: whoever pushed all of it, if one account did
	switch {
	case user != "":
		var acct gerrit.AccountInfo
		if err := im.Gerrit.Do(ctx, http.MethodGet, "accounts/"+url.PathEscape(user), nil, &acct); err != nil {
			return nil, fmt.Errorf("Gerrit account %s for @%s: %w", user, login, err)
		}
		committer = acct
	case !side: // with commits from beside it, not all theirs, whoever pushed it
		if author, err = im.pushedBy(ctx, pr, h, gerritParent, parentTip); err != nil {
			return nil, err
		}
	}

	// Both moved since the last sync? Gerrit wins (design §5.3).
	gerritMoved := false
	if st != nil && cur != nil && !foreign && len(st.Revisions) > 0 && cur.CurrentRevision != st.Revisions[len(st.Revisions)-1].GerritSHA && res.heads[h] == "" {
		gerritMoved = true
	}

	s, err := im.squash(ctx, pr, own, h, gerritParent, changeID, message, committer, author, user != "")
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.CurrentRevision == s {
		// Uploaded already (the state was not saved after): record it.
		ps := cur.Revisions[s].Number
		if !im.DryRun && st != nil && knownHeads(st)[h] == "" {
			tree, parentTree := revisionTrees(ctx, im.Mirror, s)
			if err := updateStateFn(ctx, im.Store, cur.Number, fmt.Sprintf("Import PR #%d head %s as patch set %d", pr.Number, short(h), ps), func(st *state.Change) {
				st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: s, GitHubSHA: h, Tree: tree, ParentTree: parentTree, At: time.Now()})
			}); err != nil {
				return nil, err
			}
		}
		res.heads[h] = s
		res.change, res.patchSet = cur.Number, ps
		return res, nil
	}
	if im.DryRun {
		im.Logger.Info("dry run: would import PR", "pr", pr.Number, "change_id", changeID, "commit", short(s), "as", user)
		res.heads[h] = s
		return res, nil
	}
	// A new change starts as the PR is; a later patch set keeps the
	// change's WIP state.
	var opts []string
	if !later {
		opts = []string{wipState(pr.Draft)}
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
	if st == nil && ps != 1 {
		// Someone uploaded a change with this Change-Id, public once the
		// PR's commits are, between the bridge's look and its upload,
		// which became a patch set of theirs (sixth audit): not this
		// PR's. Next time, its Change-Id is someone else's.
		im.Logger.Warn("the PR's upload became a patch set of someone else's change; not linking it", "pr", pr.Number, "change", ch.Number, "patch_set", ps)
		return nil, &syncError{state: "pending", msg: fmt.Sprintf("Change-Id %s was taken by Gerrit change %d meanwhile; trying again", changeID, ch.Number), retry: true}
	}
	im.Logger.Info("imported PR", "pr", pr.Number, "change", ch.Number, "patch_set", ps, "as", or(user, "bridge"), "commit", short(s), "github_head", short(h))
	if st == nil {
		if err := im.link(ctx, ch.Number, "github", branch, changeID, pr); err != nil {
			return nil, err
		}
	}
	tree, parentTree := revisionTrees(ctx, im.Mirror, s)
	err = updateStateFn(ctx, im.Store, ch.Number, fmt.Sprintf("Import PR #%d head %s as patch set %d", pr.Number, short(h), ps), func(st *state.Change) {
		st.State.HeadBranch = pr.HeadKey(im.GitHub.Owner + "/" + im.GitHub.Repo)
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: s, GitHubSHA: h, Tree: tree, ParentTree: parentTree, At: time.Now()})
		st.Heads = withHead(st.Heads, h, l.listed)
	})
	if err != nil {
		return nil, err
	}
	res.heads[h] = s
	res.change, res.patchSet, res.bridgeOwned = ch.Number, ps, user == "" && st == nil
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
//
// A layer that merged its parent's current head (GitHub's "Update branch")
// sits on that head, and a bottom layer on its merge base with the target
// branch, so the squash's diff is the layer's own either way.
//
// side reports whether h also has commits beside those, merged in from
// neither the target branch nor that head: code someone else chose
// (GitHub's Update branch merges whatever the PR below has, which need
// not be the version found here: eighth audit).
func (im *Importer) realParent(ctx context.Context, branch, h string, parentHeads map[string]string, parentTip string) (own []string, parent string, side bool, err error) {
	const maxCommits = 1000
	target := "refs/remotes/gerrit/heads/" + branch
	// beside reports whether h has more commits than own above below.
	beside := func(own []string, below ...string) (bool, error) {
		out, err := im.Mirror.Git(ctx, nil, append([]string{"rev-list", "--count", fmt.Sprintf("--max-count=%d", maxCommits+1), h, "--not"}, below...)...)
		if err != nil {
			return false, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		return err != nil || n != len(own), nil
	}
	if ge := parentHeads[parentTip]; ge != "" {
		if _, err := im.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", parentTip, h); err == nil {
			out, err := im.Mirror.Git(ctx, nil, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", maxCommits+1), h, "--not", parentTip, target)
			if err != nil {
				return nil, "", false, err
			}
			commits := strings.Fields(string(out))
			if len(commits) > maxCommits {
				return nil, "", false, &syncError{state: "failure", msg: fmt.Sprintf("More than %d commits: too many to import", maxCommits)}
			}
			side, err := beside(commits, parentTip, target)
			return commits, ge, side, err
		}
	}
	out, err := im.Mirror.Git(ctx, nil, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", maxCommits+1), h, "--not", target)
	if err != nil {
		return nil, "", false, err
	}
	commits := strings.Fields(string(out))
	if len(commits) > maxCommits {
		return nil, "", false, &syncError{state: "failure", msg: fmt.Sprintf("More than %d commits: too many to import", maxCommits)}
	}
	if parentHeads == nil {
		if len(commits) == 0 {
			return nil, "", false, nil
		}
		// The squash's parent must already be in Gerrit, or the upload
		// would bring all its unreviewed ancestors along: the newest
		// commit of the target branch the layer contains.
		p, err := im.Mirror.Git(ctx, nil, "merge-base", h, target)
		parent := strings.TrimSpace(string(p))
		if err != nil || parent == "" {
			return nil, "", false, &syncError{state: "failure", msg: "This PR's history does not start from " + branch + ": rebase it onto " + branch}
		}
		side, err := beside(commits, target)
		return commits, parent, side, err
	}
	for i, c := range commits {
		if ge, ok := parentHeads[c]; ok {
			side, err := beside(commits[:i], c, target)
			return commits[:i], ge, side, err
		}
	}
	return nil, "", false, &syncError{state: "pending", msg: "Restack needed: this PR is not on top of any version of the PR below it that Gerrit knows"}
}

// prunePullRefs drops the mirror's copies of closed PRs' heads, so forks'
// objects do not pile up in it for good.
func (im *Importer) prunePullRefs(ctx context.Context, open []github.PullRequest) error {
	keep := map[string]bool{}
	for _, pr := range open {
		keep[fmt.Sprintf("refs/remotes/github/pull/%d", pr.Number)] = true
	}
	out, err := im.Mirror.Git(ctx, nil, "for-each-ref", "--format=%(refname)", "refs/remotes/github/pull/")
	if err != nil {
		return err
	}
	var del strings.Builder
	for _, ref := range strings.Fields(string(out)) {
		if !keep[ref] {
			fmt.Fprintf(&del, "delete %s\n", ref)
		}
	}
	if del.Len() == 0 {
		return nil
	}
	_, err = im.Mirror.Git(ctx, []byte(del.String()), "update-ref", "--stdin")
	return err
}

// fsckFetch makes git check every object fetched from GitHub (anyone's
// fork) before keeping it: malformed trees, .gitmodules tricks and the
// like are refused before they reach Gerrit and its users' clones.
const fsckFetch = "fetch.fsckObjects=true"

// Bounds on what one PR may bring (design §7.1): Gerrit keeps every patch
// set's objects for good, so a stranger must not be able to fill it. Set
// Gerrit's receive.maxObjectSizeLimit too.
var (
	maxImportFile  int64 = 25 << 20
	maxImportTotal int64 = 100 << 20
)

// checkSize refuses a head whose new objects (those parent lacks) include
// a blob over maxImportFile or add up to more than maxImportTotal.
func (im *Importer) checkSize(ctx context.Context, h, parent, parentTip string) error {
	not := []string{"rev-list", "--objects", "--no-object-names", h, "--not", parent}
	if parentTip != "" {
		not = append(not, parentTip)
	}
	list, err := im.Mirror.Git(ctx, nil, not...)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(list)) == 0 {
		return nil
	}
	sizes, err := im.Mirror.Git(ctx, list, "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if err != nil {
		return err
	}
	var total int64
	for _, l := range strings.Split(strings.TrimSpace(string(sizes)), "\n") {
		typ, n, _ := strings.Cut(l, " ")
		size, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return fmt.Errorf("sizing %s: %q", short(h), l)
		}
		if typ == "blob" && size > maxImportFile {
			return &syncError{state: "failure", msg: fmt.Sprintf("A file is larger than %d MB: too big to import", maxImportFile>>20)}
		}
		total += size
	}
	if total > maxImportTotal {
		return &syncError{state: "failure", msg: fmt.Sprintf("This PR adds more than %d MB: too big to import", maxImportTotal>>20)}
	}
	return nil
}

// uploader says who uploads h's squash onto parent (design §7.1): "" is
// the bridge. CI trusts the uploader, so nobody is made the uploader of
// code they did not write or vouch for, however casually they clicked a
// GitHub button (Update branch, Commit suggestion) on someone else's PR.
//
// The first upload decides the owner, so it is the PR's author's, if they
// pushed everything in its head too: anyone may open a PR from someone
// else's branch (a maintainer from a stranger's fork, say), and a branch
// may move before the bridge sees the PR. A later one is the pusher's
// (as GitHub says; never guessed) if they uploaded base, the patch set
// GitHub last had (baseGH), too, and pushed all of it baseGH lacks. A pure
// rebase of base is uploaded on behalf of its uploader, as Gerrit's own
// rebase is. Anything else is the bridge's, and needs a trusted reviewer
// to start CI.
func (im *Importer) uploader(ctx context.Context, pr *github.PullRequest, cur *gerrit.ChangeInfo, base, baseGH, h, parent, parentTip, branch string) (user, login string, err error) {
	if !im.Uploader.CanActAs() {
		return "", "", nil
	}
	if base == "" {
		return im.firstUploader(ctx, pr, h, parent, parentTip)
	}
	rev := cur.Revisions[base]
	prev, prevID := rev.Uploader.Username, rev.Uploader.AccountID
	// Nobody acts as an inactive account: Gerrit may read its name as
	// someone else's.
	if rev.Uploader.AccountID == im.Bridge.AccountID || gerrit.AmbiguousUsername(prev) || rev.Uploader.Inactive {
		prev = ""
	}
	if prev == "" {
		return "", "", nil // the bridge's already: nothing to lose
	}
	by, err := im.pushedBy(ctx, pr, h, baseGH)
	if err != nil {
		return "", "", err
	}
	login = by.Login
	if !codeFrom(revisionsByNumber(cur), rev.Number, func(r gerrit.RevisionInfo) bool { return ownUploadBy(r, prevID, im.Bridge.AccountID) }) {
		// The push builds on base, whose code, or parent, is not prev's
		// own: their rebase or web edit of someone else's patch set, say,
		// which a GitHub button would then carry on as prev's code (sixth
		// audit).
		im.Logger.Info("the patch set below is not its uploader's own code: the bridge uploads the push", "pr", pr.Number, "pusher", login, "uploader", prev)
		return "", login, nil
	}
	if err := im.have(ctx, base, rev.Ref); err != nil {
		return "", "", err
	}
	if kept, err := im.keepsParents(ctx, rev, parent); err != nil {
		return "", "", err
	} else if !kept {
		im.Logger.Info("the squash's parent leaves out base's: their code would be in its diff: the bridge uploads it", "pr", pr.Number, "pusher", login, "uploader", prev)
		return "", login, nil
	}
	if pusher, ok := im.Identity.GerritUserFor(ctx, by); ok && strings.EqualFold(pusher, prev) {
		if parent != rev.Commit.Parent() {
			// It moved base's code onto a new parent too, whose code is
			// built with it: their Update branch onto the change below's
			// newer patch set, someone else's, say (seventh audit). As
			// for a rebase of it, below.
			trusted, err := im.parentTrusted(ctx, parent, base, branch, prevID)
			if err != nil {
				return "", "", err
			}
			if !trusted {
				im.Logger.Info("a push onto a parent the uploader did not upload: the bridge uploads it", "pr", pr.Number, "pusher", login, "uploader", prev)
				return "", login, nil
			}
		}
		return pusher, login, nil
	}
	pure, err := im.pureRebase(ctx, base, rev.Ref, parent, h)
	if err != nil {
		return "", "", err
	}
	if pure {
		// The new parent's code is built too: it must be the branch's,
		// or the uploader's own (second audit: anything else would let
		// someone rebase a trusted change onto their own code).
		trusted, err := im.parentTrusted(ctx, parent, base, branch, prevID)
		if err != nil {
			return "", "", err
		}
		if trusted {
			im.Logger.Info("pure rebase: uploading on behalf of the current patch set's uploader", "pr", pr.Number, "pusher", login, "uploader", prev)
			return prev, login, nil
		}
		im.Logger.Info("rebase onto a parent the uploader did not upload: the bridge uploads it", "pr", pr.Number, "pusher", login, "uploader", prev)
		return "", login, nil
	}
	im.Logger.Info("someone else's push, or pushes: the bridge uploads it", "pr", pr.Number, "pusher", login, "uploader", prev)
	return "", login, nil
}

// keepsParents reports whether each parent of rev, the patch set a push
// builds on, is below parent, the squash's, or an older patch set of
// parent's change. Otherwise its code is in the squash's diff, as the
// pusher's own: the change below abandoned, or the PR retargeted, so a
// layer is squashed onto the branch with that change folded in; a merge's
// other parent flattened (eighth audit).
func (im *Importer) keepsParents(ctx context.Context, rev gerrit.RevisionInfo, parent string) (bool, error) {
	ps := rev.Commit.ParentSHAs()
	if len(ps) == 0 {
		return false, nil
	}
	changesOf := func(commit string) (map[int]bool, error) {
		cs, err := im.Gerrit.QueryChanges(ctx, fmt.Sprintf("project:%s commit:%s", gerrit.QueryString(im.Project), commit))
		of := map[int]bool{}
		for _, c := range cs {
			of[c.Number] = true
		}
		return of, err
	}
	for _, p := range ps {
		if _, err := im.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", p, parent); err == nil {
			continue
		}
		mine, err := changesOf(p)
		if err != nil {
			return false, err
		}
		theirs, err := changesOf(parent)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(sortedKeys(mine), func(n int) bool { return theirs[n] }) {
			return false, nil
		}
	}
	return true, nil
}

// ownUploadBy reports whether r was uploaded by account, not the bridge
// nor anyone on their behalf, and, a REWORK, is their own commit (as CI
// asks: Gerrit keeps the author when someone edits, cherry-picks or
// rebases with conflicts someone else's commit, and takes someone else's
// commit as it is from whoever may forge committers). By account, not
// username, which a case-sensitive Gerrit lets two accounts spell alike.
func ownUploadBy(r gerrit.RevisionInfo, account, bridge int) bool {
	return account != 0 && r.Uploader.AccountID == account && account != bridge &&
		(r.RealUploader == nil || r.RealUploader.AccountID == r.Uploader.AccountID) && (r.Kind != "REWORK" || r.Own())
}

// firstUploader is who uploads pr's first patch set, h, onto parent, as
// above: its author if they pushed everything in it but code parent or
// parentTip, the PR below's head, has already, else the bridge (""). login
// is the pusher.
func (im *Importer) firstUploader(ctx context.Context, pr *github.PullRequest, h, parent, parentTip string) (user, login string, err error) {
	if !im.Uploader.CanActAs() {
		return "", "", nil
	}
	author, ok := im.Identity.GerritUserFor(ctx, pr.User)
	if !ok {
		return "", pr.User.Login, nil
	}
	by, err := im.pushedBy(ctx, pr, h, parent, parentTip)
	if err != nil {
		return "", "", err
	}
	login = by.Login
	if login == "" && time.Since(pr.CreatedAt) < pusherWait {
		// GitHub's activity log can lag a push by a little, and a
		// fork's push sends no webhook here: decided on a guess, the
		// change would be the bridge's for good.
		return "", "", &syncError{state: "pending", msg: "Waiting for GitHub to say who pushed it", retry: true}
	}
	if pusher, ok := im.Identity.GerritUserFor(ctx, by); !ok || !strings.EqualFold(pusher, author) {
		im.Logger.Info("not every push in it the PR author's (as GitHub says): the bridge uploads it", "pr", pr.Number, "author", pr.User.Login, "pusher", login)
		return "", login, nil
	}
	return author, pr.User.Login, nil
}

// parentTrusted reports whether parent is on Gerrit's branch, no older
// than the branch commit base was built on, or is the current patch set
// of a change whose code is user's own (codeFrom): what a rebase of base,
// on user's behalf, may be onto. Not an older base (old code, old CI
// config: a combination nobody vouched for), nor an older patch set of
// the change below, nor one with someone else's code (sixth audit).
func (im *Importer) parentTrusted(ctx context.Context, parent, base, branch string, user int) (bool, error) {
	target := "refs/remotes/gerrit/heads/" + branch
	if _, err := im.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", parent, target); err == nil {
		was, err := im.Mirror.Git(ctx, nil, "merge-base", base, target)
		if err != nil || strings.TrimSpace(string(was)) == "" {
			return false, nil
		}
		_, err = im.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", strings.TrimSpace(string(was)), parent)
		return err == nil, nil
	}
	cs, err := im.Gerrit.QueryChanges(ctx, fmt.Sprintf("project:%s commit:%s", gerrit.QueryString(im.Project), parent), "ALL_REVISIONS", "ALL_COMMITS", "DETAILED_ACCOUNTS")
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if rev, ok := c.Revisions[parent]; ok && c.CurrentRevision == parent &&
			codeFrom(revisionsByNumber(&c), rev.Number, func(r gerrit.RevisionInfo) bool { return ownUploadBy(r, user, im.Bridge.AccountID) }) {
			return true, nil
		}
	}
	return false, nil
}

// rather reports whether PR a should be imported for a head instead of
// b, another open PR from the same head: the one linked to a change, else
// one onto a base a layer may sit on (bases), else the older. Anyone may
// open a PR from someone's fork branch, into any base: that must not keep
// the author's own PR from being imported (ninth audit).
func (im *Importer) rather(a, b *github.PullRequest, bases map[string]bool) bool {
	linked := func(pr *github.PullRequest) bool { return im.Store.ChangeForPR(pr.Number) != 0 }
	here := func(pr *github.PullRequest) bool { return bases[pr.Base.Ref] }
	switch {
	case linked(a) != linked(b):
		return linked(a)
	case here(a) != here(b):
		return here(a)
	}
	return a.Number < b.Number // as before: the older (either will do)
}

// Fetch deadlines: all open PRs' heads at once, then each alone.
var (
	fetchAllTimeout = 2 * time.Minute
	fetchOneTimeout = 2 * time.Minute
	errFetchTooSlow = errors.New("fetch took too long")
)

// derivedChangeID is the Change-Id of a PR whose commits name none. Keyed
// with ChangeIDKey, so nobody can work it out from the PR and upload a
// change with it first, which would keep the PR from being imported.
func (im *Importer) derivedChangeID(pr *github.PullRequest) string {
	msg := []byte(im.GitHub.Owner + "/" + im.GitHub.Repo + "\x00" + pr.NodeID)
	if len(im.ChangeIDKey) == 0 {
		return fmt.Sprintf("I%x", sha1.Sum(msg))
	}
	mac := hmac.New(sha1.New, im.ChangeIDKey)
	mac.Write(msg)
	return fmt.Sprintf("I%x", mac.Sum(nil))
}

// pusherWait is how long after a PR opens its first import waits for
// GitHub to say who pushed its head.
const pusherWait = 10 * time.Minute

// maxPushes bounds the pushes pushedBy follows back.
const maxPushes = 300

// pushedBy is who pushed pr's head h, all of it but code in had: walking
// back from h through the pushes to the branch, as GitHub's activity log
// lists them (or, where it lags, webhooks said), each from the commit the
// one below moved it to, all by one GitHub account, to one whose code
// before it h has none of but had's (covered). Nobody (a zero User) if
// someone else pushed one of them (a commit the bridge never imported,
// which the next push would carry, say: seventh audit), or if a push is
// not known.
func (im *Importer) pushedBy(ctx context.Context, pr *github.PullRequest, h string, had ...string) (github.User, error) {
	repo := im.GitHub.Owner + "/" + im.GitHub.Repo
	if pr.Head.Repo != nil && pr.Head.Repo.FullName != "" {
		repo = pr.Head.Repo.FullName
	}
	ref := "refs/heads/" + pr.Head.Ref
	pushes, err := im.GitHub.Pushes(ctx, repo, ref)
	if err != nil {
		return github.User{}, fmt.Errorf("who pushed %s to #%d: %w", short(h), pr.Number, err)
	}
	var by github.User
	at, older := h, 0 // pushes[older:] came before the last one followed
	for range maxPushes {
		var p github.Push
		found := false
		for i := older; i < len(pushes) && !found; i++ {
			if pushes[i].After == at {
				p, older, found = pushes[i], i+1, true
			}
		}
		if !found && im.Pushers != nil && !slices.ContainsFunc(pushes[:older], func(p github.Push) bool { return p.After == at }) {
			// Not listed yet (a webhook said), unless it was listed
			// later than the push followed: then which came first?
			p, found = im.Pushers.Lookup(repo, ref, at)
		}
		if !found || p.Actor.Login == "" || (by.Login != "" && !sameGitHubUser(by, p.Actor)) {
			return github.User{}, nil
		}
		by = p.Actor
		if im.covered(ctx, h, p.Before, had...) {
			if im.carriesOthers(ctx, pushes, repo, ref, h, by, had...) {
				return github.User{}, nil
			}
			return by, nil
		}
		at = p.Before
	}
	return github.User{}, nil
}

// carriesOthers reports whether h has a commit beyond had that someone
// other than by pushed to the branch, as the activity log or webhooks
// say: theirs, though by's push brought it back. They pushed it, by
// fetched it, they reset the branch, and by pushed on top of it: the walk
// back from h never meets their push (eighth audit). If what h has is not
// known, it may.
func (im *Importer) carriesOthers(ctx context.Context, pushes []github.Push, repo, ref, h string, by github.User, had ...string) bool {
	if im.Pushers != nil {
		pushes = append(slices.Clone(pushes), im.Pushers.Pushes(repo, ref)...)
	}
	theirs := map[string]bool{}
	for _, p := range pushes {
		if !sameGitHubUser(p.Actor, by) {
			theirs[p.After] = true
		}
	}
	if len(theirs) == 0 {
		return false
	}
	const max = 10000
	args := []string{"rev-list", fmt.Sprintf("--max-count=%d", max+1), h, "--not"}
	for _, c := range had {
		if c != "" {
			args = append(args, c)
		}
	}
	out, err := im.Mirror.Git(ctx, nil, args...)
	commits := strings.Fields(string(out))
	if err != nil || len(commits) > max {
		return true
	}
	return slices.ContainsFunc(commits, func(c string) bool { return theirs[c] })
}

// covered reports whether a push from before leaves h none of the code
// the branch had then, but had's: the push made the branch, or before is
// one of had, or all h shares with before is in had. A force push replaces
// what was there: a fork's main, reused for a new PR, keeps nothing of
// whoever pushed to it for the last one. Not if before is not in the
// mirror: what h shares with it is not known.
func (im *Importer) covered(ctx context.Context, h, before string, had ...string) bool {
	if before == "" {
		return false
	}
	if made(before) || slices.Contains(had, before) {
		return true
	}
	// What two commits share is everything below their merge bases.
	out, err := im.Mirror.Git(ctx, nil, "merge-base", "--all", before, h)
	bases := strings.Fields(string(out))
	if err != nil || len(bases) == 0 {
		return false // before is unknown (or shares nothing, which is rare)
	}
	for _, m := range bases {
		if !slices.ContainsFunc(had, func(c string) bool {
			_, err := im.Mirror.Git(ctx, nil, "merge-base", "--is-ancestor", m, c)
			return c != "" && err == nil
		}) {
			return false
		}
	}
	return true
}

// made reports whether a push from before made its branch: everything in
// it was pushed then.
func made(before string) bool {
	return before != "" && strings.Trim(before, "0") == ""
}

// sameGitHubUser reports whether a and b are one GitHub account: by id,
// where both say it.
func sameGitHubUser(a, b github.User) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}
	return strings.EqualFold(a.Login, b.Login)
}

// have fetches Gerrit's ref, whose commit is commit, unless the mirror has
// it.
func (im *Importer) have(ctx context.Context, commit, ref string) error {
	if _, err := im.Mirror.Git(ctx, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		if _, err := im.Mirror.Git(ctx, nil, "fetch", "--quiet", "origin", ref); err != nil {
			return err
		}
	}
	return nil
}

// pureRebase reports whether h's tree is exactly what rebasing commit
// (Gerrit's ref) onto parent gives, with no conflicts: nothing in it is
// the pusher's own.
func (im *Importer) pureRebase(ctx context.Context, commit, ref, parent, h string) (bool, error) {
	if err := im.have(ctx, commit, ref); err != nil {
		return false, err
	}
	base, err := im.Mirror.Git(ctx, nil, "rev-parse", commit+"^")
	if err != nil {
		return false, err
	}
	// Exit status 1 is a conflict: not a pure rebase.
	merged, err := im.Mirror.Git(ctx, nil, "merge-tree", "--write-tree", "--merge-base="+strings.TrimSpace(string(base)), parent, commit)
	if err != nil {
		return false, nil
	}
	tree, err := im.Mirror.Git(ctx, nil, "rev-parse", h+"^{tree}")
	if err != nil {
		return false, err
	}
	first, _, _ := strings.Cut(string(merged), "\n")
	return strings.TrimSpace(first) == strings.TrimSpace(string(tree)), nil
}

// prBodyForMessage is the PR body without the bridge's links block and
// without Change-Id lines (the bridge adds its own).
func prBodyForMessage(body string) string {
	user, _ := splitBody(body)
	user = unGitHub(changeLinkLine.ReplaceAllString(changeIDLine.ReplaceAllString(user, ""), ""))
	user = signedOffBy.ReplaceAllString(user, "")
	// Nor the bridge's own trailer: a PR naming another would make its
	// change look like that PR's import.
	user = githubPRLine.ReplaceAllString(user, "")
	return strings.TrimSpace(user)
}

var githubPRLine = regexp.MustCompile(`(?m)^GitHub-PR:.*$`)

// signedOffBy matches DCO sign-off trailers. Projects like AOS require one
// (a Signed-off-by submit requirement), so a squash must keep them.
var signedOffBy = regexp.MustCompile(`(?m)^Signed-off-by: .+$`)

// signOffs collects the distinct Signed-off-by lines of texts, in order.
func signOffs(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range texts {
		for _, l := range signedOffBy.FindAllString(strings.ReplaceAll(t, "\r\n", "\n"), -1) {
			// A lone CR must not smuggle a second line (a Change-Id, say)
			// into the squash's footer.
			l = strings.TrimSpace(strings.ReplaceAll(l, "\r", " "))
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// noreplyEmail is u's GitHub noreply address, by account id: GitHub
// credits it to that account whatever its login becomes, where the
// id-less form follows whoever holds the login.
func noreplyEmail(u github.User) string {
	if u.ID == 0 {
		return u.Login + "@users.noreply.github.com"
	}
	return fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
}

// squash builds the deterministic commit for a layer (design §5.2), or
// reuses h itself when nothing needs rewriting.
//
// message, if set, is the commit message to use (a Gerrit-born change's);
// otherwise it is built from the PR.
func (im *Importer) squash(ctx context.Context, pr *github.PullRequest, own []string, h, parent, changeID, message string, committer gerrit.AccountInfo, author github.User, asCommitter bool) (string, error) {
	info, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%P%x00%an%x00%ae%x00%ad%x00%ce%x00%cd%x00%B", "--date=raw", h)
	if err != nil {
		return "", err
	}
	f := strings.SplitN(string(info), "\x00", 7)
	if len(f) != 7 {
		return "", fmt.Errorf("parsing commit %s", h)
	}
	// The PR's own commit is never uploaded as it is, even when it would
	// fit: Gerrit reads Change-Ids in more ways than the bridge checks
	// (Link footers, footer spelling), so the uploaded commit always
	// carries the bridge's own message, with exactly one Change-Id
	// (second audit, 2026-10-19).
	_, an, ae, ad, cd := f[0], f[1], f[2], f[3], f[5]
	if asCommitter || author.Login == "" {
		// Uploaded as a user, it is theirs: nobody may upload a commit
		// as someone else's (AOS gives Forge Author to few, and an email
		// GitHub has, a noreply one say, need not be theirs in Gerrit).
		// Others' sign-offs stay in the message. Uploaded as the bridge
		// with no one account that pushed all of it, it is the bridge's.
		an, ae = committer.Name, committer.Email
	} else {
		// Uploaded as the bridge, it is the GitHub account's that pushed
		// all of it (pushedBy), as GitHub knows them. A commit's author
		// is anyone's to write, and Gerrit lets only Forge Author upload
		// it, which the bridge has: kept, it could put a Gerrit user's
		// name on code the bridge uploaded for someone else, and the
		// bridge cannot tell whose an email is (Gerrit shows it only
		// preferred emails: fifth and sixth audits). Nor is the PR's
		// author, or a maintainer who clicked "Update branch", the author
		// of code someone else pushed: their noreply address may be
		// their Gerrit email, which would make their amend of it their
		// own commit to CI (seventh audit).
		an, ae = author.Login, noreplyEmail(author)
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
	if message != "" {
		// A later patch set: the change's message, plus the sign-offs of
		// whoever wrote the new commits (projects like AOS require them).
		msg.WriteString(strings.TrimSpace(message) + "\n")
		for _, so := range signOffs(texts[:len(texts)-1]...) {
			if !strings.Contains(message, so) {
				msg.WriteString(so + "\n")
			}
		}
	} else {
		msg.WriteString(strings.TrimSpace(pr.Title))
		if body := prBodyForMessage(pr.Body); body != "" {
			msg.WriteString("\n\n" + body)
		}
		msg.WriteString("\n\n")
		for _, so := range signOffs(texts...) {
			msg.WriteString(so + "\n")
		}
		fmt.Fprintf(&msg, "GitHub-PR: %s\nChange-Id: %s\n", pr.HTMLURL, changeID)
	}
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

// ownUpload reports whether change cur is one the bridge uploaded for pr,
// head h, whose link was never saved: it is open, no PR is linked to it,
// its first patch set carries pr's GitHub-PR footer, and whoever uploaded
// it is who the bridge would have uploaded it as (firstUploader). Nobody
// else can be made the owner of a PR's change this way: not even the
// PR's author, by uploading its Change-Id first when someone else pushed
// its head (fourth audit).
func (im *Importer) ownUpload(ctx context.Context, cur *gerrit.ChangeInfo, pr *github.PullRequest, h string) (bool, error) {
	if cur.Status != "NEW" {
		return false, nil
	}
	if st, err := im.Store.Load(ctx, cur.Number); err != nil || st.State.PR != 0 {
		return false, nil
	}
	for _, r := range cur.Revisions {
		if r.Number != 1 {
			continue
		}
		if r.RealUploader != nil || prFooter(r.Commit.Message) != pr.HTMLURL {
			return false, nil
		}
		if r.Uploader.AccountID == im.Bridge.AccountID {
			return true, nil
		}
		user, _, err := im.firstUploader(ctx, pr, h, r.Commit.Parent(), "")
		if err != nil {
			return false, err
		}
		return user != "" && strings.EqualFold(r.Uploader.Username, user) && !r.Uploader.Inactive, nil
	}
	return false, nil
}

// recordGerritHead records that GitHub has patch set ps of change n as
// Gerrit's own commit.
func (im *Importer) recordGerritHead(ctx context.Context, n, ps int, sha string) error {
	st, err := im.Store.Load(ctx, n)
	if err != nil {
		return err
	}
	if knownHeads(st)[sha] != "" || im.DryRun {
		return nil
	}
	tree, parentTree := revisionTrees(ctx, im.Mirror, sha)
	return updateStateFn(ctx, im.Store, n, fmt.Sprintf("Gerrit's patch set %d is on GitHub", ps), func(st *state.Change) {
		st.Revisions = append(st.Revisions, state.Revision{PatchSet: ps, GerritSHA: sha, GitHubSHA: sha, Tree: tree, ParentTree: parentTree, At: time.Now()})
	})
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

// lacksSignOff reports whether commit's message has no Signed-off-by.
func (im *Importer) lacksSignOff(ctx context.Context, commit string) bool {
	m, err := im.Mirror.Git(ctx, nil, "log", "-1", "--format=%B", commit)
	return err == nil && !signedOffBy.Match(m)
}
