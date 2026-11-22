package gerritchecks

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Live keeps the GitHub checks of the commits viewed in Gerrit's Checks
// tab current from GitHub's webhooks (check_run, workflow_job, status),
// so the tab is answered from memory rather than by asking GitHub on
// every poll.
//
// It is a cache of what GitHub would say, never of who may see it
// (Gerrit is asked as the viewer on every request). Only commits someone
// has viewed are kept, and each commit's checks are read from GitHub
// again after liveMaxAge, so a missed delivery (the bridge down, GitHub
// dropping it) corrects itself.
//
// A kind of delivery that has not arrived since the bridge started may
// not be subscribed to: until it does, that part is read from GitHub on
// each request, as without Live. Check runs need both check_run (they
// start and finish) and workflow_job (GitHub sends no check_run event
// when one starts running; workflow_job says so for GitHub Actions').
type Live struct {
	mu      sync.Mutex
	commits map[string]*liveCommit
	// seq counts changes: each item and commit records the one that last
	// changed it, so a read from GitHub that started before a delivery
	// does not undo it, and an answer built before one is not reused.
	seq   uint64
	bytes int // of text kept, all commits
	// Which kinds of delivery have arrived since the bridge started.
	sawCheckRun, sawWorkflowJob, sawStatus bool
	now                                    func() time.Time // tests
}

type liveCommit struct {
	runs     map[int64]liveRun
	statuses map[string]liveStatus // by context
	// runsAt and statusesAt are when each part was last read from
	// GitHub (zero: never).
	runsAt, statusesAt time.Time
	used               time.Time
	seq                uint64
	bytes              int // of text kept
	// born is the change count it was kept from: a read that began
	// before then may have missed a delivery (dropped, nobody viewing).
	born uint64
}

type liveRun struct {
	run github.CheckRun
	seq uint64
}

type liveStatus struct {
	status github.Status
	seq    uint64
}

const (
	// liveMaxAge is how long a commit's checks are taken from deliveries
	// alone before GitHub is read again.
	liveMaxAge = 10 * time.Minute
	// maxLiveCommits bounds the commits kept (the least recently viewed
	// go first), and maxLiveRuns and maxLiveStatuses each one's checks.
	maxLiveCommits  = 2000
	maxLiveRuns     = 100 // as many as a read returns
	maxLiveStatuses = 100
	// maxLiveBytes bounds the text kept for all commits (anyone whose
	// workflow may create check runs writes it): past it, the least
	// recently viewed commits go first. Each run's and status's text is
	// clipped as the tab shows it, and URLs longer than maxURL dropped.
	maxLiveBytes = 64 << 20
	maxURL       = 2048
)

func (l *Live) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// trimRun is run as kept: its text clipped as the tab shows it, an
// overlong URL dropped.
func trimRun(r github.CheckRun) github.CheckRun {
	r.Name, r.App.Name, r.App.Slug = oneLine(r.Name), oneLine(r.App.Name), oneLine(r.App.Slug)
	r.Status, r.Conclusion = oneLine(r.Status), oneLine(r.Conclusion)
	r.Output.Title, r.Output.Summary = oneLine(r.Output.Title), clip(strings.TrimSpace(r.Output.Summary))
	r.HTMLURL, r.DetailsURL = shortURL(r.HTMLURL), shortURL(r.DetailsURL)
	return r
}

// trimStatus is s as kept (trimRun).
func trimStatus(s github.Status) github.Status {
	s.Context, s.State, s.Description = oneLine(s.Context), oneLine(s.State), oneLine(s.Description)
	s.TargetURL = shortURL(s.TargetURL)
	return s
}

func shortURL(u string) string {
	if len(u) > maxURL {
		return ""
	}
	return u
}

func runBytes(r github.CheckRun) int {
	return 64 + len(r.Name) + len(r.App.Name) + len(r.App.Slug) + len(r.Status) + len(r.Conclusion) +
		len(r.Output.Title) + len(r.Output.Summary) + len(r.HTMLURL) + len(r.DetailsURL)
}

func statusBytes(s github.Status) int {
	return 64 + len(s.Context) + len(s.State) + len(s.Description) + len(s.TargetURL)
}

// resize recounts c's text, and drops the least recently viewed other
// commits while all are more than maxLiveBytes. l.mu is held.
func (l *Live) resize(c *liveCommit) {
	n := 0
	for _, r := range c.runs {
		n += runBytes(r.run)
	}
	for _, s := range c.statuses {
		n += statusBytes(s.status)
	}
	l.bytes += n - c.bytes
	c.bytes = n
	for l.bytes > maxLiveBytes && len(l.commits) > 1 {
		l.evict(c)
	}
}

// rank orders a check run's states: a delivery never takes one back
// (GitHub does not promise to deliver in order).
func rank(status string) int {
	switch status {
	case "completed":
		return 3
	case "in_progress":
		return 2
	}
	return 1
}

