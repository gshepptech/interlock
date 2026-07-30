// Package dashboard serves a read-only view of who is working the repo.
//
// It renders on the server with no JavaScript and no external assets. That is not
// minimalism for its own sake: this page displays file paths and model names that
// came out of another tool's session files, so everything goes through
// html/template, which escapes by default. Fetching a font or a script would also
// mean a network call, and this tool reads someone's development activity.
//
// The page refreshes itself with a meta refresh rather than polling in script,
// which keeps the whole thing inspectable in one file.
package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"interlock/internal/config"
	"interlock/internal/journal"
	"interlock/internal/lease"
	"interlock/internal/session"
)

// View is everything the page shows, assembled before rendering so the template
// contains no logic worth testing separately.
type View struct {
	Root        string
	Teams       []TeamView
	Sessions    []SessionView
	Leases      []LeaseView
	Events      []EventView
	Prevented   int
	Escaped     int
	GeneratedAt string
	RefreshSecs int
	Warning     string
}

type TeamView struct {
	Name    string
	Owns    []string
	Shared  []string
	Agents  []string
	Active  int
	Holding int
}

type SessionView struct {
	Agent    string
	Team     string
	Model    string
	Started  string
	LastSeen string
	Idle     bool
	Recent   []TouchView
	ShortID  string
}

type TouchView struct {
	Path string
	Ago  string
}

// EventView is one thing the tool did, for the feed that makes its work visible.
type EventView struct {
	Ago     string
	Kind    string
	Label   string
	Path    string
	Agent   string
	Team    string
	Detail  string
	Escaped bool
}

type LeaseView struct {
	Path    string
	Team    string
	Agent   string
	Held    string
	ShortID string
}

// Build assembles the view. It takes already-loaded state rather than reading it
// so the assembly is testable without a repo on disk.
func Build(cfg *config.Config, sessions []*session.Session, leases []*lease.Lease,
	events []journal.Entry, now time.Time) View {
	v := View{
		Root:        cfg.Root,
		GeneratedAt: now.Format("15:04:05"),
		RefreshSecs: 3,
	}

	holdingByTeam := map[string]int{}
	for _, l := range leases {
		holdingByTeam[l.Team]++
		v.Leases = append(v.Leases, LeaseView{
			Path:    l.Path,
			Team:    l.Team,
			Agent:   l.Agent,
			Held:    since(now, l.Acquired),
			ShortID: short(l.SessionID),
		})
	}

	activeByTeam := map[string]int{}
	for _, s := range sessions {
		activeByTeam[s.Team]++
		sv := SessionView{
			Agent:    s.Agent,
			Team:     s.Team,
			Model:    s.Model,
			Started:  since(now, s.Started),
			LastSeen: since(now, s.LastSeen),
			// A session quiet for a while is probably thinking, not gone. Worth
			// showing differently so a stalled agent is visible.
			Idle:    now.Sub(s.LastSeen) > 2*time.Minute,
			ShortID: short(s.ID),
		}
		for _, t := range s.Recent {
			sv.Recent = append(sv.Recent, TouchView{Path: t.Path, Ago: since(now, t.At)})
		}
		v.Sessions = append(v.Sessions, sv)
	}

	for _, t := range cfg.Map.Teams {
		v.Teams = append(v.Teams, TeamView{
			Name:    t.Name,
			Owns:    t.Owns,
			Shared:  t.Shared,
			Agents:  cfg.AgentsFor(t.Name),
			Active:  activeByTeam[t.Name],
			Holding: holdingByTeam[t.Name],
		})
	}

	for _, e := range events {
		ev := EventView{
			Ago: since(now, e.At), Kind: string(e.Kind), Path: e.Path,
			Agent: e.Agent, Team: e.Team, Detail: e.Detail,
		}
		switch e.Kind {
		case journal.BlockedTerritory:
			ev.Label = "blocked, wrong territory"
			v.Prevented++
		case journal.BlockedLease:
			ev.Label = "blocked, file held"
			v.Prevented++
		case journal.EscapedGuard:
			// Counted apart from the blocks and marked, because this is the
			// tool reporting its own miss. Folding it in with the successes
			// would be the page flattering itself.
			ev.Label = "got through, wrong territory"
			ev.Escaped = true
			v.Escaped++
		}
		v.Events = append(v.Events, ev)
	}

	if len(v.Sessions) == 0 {
		v.Warning = "No agent has run here since this tool was installed. " +
			"Territory is only enforced for agents whose hooks are wired up. " +
			"Run interlock doctor to check."
	}
	return v
}

