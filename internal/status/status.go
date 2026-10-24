// Package status serves a page of what the bridge is doing: its work
// queue (running, waiting, held, failing, recently done) and a few facts
// (the last Gerrit event, GitHub's rate budget). For operators only: the
// queue would tell an attacker when the bridge is starved, so it is
// served on its own loopback listener (config status_listen).
package status

import (
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/trace"
)

// Fact is one line of the page's summary; Link, if set, is where its
// details are.
type Fact struct{ Name, Value, Link string }

// Handler serves the page.
type Handler[K comparable] struct {
	Queue interface{ Snapshot() queue.Snapshot[K] }
	// Facts, if set, are shown above the queue.
	Facts func() []Fact
	// Name and Link say what a key is, and where to see it (a PR, a
	// change; "" for none).
	Name func(K) string
	Link func(K) string
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Resyncs, if set, says what resyncs caught: runs only a resync
	// asked for, and those that changed something.
	Resyncs func() Resyncs[K]
}

// Resyncs is what resyncs caught since Since.
type Resyncs[K comparable] struct {
	Since  time.Time
	Counts []ResyncCount
	Caught []Caught[K] // newest first
}

// ResyncCount is the runs of one kind of key for one reason.
type ResyncCount struct {
	Reason, Key          string
	Runs, Caught, Failed int
}

// Caught is a run a resync asked for that wrote.
type Caught[K comparable] struct {
	Key    K
	At     time.Time
	Reason string
	Writes []string
	N      int // writes in all
}

// row is an Entry as the page shows it.
type row struct {
	Key, Link, Ago, Waited, Took, Note, Err string
	Failures                                int
	// Run links its timeline ("" if it has none).
	Run string
}

type page struct {
	Now                               string
	Facts                             []Fact
	Running, Waiting, Failing, Recent []row
}

type resyncs struct {
	Now, Since string
	Counts     []resyncCount
	Caught     []caughtRow
}

type resyncCount struct {
	ResyncCount
	Share string // of runs that caught something
}

type caughtRow struct {
	Key, Link, Ago, Reason string
	Writes                 []string
	More                   int // writes not named
}

