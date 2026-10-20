package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

// sites is an in-memory Gerrit and GitHub behind one HTTP server, for the
// parts of the bridge that use the real clients (Code and Importer), with
// real git repositories for each side:
//
//	gerrit.git  Gerrit's repository (the mirror's origin)
//	github.git  GitHub's repository (Code.GitHubURL)
//	work        a work tree that makes commits and pushes to both
type sites struct {
	t   *testing.T
	srv *httptest.Server
	dir string

	mu sync.Mutex
	// Gerrit
	changes map[int]*gerrit.ChangeInfo
	// may lists the Gerrit users who may abandon, restore, edit the commit
	// message of and toggle WIP on each change, as Gerrit's permissions
	// would say.
	may      map[int][]string
	accounts map[string]gerrit.AccountInfo
	together map[int][]int // change -> submitted together
	// GitHub
	pulls    map[int]*github.PullRequest
	events   map[int][]github.IssueEvent
	comments map[int][]string
	statuses map[string][]github.Status // sha -> statuses
	// pushLog is GitHub's activity log per "owner/repo refs/heads/x",
	// oldest first.
	pushLog  map[string][]pushEntry
	unstacks []int
	stacks   []*github.Stack           // made with POST /stacks, as GitHub would
	activity map[int]github.PRActivity // what Activity returns per PR
	// onActivity, if set, runs (under s.mu) when Activity is asked.
	onActivity func()
	nextID     int64
	clock      time.Time
	calls      []string // Gerrit edits made, as "<what> <change> as <user>"
	// refuseReview makes reviews of a change fail with that status.
	refuseReview map[int]int
	// refusePulls makes edits of these PRs fail as GitHub's validation.
	refusePulls map[int]bool
}

func newSites(t *testing.T) *sites {
	t.Helper()
	s := &sites{t: t, dir: t.TempDir(),
		changes: map[int]*gerrit.ChangeInfo{}, may: map[int][]string{}, activity: map[int]github.PRActivity{}, accounts: map[string]gerrit.AccountInfo{},
		together: map[int][]int{}, pulls: map[int]*github.PullRequest{}, events: map[int][]github.IssueEvent{},
		comments: map[int][]string{}, statuses: map[string][]github.Status{},
		clock: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}
	for _, r := range []string{"gerrit.git", "github.git"} {
		s.git("", "init", "-q", "--bare", "-b", "main", filepath.Join(s.dir, r))
	}
	s.git("", "init", "-q", "-b", "main", filepath.Join(s.dir, "work"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/changes/{n}", s.getChange)
	mux.HandleFunc("GET /a/changes/", s.queryChanges)
	mux.HandleFunc("POST /a/changes/{n}/{action}", s.changeAction)
	mux.HandleFunc("POST /a/changes/{n}/revisions/{rev}/review", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, "review "+r.PathValue("n"))
		status := s.refuseReview[num(r)]
		s.mu.Unlock()
		if status != 0 {
			http.Error(w, "refused", status)
			return
		}
		s.json(w, map[string]any{}, true)
	})
	mux.HandleFunc("PUT /a/changes/{n}/message", s.setMessage)
	mux.HandleFunc("GET /a/projects/{p}/branches/{b}", s.branch)
	mux.HandleFunc("POST /graphql", s.graphql)
	mux.HandleFunc("GET /a/changes/{n}/submitted_together", s.submittedTogether)
	mux.HandleFunc("GET /a/accounts/{user}", s.account)
	mux.HandleFunc("GET /repos/o/r/pulls", s.listPulls)
	mux.HandleFunc("GET /repos/{owner}/{repo}/activity", s.activityLog)
	mux.HandleFunc("GET /repos/o/r/pulls/{n}", s.getPull)
	mux.HandleFunc("PATCH /repos/o/r/pulls/{n}", s.updatePull)
	mux.HandleFunc("GET /repos/o/r/issues/{n}/events", s.issueEvents)
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", s.createComment)
	mux.HandleFunc("POST /repos/o/r/statuses/{sha}", s.createStatus)
	mux.HandleFunc("POST /repos/o/r/stacks/{n}/unstack", s.unstack)
	mux.HandleFunc("GET /repos/o/r/stacks", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.json(w, append([]*github.Stack{}, s.stacks...), false)
	})
	mux.HandleFunc("POST /repos/o/r/stacks", s.createStack)
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in github.NewPull
		json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 100 + len(s.pulls)
		pr := &github.PullRequest{Number: n, NodeID: fmt.Sprintf("PR_%d", n), State: "open", Title: in.Title, Body: in.Body, Draft: in.Draft,
			User: github.User{Login: "bot"}, HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), CreatedAt: s.clock,
			Head: github.Ref{Ref: in.Head, SHA: strings.TrimSpace(s.gitOut("github.git", "rev-parse", "refs/heads/"+in.Head)), Repo: &struct {
				FullName string `json:"full_name"`
			}{"o/r"}},
			Base: github.Ref{Ref: in.Base}}
		s.pulls[n] = pr
		s.json(w, pr, false)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake sites: unexpected %s %s", r.Method, r.URL)
		http.Error(w, "not faked", http.StatusNotImplemented)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// git runs git in dir (relative to the sites' directory; "" = there).