// Handler serves the page. load is called per request, so the page reflects the
// repo as it is now rather than as it was when the server started.
func Handler(load func() (View, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		view, err := load()
		if err != nil {
			http.Error(w, "cannot read coordination state: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Nothing here should ever be cached: it is a live view.
		w.Header().Set("Cache-Control", "no-store")
		if err := page.Execute(w, view); err != nil {
			// The response is already partly written, so there is nothing useful
			// to send. The next refresh will try again.
			return
		}
	})
}

func since(now, then time.Time) string {
	if then.IsZero() {
		return "unknown"
	}
	d := now.Sub(then)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// The palette is warm dark with two functional accents, and it follows the
// viewer's light or dark preference. Green means holding or in your own
// territory, rust means contention. Nothing is coloured for decoration.
//
// System fonts, deliberately: a vendored font would be bytes to ship and a
// remote font would be a network call, and this page must make neither.
var page = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{.RefreshSecs}}">
<title>interlock</title>
<!-- An empty data URI, so the browser does not request a favicon this server
     would 404. A page that refreshes every few seconds would otherwise log an
     error every few seconds. -->
<link rel="icon" href="data:,">
<style>
:root {
  --bg: #f6f4ef; --panel: #fffefb; --ink: #1b1a17; --muted: #6d675d;
  --rule: #ddd7c9; --hold: #1f6b52; --contend: #9a4118;
  color-scheme: light dark;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #171613; --panel: #1f1e1a; --ink: #ece8df; --muted: #948d80;
    --rule: #322f28; --hold: #63b592; --contend: #d98457;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 2rem 1.25rem; background: var(--bg); color: var(--ink);
  font: 14px/1.55 ui-sans-serif, system-ui, sans-serif;
}
.wrap { max-width: 68rem; margin: 0 auto; }
header { display: flex; flex-wrap: wrap; gap: .75rem 1.5rem; align-items: baseline;
  border-bottom: 1px solid var(--rule); padding-bottom: .75rem; margin-bottom: 1.5rem; }
h1 { font-size: 1.05rem; margin: 0; letter-spacing: .01em; }
h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .09em;
  color: var(--muted); margin: 2rem 0 .6rem; font-weight: 600; }