// Deliver takes a verified webhook delivery for the repository. It does
// not block on anything but its own lock.
func (l *Live) Deliver(d *github.Delivery) {
	switch d.Event {
	case "check_run":
		var p struct {
			CheckRun struct {
				github.CheckRun
				HeadSHA string `json:"head_sha"`
			} `json:"check_run"`
		}
		if json.Unmarshal(d.Payload, &p) != nil || p.CheckRun.ID == 0 {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.sawCheckRun = true
		l.putRun(p.CheckRun.HeadSHA, trimRun(p.CheckRun.CheckRun), false)
	case "workflow_job":
		var p struct {
			Job struct {
				HeadSHA     string     `json:"head_sha"`
				CheckRunURL string     `json:"check_run_url"`
				Name        string     `json:"name"`
				Status      string     `json:"status"`
				Conclusion  string     `json:"conclusion"`
				HTMLURL     string     `json:"html_url"`
				StartedAt   *time.Time `json:"started_at"`
				CompletedAt *time.Time `json:"completed_at"`
			} `json:"workflow_job"`
		}
		if json.Unmarshal(d.Payload, &p) != nil {
			return
		}
		// The job is a check run: the one its check_run_url names.
		id, err := strconv.ParseInt(path.Base(p.Job.CheckRunURL), 10, 64)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.sawWorkflowJob = true
		if err != nil || id <= 0 {
			return
		}
		run := github.CheckRun{ID: id, Name: p.Job.Name, Status: p.Job.Status, Conclusion: p.Job.Conclusion,
			HTMLURL: p.Job.HTMLURL, StartedAt: p.Job.StartedAt, CompletedAt: p.Job.CompletedAt}
		run.App.Name, run.App.Slug = "GitHub Actions", actionsApp
		l.putRun(p.Job.HeadSHA, trimRun(run), true)
	case "status":
		var p struct {
			ID        int64      `json:"id"`
			SHA       string     `json:"sha"`
			Context   string     `json:"context"`
			State     string     `json:"state"`
			Desc      string     `json:"description"`
			TargetURL string     `json:"target_url"`
			UpdatedAt *time.Time `json:"updated_at"`
		}
		if json.Unmarshal(d.Payload, &p) != nil || p.Context == "" {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.sawStatus = true
		c := l.commits[p.SHA]
		if c == nil {
			return // nobody is looking at it: the first view reads GitHub
		}
		st := trimStatus(github.Status{ID: p.ID, State: p.State, TargetURL: p.TargetURL, Description: p.Desc, Context: p.Context, UpdatedAt: p.UpdatedAt})
		if old, ok := c.statuses[st.Context]; ok && !newer(st, old.status, true) {
			return // an older delivery, late
		}
		if _, ok := c.statuses[st.Context]; !ok && len(c.statuses) >= maxLiveStatuses {
			return
		}
		l.seq++
		c.statuses[st.Context] = liveStatus{st, l.seq}
		c.seq = l.seq
		l.resize(c)
	}
}

// putRun records a delivered check run of commit sha, if someone is
// looking at it. A job's delivery (job) says less than a check run's:
// what the commit's check run already says that it does not is kept.
// l.mu is held.
func (l *Live) putRun(sha string, run github.CheckRun, job bool) {
	c := l.commits[sha]
	if c == nil {
		return // nobody is looking at it: the first view reads GitHub
	}
	old, ok := c.runs[run.ID]
	if ok && rank(run.Status) < rank(old.run.Status) {
		return // an older delivery, late
	}
	if !ok && len(c.runs) >= maxLiveRuns {
		return
	}
	if ok && job {
		merged := old.run
		merged.Status, merged.Conclusion, merged.StartedAt, merged.CompletedAt = run.Status, run.Conclusion, run.StartedAt, run.CompletedAt
		if merged.HTMLURL == "" {
			merged.HTMLURL = run.HTMLURL
		}
		run = merged
	}
	l.seq++
	c.runs[run.ID] = liveRun{run, l.seq}
	c.seq = l.seq
	l.resize(c)
}

// liveView is what Live has of a commit.
type liveView struct {
	runs     []github.CheckRun
	statuses []github.Status
	// haveRuns and haveStatuses say each part is current: kept so by
	// deliveries, and read from GitHub within liveMaxAge.
	haveRuns, haveStatuses bool
	seq                    uint64
}

// get is what l has of commit, and the change count to read GitHub from
// (begin, for merge). A nil Live has nothing.
func (l *Live) get(commit string) (v liveView, begin uint64) {
	if l == nil {
		return v, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Kept from now on, before GitHub is read: a delivery that arrives
	// during the read is then not lost.
	c := l.commit(commit)
	now := l.clock()
	c.used = now
	v = c.view()
	v.haveRuns = l.sawCheckRun && l.sawWorkflowJob && !c.runsAt.IsZero() && now.Sub(c.runsAt) < liveMaxAge && c.followed()
	v.haveStatuses = l.sawStatus && !c.statusesAt.IsZero() && now.Sub(c.statusesAt) < liveMaxAge
	return v, l.seq
}

// actionsApp is GitHub Actions' app: its jobs' starts are delivered
// (workflow_job).
const actionsApp = "github-actions"

// followed says deliveries tell every change of c's check runs still to
// come: each is finished or GitHub Actions'. Another app's run starting
// is delivered to nobody (check_run has no such event for a repository
// webhook), so while one has not finished, the runs are read.
func (c *liveCommit) followed() bool {
	for _, r := range c.runs {
		if r.run.Status != "completed" && r.run.App.Slug != actionsApp {
			return false
		}
	}
	return true
}

func (c *liveCommit) view() liveView {
	v := liveView{seq: c.seq}
	for _, r := range c.runs {
		v.runs = append(v.runs, r.run)
	}
	for _, s := range c.statuses {
		v.statuses = append(v.statuses, s.status)
	}
	sort.Slice(v.statuses, func(i, j int) bool { return v.statuses[i].Context < v.statuses[j].Context })
	return v
}

// newer says status a was set after b, for the same context: by
// GitHub's ID, which grows (updated_at is to the second: a quick
// check's pending and success share one); without IDs, by time; if
// neither tells, unknown.
func newer(a, b github.Status, unknown bool) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID > b.ID
	}
	if a.UpdatedAt != nil && b.UpdatedAt != nil && !a.UpdatedAt.Equal(*b.UpdatedAt) {
		return a.UpdatedAt.After(*b.UpdatedAt)
	}
	return unknown
}

// merge records what GitHub said of commit (runs and statuses, each nil
// if not read) in a read that began at change count begin, and returns
// all l has of it. An item a delivery changed since begin is kept as the
// delivery said. A nil Live keeps nothing.
func (l *Live) merge(commit string, begin uint64, runs []github.CheckRun, readRuns bool, statuses []github.Status, readStatuses bool) liveView {
	if l == nil {
		return liveView{runs: runs, statuses: statuses}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	c := l.commit(commit)
	c.used = now
	// A commit dropped and kept again since the read began may have
	// missed deliveries in between: what was read is kept, but it is
	// read again next time.
	confirm := c.born <= begin
	l.seq++
	if readRuns {
		next := map[int64]liveRun{}
		for id, r := range c.runs {
			if r.seq > begin {
				next[id] = r
			}
		}
		for _, r := range runs {
			r = trimRun(r)
			// A delivery since begin wins, unless it is older news
			// than the read (late, out of order).
			if d, newer := next[r.ID]; (!newer && len(next) < maxLiveRuns) || (newer && rank(r.Status) > rank(d.run.Status)) {
				next[r.ID] = liveRun{r, l.seq}
			}
		}
		c.runs = next
		if confirm {
			c.runsAt = now
		}
	}
	if readStatuses {
		next := map[string]liveStatus{}
		for ctx, s := range c.statuses {
			if s.seq > begin {
				next[ctx] = s
			}
		}
		for _, s := range statuses {
			s = trimStatus(s)
			if d, found := next[s.Context]; (!found && len(next) < maxLiveStatuses) || (found && newer(s, d.status, false)) {
				next[s.Context] = liveStatus{s, l.seq}
			}
		}
		c.statuses = next
		if confirm {
			c.statusesAt = now
		}
	}
	c.seq = l.seq
	l.resize(c)
	return c.view()
}

// commit is what l keeps of commit, kept from now on. l.mu is held.
func (l *Live) commit(commit string) *liveCommit {
	c := l.commits[commit]
	if c == nil {
		if l.commits == nil {
			l.commits = map[string]*liveCommit{}
		}
		if len(l.commits) >= maxLiveCommits {
			l.evict(nil)
		}
		l.seq++
		c = &liveCommit{runs: map[int64]liveRun{}, statuses: map[string]liveStatus{}, used: l.clock(), born: l.seq, seq: l.seq}
		l.commits[commit] = c
	}
	return c
}

// evict drops the least recently viewed commit but keep. l.mu is held.
func (l *Live) evict(keep *liveCommit) {
	var oldest string
	var at time.Time
	for sha, c := range l.commits {
		if c != keep && (oldest == "" || c.used.Before(at)) {
			oldest, at = sha, c.used
		}
	}
	if c := l.commits[oldest]; c != nil {
		l.bytes -= c.bytes
		delete(l.commits, oldest)
	}
}

// changed says whether commit has changed since seq, when an answer was
// built from what l had of it at seq: if so, the answer is not reused.
func (l *Live) changed(commit string, seq uint64) bool {
	if l == nil || commit == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.commits[commit]
	return c == nil || c.seq != seq
}

// SawStatus says a status delivery has arrived (tests).
func (l *Live) SawStatus() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sawStatus
}