func (s *sites) git(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = filepath.Join(s.dir, dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Austin", "GIT_AUTHOR_EMAIL=austin@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=Austin", "GIT_COMMITTER_EMAIL=austin@example.com", "GIT_COMMITTER_DATE=1700000000 +0000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitOut runs git in dir without failing the test (for handlers).
func (s *sites) gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = filepath.Join(s.dir, dir)
	out, _ := cmd.Output()
	return string(out)
}

// commit commits file in the work tree on top of parent ("" = the current
// HEAD) and returns the commit.
func (s *sites) commit(parent, file, content, msg string) string {
	s.t.Helper()
	if parent != "" {
		s.git("work", "checkout", "-q", "--detach", parent)
	}
	os.WriteFile(filepath.Join(s.dir, "work", file), []byte(content), 0o644)
	s.git("work", "add", file)
	s.git("work", "commit", "-q", "-m", msg)
	return s.git("work", "rev-parse", "HEAD")
}

// push force-pushes commit to ref in gerrit.git or github.git.
func (s *sites) push(repo, commit, ref string) {
	s.t.Helper()
	s.git("work", "push", "-q", "-f", filepath.Join(s.dir, repo), commit+":"+ref)
}

func (s *sites) ref(repo, ref string) string {
	s.t.Helper()
	return s.git(repo, "rev-parse", ref)
}

func (s *sites) mirror(ctx context.Context) *gitmirror.Repo {
	s.t.Helper()
	m := &gitmirror.Repo{Dir: filepath.Join(s.dir, "m.git"), Remote: filepath.Join(s.dir, "gerrit.git"), Name: "Bridge", Email: "bridge@example.com"}
	if err := m.Init(ctx); err != nil {
		s.t.Fatal(err)
	}
	return m
}

func (s *sites) gerritClient() *gerrit.Client {
	return &gerrit.Client{BaseURL: s.srv.URL, User: "bridge", Password: "x"}
}

func (s *sites) githubClient() *github.Client {
	return &github.Client{BaseURL: s.srv.URL, Owner: "o", Repo: "r", Tokens: github.StaticToken("t")}
}

// change adds change n at patch set ps (commit rev) with Change-Id cid.
func (s *sites) change(n int, cid, rev, msg, status string) *gerrit.ChangeInfo {
	ci := &gerrit.ChangeInfo{Number: n, ChangeID: cid, Project: "p", Branch: "main", Status: status, CurrentRevision: rev,
		Revisions: map[string]gerrit.RevisionInfo{rev: {Number: 1, Ref: fmt.Sprintf("refs/changes/%02d/%d/1", n%100, n),
			Commit: gerrit.CommitInfo{Message: msg}}}}
	s.changes[n] = ci
	s.push("gerrit.git", rev, ci.Revisions[rev].Ref)
	return ci
}

func (s *sites) pr(n int, head, sha, base, author string) *github.PullRequest {
	pr := &github.PullRequest{Number: n, NodeID: fmt.Sprintf("PR_%d", n), State: "open", Title: "PR", User: github.User{Login: author},
		HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), CreatedAt: s.clock,
		Head: github.Ref{Ref: head, SHA: sha, Repo: &struct {
			FullName string `json:"full_name"`
		}{"o/r"}},
		Base: github.Ref{Ref: base}}
	s.pulls[n] = pr
	return pr
}