.mono, code, td, th { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.meta { color: var(--muted); font-size: .82rem; }
.card { background: var(--panel); border: 1px solid var(--rule); border-radius: 6px;
  padding: .85rem 1rem; margin-bottom: .6rem; }
.row { display: flex; flex-wrap: wrap; gap: .4rem 1rem; align-items: baseline; }
.grow { flex: 1 1 12rem; }
.tag { font-family: ui-monospace, monospace; font-size: .78rem; padding: .1rem .45rem;
  border: 1px solid var(--rule); border-radius: 3px; color: var(--muted); }
.hold { color: var(--hold); border-color: var(--hold); }
.contend { color: var(--contend); border-color: var(--contend); }
.files { margin: .5rem 0 0; padding: 0; list-style: none;
  font-family: ui-monospace, monospace; font-size: .82rem; }
.files li { display: flex; justify-content: space-between; gap: 1rem;
  padding: .12rem 0; border-top: 1px dotted var(--rule); }
.files li span:last-child { color: var(--muted); white-space: nowrap; }
.scroll { overflow-x: auto; }
.empty { color: var(--muted); font-style: italic; }
.warn { border-left: 3px solid var(--contend); padding-left: .8rem; color: var(--muted); }
.paths { color: var(--muted); font-size: .82rem; word-break: break-all; }
</style>
</head>
<body>
<div class="wrap">
<header>
  <h1>interlock</h1>
  <span class="meta mono grow">{{.Root}}</span>
  <span class="meta">{{.GeneratedAt}}, refreshing every {{.RefreshSecs}}s</span>
</header>

{{if .Warning}}<p class="warn">{{.Warning}}</p>{{end}}

<h2>Agents here now</h2>
{{if .Sessions}}
  {{range .Sessions}}
  <div class="card">
    <div class="row">
      <strong class="grow">{{.Agent}}{{if .Team}} on {{.Team}}{{end}}</strong>
      {{if .Model}}<span class="tag">{{.Model}}</span>{{end}}
      <span class="tag{{if .Idle}} contend{{end}}">
        {{if .Idle}}quiet {{.LastSeen}}{{else}}active {{.LastSeen}} ago{{end}}
      </span>
      <span class="meta mono">{{.ShortID}}</span>
      <span class="meta">up {{.Started}}</span>
    </div>
    {{if .Recent}}
    <ul class="files">
      {{range .Recent}}<li><span>{{.Path}}</span><span>{{.Ago}} ago</span></li>{{end}}
    </ul>
    {{end}}
  </div>
  {{end}}
{{else}}
  <p class="empty">Nobody is working here.</p>
{{end}}

<h2>What this has prevented</h2>
{{if .Events}}
  <div class="row" style="margin-bottom:.6rem">
    <span class="tag hold">{{.Prevented}} collision{{if ne .Prevented 1}}s{{end}} prevented</span>
    {{if .Escaped}}<span class="tag contend">{{.Escaped}} got through</span>{{end}}
  </div>
  {{range .Events}}
  <div class="card">
    <div class="row">
      <code class="grow">{{.Path}}</code>
      <span class="tag{{if .Escaped}} contend{{else}} hold{{end}}">{{.Label}}</span>
      <span class="meta">{{.Ago}} ago</span>
    </div>
    <div class="paths">{{.Agent}} on {{.Team}}: {{.Detail}}</div>
  </div>
  {{end}}
{{else}}
  <p class="empty">Nothing has been blocked. Either the agents have stayed in their
  own territory, or the hooks are not wired up. Run <code>interlock doctor</code>
  to tell the difference.</p>
{{end}}

<h2>Shared files held</h2>
{{if .Leases}}
  {{range .Leases}}
  <div class="card row">
    <code class="grow">{{.Path}}</code>
    <span class="tag hold">{{.Team}}</span>
    <span class="meta">{{.Agent}}, {{.ShortID}}, for {{.Held}}</span>
  </div>
  {{end}}
{{else}}
  <p class="empty">Nothing held. Shared files are free.</p>
{{end}}

<h2>Territory</h2>
{{range .Teams}}
<div class="card">
  <div class="row">
    <strong class="grow">{{.Name}}</strong>
    {{if .Agents}}<span class="meta">default for {{range $i, $a := .Agents}}{{if $i}}, {{end}}{{$a}}{{end}}</span>{{end}}
    <span class="tag{{if .Active}} hold{{end}}">{{.Active}} active</span>
    {{if .Holding}}<span class="tag hold">holding {{.Holding}}</span>{{end}}
  </div>
  <div class="paths scroll">owns {{range $i, $p := .Owns}}{{if $i}} · {{end}}{{$p}}{{end}}</div>
  {{if .Shared}}<div class="paths scroll">shared {{range $i, $p := .Shared}}{{if $i}} · {{end}}{{$p}}{{end}}</div>{{end}}
</div>
{{end}}
</div>
</body>
</html>
`))
