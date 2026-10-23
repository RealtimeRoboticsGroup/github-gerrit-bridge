// Package status serves a page of what the bridge is doing: its work
// queue (running, waiting, held, failing, recently done) and a few facts
// (the last Gerrit event, GitHub's rate budget). For operators only: the
// queue would tell an attacker when the bridge is starved, so it is
// served on its own loopback listener (config status_listen).
package status

import (
	"html/template"
	"net/http"
	"time"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/queue"
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
}

type page struct {
	Now                                     string
	Facts                                   []Fact
	Running, Waiting, Held, Failing, Recent []row
}

func (h *Handler[K]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	s := h.Queue.Snapshot()
	rows := func(es []queue.Entry[K]) []row {
		var out []row
		for _, e := range es {
			r := row{Key: h.Name(e.Key), Failures: e.Failures, Err: e.Err}
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
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if err := tmpl.Execute(w, p); err != nil {
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
{{define "key"}}{{if .Link}}<a href="{{.Link}}">{{.Key}}</a>{{else}}{{.Key}}{{end}}{{end}}
<h2>Running ({{len .Running}})</h2>
{{if .Running}}<table><tr><th>Key</th><th>For</th><th>Waited first</th><th></th></tr>
{{range .Running}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}}</td><td class="n">{{.Waited}}</td><td class="err">{{if .Failures}}failed {{.Failures}}× before: {{.Err}}{{end}}</td></tr>{{end}}</table>{{else}}<div class="dim">Idle.</div>{{end}}
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
{{if .Recent}}<table><tr><th>Key</th><th>Started</th><th>Waited</th><th>Took</th><th></th></tr>
{{range .Recent}}<tr><td>{{template "key" .}}</td><td class="n">{{.Ago}} ago</td><td class="n">{{.Waited}}</td><td class="n">{{.Took}}</td><td class="err">{{.Err}}</td></tr>{{end}}</table>{{end}}
</body></html>
`))
