// Package dashboard serves a read-only view of who is working the repo.
//
// It renders on the server with no JavaScript and no external assets. That is not
// minimalism for its own sake: this page displays file paths and model names that
// came out of another tool's session files, so everything goes through
// html/template, which escapes by default. Fetching a font or a script would also
// mean a network call, and this tool reads someone's development activity.
//
// The page refreshes itself with a meta refresh and one tiny same-page script
// that reloads when a backgrounded tab comes back into view. Background tabs can
// throttle meta refreshes, and this is a live status panel; showing a stale
// quiet state after an agent wakes up is worse than spending one local reload.
package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/session"
)

// View is everything the page shows, assembled before rendering so the template
// contains no logic worth testing separately.
type View struct {
	Root        string
	Teams       []TeamView
	Sessions    []SessionView
	Runs        []RunView
	Leases      []LeaseView
	Events      []EventView
	Prevented   int
	Escaped     int
	GeneratedAt string
	RefreshSecs int
	Warning     string
}

// RunView is an agent Interlock started.
//
// It is kept apart from SessionView because the two are different kinds of
// claim. A session is what hook events let us infer about an agent that turned
// up; a run is a process Interlock owns, whose state it knows. Merging them
// would mean picking one vocabulary for both, and whichever was picked would
// overstate one of them.
type RunView struct {
	ID      string
	ShortID string
	State   string
	// Describe is the sentence a person reads: the state plus the detail that
	// makes it actionable, such as the exit code or that force is required.
	Describe string
	Runner   string
	Name     string
	Model    string
	Team     string
	Dir      string
	Age      string
	Duration string
	Summary  string
	RetryOf  string
	// Live marks a run with a process behind it, which is what decides whether
	// lifecycle controls mean anything.
	Live          bool
	Paused        bool
	Stopping      bool
	ForceRequired bool
	Terminal      bool
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
	ID       string
	Agent    string
	Team     string
	Model    string
	Started  string
	LastSeen string
	Expires  string
	Recent   []TouchView
	ShortID  string
	// Aspect is the signal this agent is showing: proceeding, waiting, or
	// stopped. It is the one piece of state a person scanning the page needs,
	// so it is computed here rather than left to the template.
	Aspect Aspect
	// Areas are the directories this agent recently wrote in. More useful at a
	// glance than a file list: it says which part of the repo is busy.
	Areas []string
	// Latest is the last file written, kept alongside Areas because "which part
	// of the repo" and "what exactly" answer different questions.
	Latest *TouchView
	Holds  int
	// RunID is set when Interlock started this agent. Everything else on this
	// page is inferred from hook events; this is the one field that says the
	// process is ours, and it comes from the environment the supervisor built
	// rather than from anything the agent could claim.
	RunID string
}

// Managed reports whether Interlock launched this agent.
func (s SessionView) Managed() bool { return s.RunID != "" }

// Origin is the word shown beside a session. The distinction is load-bearing:
// an observed agent can be watched and blocked but never paused or stopped,
// because Interlock does not own its process.
func (s SessionView) Origin() string {
	if s.Managed() {
		return "managed"
	}
	return "observed"
}

// Aspect is an agent's current state, named for the signal it would be showing.
type Aspect string

const (
	// Proceeding: seen very recently, nothing in its way.
	Proceeding Aspect = "proceeding"
	// Waiting: alive but quiet, most likely thinking or parked.
	Waiting Aspect = "waiting"
	// Stopped: was refused a file within the last minute.
	Stopped Aspect = "stopped"
	// Ended: no hook event arrived before the observation expiry threshold.
	Ended Aspect = "ended"
)

// Label is the plain word shown to a person. Interlock only sees hook events,
// so the active state is deliberately phrased as recency, not certainty that an
// agent is currently producing work.
func (a Aspect) Label() string {
	switch a {
	case Stopped:
		return "blocked"
	case Waiting:
		return "quiet"
	case Ended:
		return "ended"
	}
	return "recent"
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
	Expires string
	ShortID string
}

