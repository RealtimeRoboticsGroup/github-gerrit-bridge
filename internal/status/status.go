// Package status serves a page of what the bridge is doing: its work
// queue (running, waiting, held, failing, recently done) and a few facts
// (the last Gerrit event, GitHub's rate budget). For operators only: the
// queue would tell an attacker when the bridge is starved, so it is
// served on its own loopback listener (config status_listen).
package status

import (
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

// Fact is one line of the page's summary.
type Fact struct{ Name, Value string }

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
}

// row is an Entry as the page shows it.
type row struct {
	Key, Link, Ago, Waited, Took, Note, Err string
	Failures                                int
	// Run links its timeline ("" if it has none).
	Run string
}

type page struct {
	Now                                     string
	Facts                                   []Fact
	Running, Waiting, Held, Failing, Recent []row
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
	if id, ok := strings.CutPrefix(r.URL.Path, "/run/"); ok {
		h.serveRun(w, id)
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
			switch {
			case e.Resync:
				r.Note = "resync"
			case e.Again:
				r.Note = "runs again after this run"
			}
			out = append(out, r)
		}
		return out
	}
	p := page{Now: now.Format(time.RFC3339), Running: rows(s.Running), Waiting: rows(s.Waiting),
		Held: rows(s.Held), Failing: rows(s.Failing), Recent: rows(s.Recent)}
	if h.Facts != nil {
		p.Facts = h.Facts()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, p); err != nil {
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
{{with .Facts}}<table style="width:auto;margin-top:10px">{{range .}}<tr><th>{{.Name}}</th><td>{{.Value}}</td></tr>{{end}}</table>{{end}}
{{define "run"}}{{if .Run}}<a href="{{.Run}}">timeline</a>{{end}}{{end}}
{{define "key"}}{{if .Link}}<a href="{{.Link}}">{{.Key}}</a>{{else}}{{.Key}}{{end}}{{end}}
<h2>Running ({{len .Running}})</h2>
{{if .Running}}<table><tr><th>Key</th><th>For</th><th>Waited first</th><th></th><th></th></tr>
{{range .Running}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}}</td><td class="n">{{.Waited}}</td><td>{{template "run" .}}</td><td class="err">{{if .Failures}}failed {{.Failures}}× before: {{.Err}}{{end}}</td></tr>{{end}}</table>{{else}}<div class="dim">Idle.</div>{{end}}
<h2>Waiting ({{len .Waiting}})</h2>
{{if .Waiting}}<table><tr><th>#</th><th>Key</th><th>Waiting</th><th></th></tr>
{{range $i, $r := .Waiting}}<tr><td class="n dim">{{$i}}</td><td>{{template "key" $r}}</td><td class="n">{{$r.Ago}}</td><td class="dim">{{$r.Note}}</td></tr>{{end}}</table>{{else}}<div class="dim">Nothing waiting.</div>{{end}}
<h2>Held ({{len .Held}})</h2>
<div class="dim">Added, but held back: it is running and will run again, or it ran moments ago.</div>
{{if .Held}}<table><tr><th>Key</th><th>Added</th><th></th></tr>
{{range .Held}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}} ago</td><td class="dim">{{or .Note "ran moments ago"}}</td></tr>{{end}}</table>{{end}}
<h2>Failing ({{len .Failing}})</h2>
{{if .Failing}}<table><tr><th>Key</th><th>Failures</th><th>Last error</th></tr>
{{range .Failing}}<tr><td>{{template "key" .}}</td><td class="n">{{.Failures}}</td><td class="err">{{.Err}}</td></tr>{{end}}</table>{{else}}<div class="dim">None.</div>{{end}}
<h2>Recently done ({{len .Recent}})</h2>
{{if .Recent}}<table><tr><th>Key</th><th>Started</th><th>Waited</th><th>Took</th><th></th><th></th></tr>
{{range .Recent}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}} ago</td><td class="n">{{.Waited}}</td><td class="n">{{.Took}}</td><td>{{template "run" .}}</td><td class="err">{{.Err}}</td></tr>{{end}}</table>{{end}}
</body></html>
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
<h1>{{if .Link}}<a href="{{.Link}}">{{.Key}}</a>{{else}}{{.Key}}{{end}}</h1>
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