// createStack makes a stack of the PRs named, refusing a fork's PR as
// GitHub does.
func (s *sites) createStack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PullRequests []int `json:"pull_requests"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &github.Stack{Number: len(s.stacks) + 1, Open: true}
	for _, n := range in.PullRequests {
		if pr := s.pulls[n]; pr != nil && pr.Head.Repo != nil && pr.Head.Repo.FullName != "o/r" {
			http.Error(w, `{"message":"Pull requests from forks cannot be added to stacks."}`, http.StatusUnprocessableEntity)
			return
		}
		st.PullRequests = append(st.PullRequests, github.StackPR{Number: n, State: "open"})
	}
	s.stacks = append(s.stacks, st)
	s.json(w, st, false)
}

// webhookPush records, as a webhook would, that login pushed PR n's head
// to its branch.
func (s *sites) webhookPush(p *Pushers, n int, login string) {
	pr := s.pulls[n]
	p.Record(pr.Head.Repo.FullName, "refs/heads/"+pr.Head.Ref, pr.Head.SHA, login)
}

// ghEvent records that login closed or reopened PR n, a minute after the
// last thing that happened.
func (s *sites) ghEvent(n int, event, login string) {
	s.clock = s.clock.Add(time.Minute)
	s.events[n] = append(s.events[n], github.IssueEvent{Event: event, Actor: github.User{Login: login}, CreatedAt: s.clock})
}

// gerritStatus changes change n's status as Gerrit would, with the message
// abandon/restore leaves.
func (s *sites) gerritStatus(n int, status, author string) {
	s.clock = s.clock.Add(time.Minute)
	tag := map[string]string{"ABANDONED": "autogenerated:gerrit:abandon", "NEW": "autogenerated:gerrit:restore"}[status]
	ci := s.changes[n]
	ci.Status = status
	ci.Messages = append(ci.Messages, gerrit.ChangeMessage{ID: fmt.Sprintf("m%d", len(ci.Messages)+1), Tag: tag,
		Author: gerrit.AccountInfo{Username: author}, Date: s.clock.Format("2006-01-02 15:04:05.000000000")})
}

// branch answers whether gerrit.git has the branch.
func (s *sites) branch(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", r.PathValue("b"))
	cmd.Dir = filepath.Join(s.dir, "gerrit.git")
	if cmd.Run() != nil {
		http.NotFound(w, r)
		return
	}
	s.json(w, map[string]string{"ref": r.PathValue("b")}, true)
}

func (s *sites) json(w http.ResponseWriter, v any, xssi bool) {
	w.Header().Set("Content-Type", "application/json")
	if xssi {
		w.Write([]byte(")]}'\n"))
	}
	json.NewEncoder(w).Encode(v)
}

func num(r *http.Request) int {
	n, _ := strconv.Atoi(r.PathValue("n"))
	return n
}

func (s *sites) getChange(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ci := s.changes[num(r)]
	if ci == nil {
		http.NotFound(w, r)
		return
	}
	s.json(w, ci, true)
}

// queryChanges answers "... change:<Change-Id>" and "... status:open".
func (s *sites) queryChanges(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query().Get("q")
	out := []*gerrit.ChangeInfo{}
	for _, n := range sortedKeys(s.changes) {
		ci := s.changes[n]
		_, hasCommit := ci.Revisions[strings.TrimPrefix(q[strings.LastIndex(q, " ")+1:], "commit:")]
		if strings.Contains(q, "change:"+ci.ChangeID) || (strings.Contains(q, "status:open") && ci.Status == "NEW") ||
			(strings.Contains(q, "commit:") && hasCommit) {
			out = append(out, ci)
		}
	}
	s.json(w, out, true)
}

// changeAction abandons or restores a change, as the Run-As user if any,
// if mayAbandon lets them.
func (s *sites) changeAction(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, action := num(r), r.PathValue("action")
	user := r.Header.Get("X-Gerrit-RunAs")
	if user == "" {
		user, _, _ = r.BasicAuth()
	}
	ci := s.changes[n]
	switch {
	case ci == nil:
		http.NotFound(w, r)
	case !strings.Contains(" "+strings.Join(s.may[n], " ")+" ", " "+user+" "):
		http.Error(w, action+" not permitted", http.StatusForbidden)
	case action == "abandon" || action == "restore":
		s.gerritStatus(n, map[string]string{"abandon": "ABANDONED", "restore": "NEW"}[action], user)
		s.json(w, ci, true)
	case action == "wip" || action == "ready":
		ci.WorkInProgress = action == "wip"
		s.calls = append(s.calls, fmt.Sprintf("%s %d as %s", action, n, user))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not faked: "+action, http.StatusNotImplemented)
	}
}

// setMessage edits change n's commit message (a new patch set, same tree)
// as the Run-As user, if may lets them.
func (s *sites) setMessage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := num(r)
	user := r.Header.Get("X-Gerrit-RunAs")
	if user == "" {
		user, _, _ = r.BasicAuth()
	}
	if !strings.Contains(" "+strings.Join(s.may[n], " ")+" ", " "+user+" ") {
		http.Error(w, "edit not permitted", http.StatusForbidden)
		return
	}
	var in struct{ Message string }
	json.NewDecoder(r.Body).Decode(&in)
	ci := s.changes[n]
	sha := fmt.Sprintf("msg%d", len(ci.Revisions)+1)
	ci.Revisions[sha] = gerrit.RevisionInfo{Number: len(ci.Revisions) + 1, Commit: gerrit.CommitInfo{Message: in.Message}}
	ci.CurrentRevision = sha
	s.calls = append(s.calls, fmt.Sprintf("message %d as %s", n, user))
	w.WriteHeader(http.StatusNoContent)
}

// graphql answers Activity's query.
func (s *sites) graphql(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in struct {
		Variables struct{ N int }
	}
	json.NewDecoder(r.Body).Decode(&in)
	a := s.activity[in.Variables.N]
	if s.onActivity != nil {
		s.onActivity()
	}
	type actor struct {
		Login string `json:"login"`
	}
	var nodes []map[string]any
	pr := map[string]any{"timelineItems": map[string]any{"nodes": &nodes}}
	if a.BodyEditor != "" {
		pr["lastEditedAt"], pr["editor"] = s.clock, actor{a.BodyEditor}
	}
	if a.TitleEditor != "" {
		nodes = append(nodes, map[string]any{"__typename": "RenamedTitleEvent", "createdAt": s.clock, "actor": actor{a.TitleEditor}})
	}
	if a.DraftActor != "" {
		nodes = append(nodes, map[string]any{"__typename": "ConvertToDraftEvent", "createdAt": s.clock, "actor": actor{a.DraftActor}})
	}
	s.json(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}}, false)
}

func (s *sites) submittedTogether(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*gerrit.ChangeInfo{}
	for _, n := range s.together[num(r)] {
		out = append(out, s.changes[n])
	}
	s.json(w, out, true)
}

func (s *sites) account(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[r.PathValue("user")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.json(w, a, true)
}

type pushEntry struct{ sha, actor, kind string }

// pushed logs that actor pushed sha to ref of repo, as GitHub's activity
// log would.
func (s *sites) pushed(repo, ref, sha, actor string) {
	if s.pushLog == nil {
		s.pushLog = map[string][]pushEntry{}
	}
	s.pushLog[repo+" "+ref] = append(s.pushLog[repo+" "+ref], pushEntry{sha, actor, "push"})
}

func (s *sites) activityLog(w http.ResponseWriter, r *http.Request) {
	repo, ref := r.PathValue("owner")+"/"+r.PathValue("repo"), r.URL.Query().Get("ref")
	s.mu.Lock()
	log := s.pushLog[repo+" "+ref]
	var out []map[string]any
	for i := len(log) - 1; i >= 0; i-- { // newest first
		out = append(out, map[string]any{"ref": ref, "after": log[i].sha, "activity_type": log[i].kind,
			"actor": map[string]string{"login": log[i].actor}})
	}
	s.mu.Unlock()
	s.json(w, out, false)
}

func (s *sites) listPulls(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*github.PullRequest{}
	for _, n := range sortedKeys(s.pulls) {
		if pr := s.pulls[n]; r.URL.Query().Get("state") == pr.State {
			out = append(out, pr)
		}
	}
	s.json(w, out, false)
}

func (s *sites) getPull(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.pulls[num(r)]
	if pr == nil {
		http.NotFound(w, r)
		return
	}
	s.json(w, pr, false)
}

func (s *sites) updatePull(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.pulls[num(r)]
	s.calls = append(s.calls, fmt.Sprintf("update #%d", num(r)))
	if s.refusePulls[num(r)] {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	var u github.PullUpdate
	json.NewDecoder(r.Body).Decode(&u)
	if u.State != nil {
		pr.State = *u.State
	}
	if u.Base != nil {
		pr.Base.Ref = *u.Base
	}
	if u.Title != nil {
		pr.Title = *u.Title
	}
	if u.Body != nil {
		pr.Body = *u.Body
	}
	s.json(w, pr, false)
}

func (s *sites) issueEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.json(w, append([]github.IssueEvent{}, s.events[num(r)]...), false)
}

func (s *sites) createComment(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in struct{ Body string }
	json.NewDecoder(r.Body).Decode(&in)
	n := num(r)
	s.comments[n] = append(s.comments[n], in.Body)
	s.nextID++
	s.json(w, github.IssueComment{ID: s.nextID, Body: in.Body}, false)
}

func (s *sites) createStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st github.Status
	json.NewDecoder(r.Body).Decode(&st)
	s.statuses[r.PathValue("sha")] = append(s.statuses[r.PathValue("sha")], st)
	s.json(w, st, false)
}

func (s *sites) unstack(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unstacks = append(s.unstacks, num(r))
	for _, pr := range s.pulls {
		if pr.Stack != nil && pr.Stack.Number == num(r) {
			pr.Stack = nil
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// code is a Code against the sites, with change state in store.
func (s *sites) code(ctx context.Context) (*Code, *state.Store) {
	s.t.Helper()
	m := s.mirror(ctx)
	store := &state.Store{Repo: m}
	return &Code{Project: "p", Gerrit: s.gerritClient(), GitHub: s.githubClient(), GitHubURL: filepath.Join(s.dir, "github.git"),
		Mirror: m, Store: store, DefaultBranch: "main", Logger: slog.Default(), BridgeLogin: "bot",
		Identity: &Users{Overrides: map[string]string{"austin": "austin", "brt": "brt"}}}, store
}

func (s *sites) save(ctx context.Context, store *state.Store, n int, st *state.Change) {
	s.t.Helper()
	if err := store.Save(ctx, n, st, "test"); err != nil {
		s.t.Fatal(err)
	}
}