func (h *Handler[K]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	// Only a page asked for by a loopback name: a site that rebinds its
	// own name to 127.0.0.1 sends that name (ninth audit).
	if !loopbackHost(r.Host) {
		http.Error(w, "the status page answers only to 127.0.0.1, [::1] or localhost", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/run/"); ok {
		h.serveRun(w, id)
		return
	}
	if r.URL.Path == "/resyncs" && h.Resyncs != nil {
		h.serveResyncs(w)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	s := h.Queue.Snapshot()
	rows := func(es []queue.Entry[K]) []row {
		var out []row
		for _, e := range es {
			r := row{Key: h.Name(e.Key), Failures: e.Failures, Err: e.Err}
			if _, ok := e.Detail.(*trace.Trace); ok && e.ID != 0 {
				r.Run = "run/" + strconv.FormatUint(e.ID, 10)
			}
			if h.Link != nil {
				r.Link = h.Link(e.Key)
			}
			if !e.At.IsZero() {
				r.Ago = dur(now.Sub(e.At))
			}
			if e.Waited > 0 {
				r.Waited = dur(e.Waited)
			}
			if e.Took > 0 {
				r.Took = dur(e.Took)
			}
			var notes []string
			if e.Resync != "" {
				notes = append(notes, "resync ("+e.Resync+")")
			}
			switch {
			case e.Again:
				notes = append(notes, "after its current run")
			case !e.Ready.IsZero():
				notes = append(notes, "in "+dur(max(0, e.Ready.Sub(now)))+" (ran moments ago)")
			}
			r.Note = strings.Join(notes, ", ")
			out = append(out, r)
		}
		return out
	}
	// Held keys wait too, for their run or the throttle: after those a
	// worker may take now, since none will until then.
	p := page{Now: now.Format(time.RFC3339), Running: rows(s.Running), Waiting: rows(append(s.Waiting, s.Held...)),
		Failing: rows(s.Failing), Recent: rows(s.Recent)}
	if h.Facts != nil {
		p.Facts = h.Facts()
	}
	if h.Resyncs != nil {
		p.Facts = append(p.Facts, resyncFacts(h.Resyncs(), now)...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// resyncFacts sums up what resyncs caught, for the top of the page.
func resyncFacts[K comparable](rs Resyncs[K], now time.Time) []Fact {
	runs, caught := 0, 0
	for _, c := range rs.Counts {
		runs += c.Runs
		caught += c.Caught
	}
	v := fmt.Sprintf("%d runs; none caught anything in %s since start", runs, dur(now.Sub(rs.Since)))
	if len(rs.Caught) > 0 {
		v = fmt.Sprintf("%d runs; %d caught something, the last %s ago", runs, caught, dur(now.Sub(rs.Caught[0].At)))
	}
	return []Fact{{Name: "Resyncs", Value: v, Link: "resyncs"}}
}

// serveResyncs shows what resyncs caught: counts by kind, and the last
// catches with what each wrote.
func (h *Handler[K]) serveResyncs(w http.ResponseWriter) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	rs := h.Resyncs()
	p := &resyncs{Now: now.Format(time.RFC3339), Since: dur(now.Sub(rs.Since)) + " ago"}
	for _, c := range rs.Counts {
		rc := resyncCount{ResyncCount: c}
		if c.Runs > 0 {
			rc.Share = strconv.FormatFloat(100*float64(c.Caught)/float64(c.Runs), 'f', 1, 64) + "%"
		}
		p.Counts = append(p.Counts, rc)
	}
	for _, c := range rs.Caught {
		cr := caughtRow{Key: h.Name(c.Key), Ago: dur(now.Sub(c.At)), Reason: c.Reason, Writes: c.Writes, More: c.N - len(c.Writes)}
		if h.Link != nil {
			cr.Link = h.Link(c.Key)
		}
		p.Caught = append(p.Caught, cr)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := resyncTmpl.Execute(w, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// spanRow is a Span as the timeline shows it.
type spanRow struct {
	At, Took, What, Err string
	Indent              int // px
	// Left and Width place its bar, in percent of the run.
	Left, Width float64
	Open        bool // still running
	Wait        bool // a wait for a lock
}

type total struct {
	What, Took string
	Count      int
	d          time.Duration
}

type runPage struct {
	Key, Link, Started, Took, Waited, Err string
	Running                               bool
	Spans                                 []spanRow
	Totals                                []total
	Dropped                               int
}

// serveRun shows run id's timeline: a running or recent run.
func (h *Handler[K]) serveRun(w http.ResponseWriter, id string) {
	s := h.Queue.Snapshot()
	var e *queue.Entry[K]
	running := false
	for i := range s.Running {
		if strconv.FormatUint(s.Running[i].ID, 10) == id {
			e, running = &s.Running[i], true
		}
	}
	for i := range s.Recent {
		if e == nil && strconv.FormatUint(s.Recent[i].ID, 10) == id {
			e = &s.Recent[i]
		}
	}
	t, _ := func() (*trace.Trace, bool) {
		if e == nil {
			return nil, false
		}
		t, ok := e.Detail.(*trace.Trace)
		return t, ok
	}()
	if t == nil {
		http.Error(w, "No such run: only running and recent ones are kept.", http.StatusNotFound)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	length := e.Took
	if running {
		length = now.Sub(t.Started())
	}
	p := runPage{Key: h.Name(e.Key), Started: t.Started().Format("15:04:05.000"), Took: dur(length), Err: e.Err, Running: running}
	if h.Link != nil {
		p.Link = h.Link(e.Key)
	}
	if e.Waited > 0 {
		p.Waited = dur(e.Waited)
	}
	spans, dropped := t.Spans()
	p.Dropped = dropped
	totals := map[string]*total{}
	add := func(what string, d time.Duration) {
		if totals[what] == nil {
			totals[what] = &total{What: what}
		}
		totals[what].Count++
		totals[what].d += d
	}
	pct := func(d time.Duration) float64 {
		if length <= 0 {
			return 0
		}
		return min(100, max(0, 100*float64(d)/float64(length)))
	}
	for i, sp := range spans {
		took := sp.Took
		open := took < 0
		if open {
			took = now.Sub(t.Started()) - sp.Start
		}
		leaf := i+1 >= len(spans) || spans[i+1].Depth <= sp.Depth
		switch {
		case strings.HasPrefix(sp.What, "wait for "):
			add("waiting for locks", took)
		case leaf && strings.HasPrefix(sp.What, "github GET"), leaf && strings.HasPrefix(sp.What, "github HEAD"):
			add("GitHub reads", took)
		case leaf && strings.HasPrefix(sp.What, "github "):
			add("GitHub writes", took)
		case leaf && strings.HasPrefix(sp.What, "gerrit "):
			add("Gerrit requests", took)
		case leaf && strings.HasPrefix(sp.What, "git "):
			add("git commands", took)
		}
		r := spanRow{At: "+" + dur(sp.Start), Took: dur(took), What: sp.What, Err: sp.Err, Indent: 14 * sp.Depth, Open: open,
			Left: pct(sp.Start), Width: max(pct(took), 0.3), Wait: strings.HasPrefix(sp.What, "wait for ")}
		if sp.Start == 0 {
			r.At = "+0"
		}
		p.Spans = append(p.Spans, r)
	}
	for _, t := range totals {
		t.Took = dur(t.d)
		p.Totals = append(p.Totals, *t)
	}
	sort.Slice(p.Totals, func(i, j int) bool { return totals[p.Totals[i].What].d > totals[p.Totals[j].What].d })
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := runTmpl.Execute(w, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dur is d rounded for reading: <1ms, 340ms, 12.4s, 3m05s.
func dur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

var tmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="5">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bridge status</title>
<style>
:root { color-scheme: light dark; --line: #8884; --dim: #888; --bad: #c33; }
body { font: 14px/1.4 system-ui, sans-serif; margin: 16px; background: Canvas; color: CanvasText; }
h1 { font-size: 18px; margin: 0 0 4px; } h2 { font-size: 15px; margin: 20px 0 6px; }
table { border-collapse: collapse; width: 100%; } td, th { text-align: left; padding: 3px 10px 3px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
th { font-weight: 600; } td.n { font-variant-numeric: tabular-nums; white-space: nowrap; }
.dim { color: var(--dim); } .err { color: var(--bad); word-break: break-word; }
</style></head><body>
<h1>Bridge status</h1>
<div class="dim">{{.Now}} · refreshes every 5s</div>
{{with .Facts}}<table style="width:auto;margin-top:10px">{{range .}}<tr><th>{{.Name}}</th><td>{{if .Link}}<a href="{{.Link}}">{{.Value}}</a>{{else}}{{.Value}}{{end}}</td></tr>{{end}}</table>{{end}}
{{define "run"}}{{if .Run}}<a href="{{.Run}}">timeline</a>{{end}}{{end}}
{{define "key"}}{{if .Link}}<a href="{{.Link}}" target="_blank" rel="noopener">{{.Key}}</a>{{else}}{{.Key}}{{end}}{{end}}
<h2>Running ({{len .Running}})</h2>
{{if .Running}}<table><tr><th>Key</th><th>For</th><th>Waited first</th><th></th><th></th></tr>
{{range .Running}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}}</td><td class="n">{{.Waited}}</td><td>{{template "run" .}}</td><td class="err">{{if .Failures}}failed {{.Failures}}× before: {{.Err}}{{end}}</td></tr>{{end}}</table>{{else}}<div class="dim">Idle.</div>{{end}}
<h2>Waiting ({{len .Waiting}})</h2>
{{if .Waiting}}<table><tr><th>#</th><th>Key</th><th>Waiting</th><th></th></tr>
{{range $i, $r := .Waiting}}<tr><td class="n dim">{{$i}}</td><td>{{template "key" $r}}</td><td class="n">{{$r.Ago}}</td><td class="dim">{{$r.Note}}</td></tr>{{end}}</table>{{else}}<div class="dim">Nothing waiting.</div>{{end}}
<h2>Failing ({{len .Failing}})</h2>
{{if .Failing}}<table><tr><th>Key</th><th>Failures</th><th>Last error</th></tr>
{{range .Failing}}<tr><td>{{template "key" .}}</td><td class="n">{{.Failures}}</td><td class="err">{{.Err}}</td></tr>{{end}}</table>{{else}}<div class="dim">None.</div>{{end}}
<h2>Recently done ({{len .Recent}})</h2>
{{if .Recent}}<table><tr><th>Key</th><th>Started</th><th>Waited</th><th>Took</th><th></th><th></th></tr>
{{range .Recent}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}} ago</td><td class="n">{{.Waited}}</td><td class="n">{{.Took}}</td><td>{{template "run" .}}</td><td class="err">{{.Err}}</td></tr>{{end}}</table>{{end}}
</body></html>
`))

var resyncTmpl = template.Must(template.New("resyncs").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="30">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bridge resyncs</title>
<style>
:root { color-scheme: light dark; --line: #8884; --dim: #888; }
body { font: 14px/1.4 system-ui, sans-serif; margin: 16px; background: Canvas; color: CanvasText; }
h1 { font-size: 18px; margin: 0 0 4px; }
table { border-collapse: collapse; width: 100%; margin-top: 10px; } td, th { text-align: left; padding: 3px 10px 3px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
th { font-weight: 600; } td.n { font-variant-numeric: tabular-nums; white-space: nowrap; }
td.w { word-break: break-all; font-family: ui-monospace, monospace; font-size: 12px; } .dim { color: var(--dim); }
</style></head><body>
<a href="./">← status</a>
<h1>What resyncs caught</h1>
<div class="dim">{{.Now}} · refreshes every 30s</div>
{{define "key"}}{{if .Link}}<a href="{{.Link}}" target="_blank" rel="noopener">{{.Key}}</a>{{else}}{{.Key}}{{end}}{{end}}
<div class="dim">Since {{.Since}}: runs only a resync asked for, and those that changed something on Gerrit or GitHub, which an event should have done.</div>
{{if .Counts}}<table style="width:auto"><tr><th>Why</th><th>Key</th><th>Runs</th><th>Caught</th><th></th><th>Failed</th></tr>
{{range .Counts}}<tr><td>{{.Reason}}</td><td>{{.Key}}</td><td class="n">{{.Runs}}</td><td class="n">{{.Caught}}</td><td class="n dim">{{.Share}}</td><td class="n">{{.Failed}}</td></tr>{{end}}</table>{{else}}<div class="dim">No resync runs yet.</div>{{end}}
{{if .Caught}}<table><tr><th>Key</th><th>When</th><th>Why</th><th>What it wrote</th></tr>
{{range .Caught}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}} ago</td><td>{{.Reason}}</td><td class="w">{{range .Writes}}{{.}}<br>{{end}}{{with .More}}<span class="dim">and {{.}} more</span>{{end}}</td></tr>{{end}}</table>{{end}}</body></html>
`))

var runTmpl = template.Must(template.New("run").Parse(`<!doctype html>
<html><head><meta charset="utf-8">{{if .Running}}<meta http-equiv="refresh" content="2">{{end}}
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bridge run</title>
<style>
:root { color-scheme: light dark; --line: #8884; --dim: #888; --bad: #c33; --bar: #4a7fd4; --wait: #d49a3a; }
body { font: 14px/1.4 system-ui, sans-serif; margin: 16px; background: Canvas; color: CanvasText; }
h1 { font-size: 18px; margin: 0 0 4px; } h2 { font-size: 15px; margin: 20px 0 6px; }
table { border-collapse: collapse; width: 100%; } td, th { text-align: left; padding: 2px 10px 2px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
td.n { font-variant-numeric: tabular-nums; white-space: nowrap; } td.w { word-break: break-all; font-family: ui-monospace, monospace; font-size: 12px; }
td.t { width: 28%; min-width: 120px; } .track { position: relative; height: 10px; }
.bar { position: absolute; top: 1px; height: 8px; background: var(--bar); border-radius: 2px; }
.bar.wait { background: var(--wait); } .bar.open { opacity: .5; }
.dim { color: var(--dim); } .err { color: var(--bad); }
</style></head><body>
<a href="../">← status</a>
<h1>{{if .Link}}<a href="{{.Link}}" target="_blank" rel="noopener">{{.Key}}</a>{{else}}{{.Key}}{{end}}</h1>
<div class="dim">started {{.Started}}{{with .Waited}} after waiting {{.}} in the queue{{end}} · {{if .Running}}running for {{.Took}} (refreshes){{else}}took {{.Took}}{{end}}</div>
{{with .Err}}<div class="err">{{.}}</div>{{end}}
{{with .Totals}}<h2>Where the time went</h2>
<table style="width:auto">{{range .}}<tr><th>{{.What}}</th><td class="n">{{.Took}}</td><td class="n dim">{{.Count}}×</td></tr>{{end}}</table>
<div class="dim">Lock waits overlap what holds them; calls inside a phase count once.</div>{{end}}
<h2>Timeline ({{len .Spans}})</h2>
{{with .Dropped}}<div class="dim">{{.}} more not kept.</div>{{end}}
<table><tr><th>At</th><th>Took</th><th></th><th>What</th></tr>
{{range .Spans}}<tr><td class="n dim">{{.At}}</td><td class="n">{{.Took}}{{if .Open}}…{{end}}</td>
<td class="t"><div class="track"><div class="bar{{if .Wait}} wait{{end}}{{if .Open}} open{{end}}" style="left:{{printf "%.2f" .Left}}%;width:{{printf "%.2f" .Width}}%"></div></div></td>
<td class="w" style="padding-left:{{.Indent}}px">{{.What}}{{with .Err}} <span class="err">{{.}}</span>{{end}}</td></tr>{{end}}</table>
</body></html>
`))

// loopbackHost reports whether a request's Host names this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