// RunViews renders the runs Interlock started.
//
// Every field here is a fact from the run record. Nothing is inferred from
// recency the way a session's aspect is, because for these processes there is
// nothing to infer: a supervisor either answered for the run or the run is
// marked lost, and both are decided before the page is built.
func RunViews(records []managedrun.Record, now time.Time) []RunView {
	views := make([]RunView, 0, len(records))
	for _, record := range records {
		state := record.Status.State
		views = append(views, RunView{
			ID: record.ID(), ShortID: short(record.ID()),
			State: string(state), Describe: record.Describe(),
			Runner: string(record.Runner()), Name: record.Label(),
			Model: record.Spec.Options.Model, Team: record.Spec.Options.Team,
			Dir: record.Spec.Options.Dir(), Age: since(now, record.Spec.CreatedAt),
			Duration: duration(record.Duration(now)), Summary: record.Status.Summary,
			RetryOf:       record.Spec.RetryOf,
			Live:          state.Live(),
			Paused:        state == managedrun.Paused,
			Stopping:      state == managedrun.Stopping,
			ForceRequired: record.Status.ForceRequired,
			Terminal:      state.Terminal(),
		})
	}
	return views
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
			Expires: "in " + duration(l.Remaining(now)),
			ShortID: short(l.SessionID),
		})
	}

	// A block within the last minute is what makes an agent read as stopped
	// rather than merely quiet, so the journal has to be consulted per session.
	blockedRecently := map[string]bool{}
	for _, e := range events {
		if e.Kind != journal.EscapedGuard && now.Sub(e.At) < time.Minute {
			blockedRecently[e.SessionID] = true
		}
	}
	holdsBySession := map[string]int{}
	for _, l := range leases {
		holdsBySession[l.SessionID]++
	}

	activeByTeam := map[string]int{}
	for _, s := range sessions {
		if !s.Idle(now) {
			activeByTeam[s.Team]++
		}
		sv := SessionView{
			ID:       s.ID,
			Agent:    s.Agent,
			Team:     s.Team,
			Model:    s.Model,
			Started:  since(now, s.Started),
			LastSeen: since(now, s.LastSeen),
			Expires:  observationExpiry(now, s),
			ShortID:  short(s.ID),
			Holds:    holdsBySession[s.ID],
			Aspect:   Proceeding,
			RunID:    s.RunID,
		}
		switch s.ObservationAt(now) {
		case session.Ended:
			sv.Aspect = Ended
		case session.Quiet:
			// A recent hook means the session is alive, not necessarily working
			// this second. Flip to quiet quickly so the panel does not overstate
			// what Interlock can know.
			sv.Aspect = Waiting
		}
		if sv.Aspect != Ended && blockedRecently[s.ID] {
			sv.Aspect = Stopped
		}

		seen := map[string]bool{}
		for _, t := range s.Recent {
			touch := TouchView{Path: t.Path, Ago: since(now, t.At)}
			sv.Recent = append(sv.Recent, touch)
			if sv.Latest == nil {
				sv.Latest = &touch
			}
			if dir := filepath.Dir(t.Path); dir != "." && !seen[dir] {
				seen[dir] = true
				if len(sv.Areas) < 3 {
					sv.Areas = append(sv.Areas, dir)
				}
			}
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
		case journal.ControlAction:
			ev.Label = string(e.Action) + ": " + string(e.Result)
			ev.Path = e.Target
			ev.Agent = string(e.Actor)
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
		if err := readOnlyPage.Execute(w, view); err != nil {
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

func observationExpiry(now time.Time, sess *session.Session) string {
	expires := sess.LastSeen.Add(session.IdleAfter)
	if !expires.After(now) {
		return duration(now.Sub(expires)) + " ago"
	}
	return "in " + duration(expires.Sub(now))
}

func duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return since(time.Unix(0, d.Nanoseconds()), time.Unix(0, 0))
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// The page is built to look like a signalling panel, because an interlocking is
// one. Files are track sections that are occupied or clear, and each agent shows
// an aspect: green recent, amber quiet, red blocked, or grey ended. Those colours
// are the railway's own system rather than decoration, which is why they are the
// only colour on the page.
//
// The vocabulary stays in the visual language and out of the writing. A person
// reading this needs to know an agent is blocked, not that a signal is at
// danger, so every word on the page is plain.
//
// System fonts, because the page must fetch nothing: a vendored face would be
// bytes to ship and a remote one would be a network call. The personality comes
// from the treatment instead, letterspaced panel legends over tabular data.
var readOnlyPage = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{.RefreshSecs}}">
<title>Interlock</title>
<link rel="icon" href="data:,">
<script>
(() => {
  let hiddenAt = 0;
  const remember = () => { if (document.hidden) hiddenAt = Date.now(); };
  const catchUp = () => {
    if (!document.hidden && hiddenAt && Date.now() - hiddenAt > 1000) {
      location.reload();
    }
  };
  document.addEventListener("visibilitychange", () => { remember(); catchUp(); });
  window.addEventListener("focus", catchUp);
})();
</script>
<style>
:root {
  --panel:      #ffffff;
  --deck:       #e8ebed;
  --ink:        #14181b;
  --ink-2:      #5b666d;
  --ink-3:      #8b959b;
  --rule:       #ccd3d7;
  --rule-hard:  #aab4ba;
  --proceeding: #1c7a4b;
  --waiting:    #b57500;
  --stopped:    #bd1f2d;
  --ended:      #8b959b;
  --occupied:   #14181b;
}
@media (prefers-color-scheme: dark) {
  :root {
    --panel:      #1b2024;
    --deck:       #111518;
    --ink:        #e6ebee;
    --ink-2:      #98a4ab;
    --ink-3:      #6d787f;
    --rule:       #2b3238;
    --rule-hard:  #3d464d;
    --proceeding: #43b47c;
    --waiting:    #d99a2b;
    --stopped:    #e0616c;
    --ended:      #6d787f;
    --occupied:   #e6ebee;
  }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0; background: var(--deck); color: var(--ink);
  font: 13px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
  font-variant-numeric: tabular-nums;
}
.mono, code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.wrap { max-width: 1180px; margin: 0 auto; padding: 0 16px 56px; }

/* Panel header: the plate on the front of the equipment. */
.head {
  display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px 20px;
  padding: 18px 0 14px; border-bottom: 2px solid var(--rule-hard);
}
.mark {
  font-size: 13px; font-weight: 700; letter-spacing: .34em; text-transform: uppercase;
}
.repo { flex: 1 1 auto; color: var(--ink-2); font-size: 12px; word-break: break-all; }
.clock { color: var(--ink-3); font-size: 11px; letter-spacing: .06em; }

/* Indications: the counters along the top of a panel. */
.indications {
  display: grid; grid-template-columns: repeat(auto-fit, minmax(132px, 1fr));
  border-bottom: 1px solid var(--rule); background: var(--panel);
}
.ind { padding: 14px 16px; border-right: 1px solid var(--rule); }
.ind:last-child { border-right: 0; }
.ind b {
  display: block; font-family: ui-monospace, monospace; font-size: 30px;
  font-weight: 500; line-height: 1.05; letter-spacing: -.02em;
}
.ind span {
  display: block; margin-top: 5px; font-size: 10px; font-weight: 600;
  letter-spacing: .13em; text-transform: uppercase; color: var(--ink-3);
}
.ind.alert b { color: var(--stopped); }

h2 {
  margin: 30px 0 0; padding-bottom: 7px; border-bottom: 1px solid var(--rule);
  font-size: 10px; font-weight: 700; letter-spacing: .15em; text-transform: uppercase;
  color: var(--ink-3);
}
h2 em { font-style: normal; color: var(--ink-3); font-weight: 500; letter-spacing: .04em; text-transform: none; }

table { width: 100%; border-collapse: collapse; background: var(--panel); }
.scroll { overflow-x: auto; }
th {
  text-align: left; padding: 9px 12px; font-size: 10px; font-weight: 600;
  letter-spacing: .11em; text-transform: uppercase; color: var(--ink-3);
  border-bottom: 1px solid var(--rule); white-space: nowrap;
}
td { padding: 11px 12px; border-bottom: 1px solid var(--rule); vertical-align: top; }
tr:last-child td { border-bottom: 0; }
.num { text-align: right; font-family: ui-monospace, monospace; color: var(--ink-2); white-space: nowrap; }

/* The signature: an illuminated aspect. Colour carries state, never decoration. */
.aspect { display: flex; align-items: center; gap: 8px; white-space: nowrap; }
.lamp {
  width: 11px; height: 11px; border-radius: 50%; flex: none;
  box-shadow: 0 0 0 2.5px color-mix(in srgb, currentColor 18%, transparent);
}
.proceeding { color: var(--proceeding); }
.waiting    { color: var(--waiting); }
.stopped    { color: var(--stopped); }
.ended      { color: var(--ended); }
.lamp.proceeding { background: var(--proceeding); }
.lamp.waiting    { background: var(--waiting); }
.lamp.stopped    { background: var(--stopped); }
.lamp.ended      { background: var(--ended); }
.aspect b { font-size: 12px; font-weight: 600; }

.who { font-weight: 600; }
.meta { color: var(--ink-3); font-size: 11px; }
.areas { color: var(--ink-2); font-size: 12px; }
.areas span + span:before { content: " · "; color: var(--ink-3); }

/* An occupied section: a solid bar of track before the path. */
.section { display: flex; align-items: center; gap: 9px; }
.section:before {
  content: ""; width: 16px; height: 3px; background: var(--occupied); flex: none;
}
.files { margin: 0; padding: 0; list-style: none; }
.files li { display: flex; justify-content: space-between; gap: 16px; padding: 1px 0; }
.files li span:last-child { color: var(--ink-3); }

.empty { background: var(--panel); padding: 18px 14px; color: var(--ink-2); }
.empty code { color: var(--ink); }
.note {
  background: var(--panel); border-left: 3px solid var(--waiting);
  padding: 12px 14px; margin-top: 12px; color: var(--ink-2);
}
@media (max-width: 620px) {
  .hide-narrow { display: none; }
  .ind b { font-size: 24px; }
}
</style>
</head>
<body>
<div class="wrap">

<header class="head">
  <span class="mark">Interlock</span>
  <span class="repo mono">{{.Root}}</span>
  <span class="clock mono">{{.GeneratedAt}} · {{.RefreshSecs}}s</span>
</header>

<section class="indications">
  <div class="ind"><b>{{len .Sessions}}</b><span>Sessions</span></div>
  <div class="ind"><b>{{len .Leases}}</b><span>Files held</span></div>
  <div class="ind"><b>{{.Prevented}}</b><span>Collisions stopped</span></div>
  <div class="ind{{if .Escaped}} alert{{end}}"><b>{{.Escaped}}</b><span>Got through</span></div>
</section>

{{if .Warning}}<p class="note">{{.Warning}}</p>{{end}}

<h2>Agents</h2>
{{if .Sessions}}
<div class="scroll"><table>
  <tr>
    <th>State</th><th>Agent</th><th class="hide-narrow">Model</th>
    <th>Recent areas</th><th class="num">Holds</th>
    <th class="num">Last seen</th><th class="num hide-narrow">Expires</th>
    <th class="num hide-narrow">Observed for</th>
  </tr>
  {{range .Sessions}}
  <tr>
    <td><span class="aspect {{.Aspect}}"><i class="lamp {{.Aspect}}"></i><b>{{.Aspect.Label}}</b></span></td>
    <td>
      <div class="who">{{.Agent}}{{if .Team}} <span class="meta">{{.Team}}</span>{{end}}</div>
      <div class="meta mono">{{.ShortID}}</div>
    </td>
    <td class="hide-narrow meta mono">{{.Model}}</td>
    <td class="areas mono">
      {{range .Areas}}<span>{{.}}</span>{{else}}<span class="meta">nothing yet</span>{{end}}
      {{with .Latest}}<div class="meta mono">{{.Path}}</div>{{end}}
    </td>
    <td class="num">{{.Holds}}</td>
    <td class="num">{{.LastSeen}}</td>
    <td class="num hide-narrow">{{.Expires}}</td>
    <td class="num hide-narrow">{{.Started}}</td>
  </tr>
  {{end}}
</table></div>
{{else}}
<p class="empty">No agent is working here. Open Claude Code or Codex in this
repository and they will appear.</p>
{{end}}

<h2>Files held <em>claimed while an agent works on them, released when it moves on</em></h2>
{{if .Leases}}
<div class="scroll"><table>
  <tr><th>Section</th><th>Held by</th><th class="num">For</th><th class="num">Expires</th></tr>
  {{range .Leases}}
  <tr>
    <td><span class="section mono">{{.Path}}</span></td>
    <td>{{.Agent}}{{if .Team}} <span class="meta">{{.Team}}</span>{{end}} <span class="meta mono">{{.ShortID}}</span></td>
    <td class="num">{{.Held}}</td>
    <td class="num">{{.Expires}}</td>
  </tr>
  {{end}}
</table></div>
{{else}}
<p class="empty">Every file is clear.</p>
{{end}}

<h2>What was stopped</h2>
{{if .Events}}
<div class="scroll"><table>
  <tr><th class="num">Ago</th><th>What happened</th><th>File</th><th class="hide-narrow">Why</th></tr>
  {{range .Events}}
  <tr>
    <td class="num">{{.Ago}}</td>
    <td><span class="aspect {{if .Escaped}}waiting{{else}}stopped{{end}}"><i class="lamp {{if .Escaped}}waiting{{else}}stopped{{end}}"></i><b>{{.Label}}</b></span></td>
    <td class="mono">{{.Path}}</td>
    <td class="hide-narrow meta">{{.Agent}}{{if .Team}} on {{.Team}}{{end}}: {{.Detail}}</td>
  </tr>
  {{end}}
</table></div>
{{else}}
<p class="empty">Nothing has been stopped. Either the agents have stayed out of
each other's way, or the hooks are not wired up. Run <code>interlock doctor</code>
to tell the difference.</p>
{{end}}

{{if .Teams}}
<h2>Territory <em>declared rules, applied on top of the automatic behaviour</em></h2>
<div class="scroll"><table>
  <tr><th>Team</th><th>Owns</th><th class="hide-narrow">Shared</th><th class="num">Agents</th></tr>
  {{range .Teams}}
  <tr>
    <td class="who">{{.Name}}</td>
    <td class="areas mono">{{range .Owns}}<span>{{.}}</span>{{end}}</td>
    <td class="hide-narrow areas mono">{{range .Shared}}<span>{{.}}</span>{{end}}</td>
    <td class="num meta">{{range $i, $a := .Agents}}{{if $i}}, {{end}}{{$a}}{{end}}</td>
  </tr>
  {{end}}
</table></div>
{{end}}

</div>
</body>
</html>
`))
