package dashboard

import (
	"html/template"
	"io"

	"github.com/gshepptech/interlock/internal/diagnostic"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/runner"
)

type ControlView struct {
	View
	CSRF     string
	Nonce    string
	Flash    string
	Requests []RequestView
	Checks   []CheckView
	Runners  []string
}

type RequestView struct {
	ID      string
	Source  string
	Kind    string
	Target  string
	Note    string
	Created string
	Open    bool
}

type CheckView struct {
	Subject  string
	Severity string
	Detail   string
}

func Controls(view View, csrf, nonce, flash string, requests []handoffrequest.Item,
	report *diagnostic.Report) ControlView {
	out := ControlView{View: view, CSRF: csrf, Nonce: nonce, Flash: flash, Runners: Runners()}
	for _, item := range requests {
		out.Requests = append(out.Requests, RequestView{
			ID: item.Request.ID, Source: item.Request.SourceSession,
			Kind: string(item.Request.TargetKind), Target: item.Request.Target,
			Note: item.Request.Note, Created: item.Request.CreatedAt.Format("2006-01-02 15:04"),
			Open: item.Open(),
		})
	}
	if report != nil {
		for _, check := range report.Checks {
			out.Checks = append(out.Checks, CheckView{
				Subject: check.Subject, Severity: string(check.Severity), Detail: check.Detail,
			})
		}
	}
	return out
}

// RunLogView is a bounded piece of one run's output, on its way to a page.
type RunLogView struct {
	RunID     string
	Stream    string
	Text      string
	Truncated bool
}

// Runners are the agents the launcher offers. The list is the closed set the
// server validates against, so the form cannot offer something the service
// would refuse.
func Runners() []string {
	names := make([]string, 0, len(runner.Names()))
	for _, name := range runner.Names() {
		names = append(names, string(name))
	}
	return names
}

func RenderControlPage(w io.Writer, view ControlView) error {
	return controlTemplates.ExecuteTemplate(w, "page", view)
}

// RenderRunLog writes the log fragment. The text is escaped by the template,
// which is the last line of a defence that started when the bytes were read.
func RenderRunLog(w io.Writer, view RunLogView) error {
	return controlTemplates.ExecuteTemplate(w, "runlog", view)
}

func RenderControlStatus(w io.Writer, view ControlView) error {
	return controlTemplates.ExecuteTemplate(w, "status", view)
}

var controlTemplates = template.Must(template.New("control").Parse(`
{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<noscript><meta http-equiv="refresh" content="{{.RefreshSecs}}"></noscript>
<title>Interlock</title>
<link rel="icon" href="data:,">
<style nonce="{{.Nonce}}">
/* Shared design language with keep's board: same canvas, same hairline,
   same three semantic colours, same monospace-led type. The two tools are
   separate products that answer different questions, and a person running
   both should still read them as one surface. That is cheaper to achieve
   with one set of tokens than with a shared stylesheet neither repo could
   own. These values have to stay in step with keep's own
   internal/dash/style.css by hand; there is nothing enforcing it. */
:root {
  color-scheme: light dark;
  --bg: #fbfbfa;
  --panel: #ffffff;
  --line: #e6e4e0;
  --line-soft: #efedea;
  --fg: #16161a;
  --muted: #6b6b74;
  --faint: #96969f;
  --ok: #1a7f37;
  --warn: #9a6700;
  --bad: #b32428;
  --radius: 6px;
  --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0b0c0e;
    --panel: #111316;
    --line: #23262b;
    --line-soft: #1a1d21;
    --fg: #e8eaed;
    --muted: #8b9198;
    --faint: #5a6069;
    --ok: #3fb950;
    --warn: #d29922;
    --bad: #f85149;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font-family: var(--mono); font-size: 13px; line-height: 1.5;
  font-variant-numeric: tabular-nums; -webkit-font-smoothing: antialiased;
}
.wrap { max-width: 1180px; margin: auto; padding: 0 24px 64px; }
.mono, code { font-family: var(--mono); }

/* The header is a single line: product, place, time. No bar, no logo block --
   the page should start with state, not with branding. */
.head { display: flex; align-items: baseline; gap: 16px; padding: 18px 0 16px;
  border-bottom: 1px solid var(--line); }
.mark { font-weight: 600; letter-spacing: 0.06em; text-transform: uppercase; font-size: 12px; }
.repo { flex: 1; color: var(--muted); font-size: 12px; word-break: break-all; }
.clock, .meta { color: var(--faint); font-size: 11px; }

/* Counts read as a row of figures, not as cards. One hairline between them
   and no boxes: they are a readout, and a box around each would give four
   numbers the visual weight of four sections. */
.indications { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 1px; background: var(--line); border: 1px solid var(--line);
  border-radius: var(--radius); overflow: hidden; margin-top: 20px; }
.ind { padding: 14px 16px; background: var(--panel); }
.ind b { display: block; font-size: 26px; font-weight: 500; line-height: 1.1; letter-spacing: -0.02em; }
.ind span { display: block; margin-top: 5px; font-size: 10px; font-weight: 600;
  letter-spacing: 0.09em; text-transform: uppercase; color: var(--faint); }
.ind.alert b { color: var(--bad); }

h2 { margin: 34px 0 10px; padding: 0; font-size: 11px; font-weight: 600;
  letter-spacing: 0.09em; text-transform: uppercase; color: var(--faint); border: none; }
h2 em { font-style: normal; font-weight: 400; letter-spacing: 0.02em;
  text-transform: none; color: var(--faint); }

.scroll { overflow: auto; border: 1px solid var(--line); border-radius: var(--radius); }
table { width: 100%; border-collapse: collapse; background: var(--panel); }
th, td { text-align: left; padding: 9px 14px; vertical-align: top; }
th { font-size: 10.5px; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase;
  color: var(--faint); white-space: nowrap; background: var(--bg);
  border-bottom: 1px solid var(--line); }
td { border-top: 1px solid var(--line-soft); }
tr:first-child td { border-top: none; }
tbody tr:hover td, table tr:hover td { background: var(--line-soft); }
.num { text-align: right; white-space: nowrap; }

/* The score is the one number keep exists to produce, so it anchors its
   row here the same way it does on keep's own board. */
td.num.score, .queue td.num:first-child { font-size: 15px; font-weight: 600; color: var(--fg); }

/* State is a dot and a word, never a filled row. Colour is the second
   signal, so the page still reads with colour off. */
.state { display: inline-flex; align-items: center; gap: 7px; font-weight: 500; white-space: nowrap; }
.state:before { content: ""; width: 6px; height: 6px; border-radius: 50%;
  background: currentColor; flex: none; }
.proceeding, .ok { color: var(--ok); }
.waiting, .warn { color: var(--warn); }
.stopped, .fail { color: var(--bad); }
.ended { color: var(--faint); }

.empty, .flash { margin: 0; background: var(--panel); padding: 14px; color: var(--muted);
  border: 1px solid var(--line); border-radius: var(--radius); font-size: 12.5px; }
.flash { border-left: 2px solid var(--ok); margin-top: 12px; }

.actions { display: flex; flex-wrap: wrap; gap: 6px; justify-content: flex-end; }
form { margin: 0; }
button { border: 1px solid var(--line); background: var(--panel); color: var(--fg);
  padding: 5px 10px; border-radius: var(--radius); font: inherit; font-size: 12px; cursor: pointer; }
button:hover { border-color: var(--faint); background: var(--line-soft); }
button:focus-visible { outline: 2px solid var(--fg); outline-offset: 1px; }
button.danger { color: var(--bad); }
button.danger:hover { border-color: var(--bad); }
details { min-width: 180px; }
details form { display: grid; gap: 7px; padding: 9px 0; }
summary { cursor: pointer; color: var(--muted); }
summary:hover { color: var(--fg); }
input, select, textarea { width: 100%; border: 1px solid var(--line); background: var(--bg);
  color: var(--fg); padding: 7px 9px; border-radius: var(--radius); font: inherit; font-size: 12px; }
input:focus, select:focus, textarea:focus { outline: none; border-color: var(--faint); }
textarea { min-height: 58px; resize: vertical; }
.doctor { display: flex; flex-wrap: wrap; gap: 7px; padding: 12px; background: var(--panel);
  border: 1px solid var(--line); border-radius: var(--radius); }
.note { max-width: 420px; white-space: pre-wrap; }
.hide { display: none; }
a { color: var(--fg); text-underline-offset: 3px; text-decoration-color: var(--faint); }
a:hover { text-decoration-color: var(--fg); }
@media (max-width: 700px) {
  .wide { display: none; }
  .wrap { padding: 0 14px 40px; }
  th, td { padding: 8px 10px; }
  .actions { justify-content: flex-start; }
}
@media (prefers-reduced-motion: reduce) { * { transition: none !important; } }
.head{flex-wrap:wrap;gap:8px 18px}.repo{min-width:180px}.meta.mono{white-space:nowrap}@media(max-width:700px){.scroll table{min-width:620px}}
</style>
<noscript><style nonce="{{.Nonce}}">.control-only{display:none!important}</style></noscript>
<script nonce="{{.Nonce}}">
(() => {
  const refresh = async () => {
    if (document.hidden) return;
    try {
      const response = await fetch('/api/status', {headers:{'Accept':'text/html'}, cache:'no-store'});
      if (response.ok) document.getElementById('live-status').innerHTML = await response.text();
    } catch (_) {}
  };
  setInterval(refresh, {{.RefreshSecs}} * 1000);
  document.addEventListener('submit', async event => {
	const form = event.target;
    const message = event.target.dataset.confirm;
	if (message && !window.confirm(message)) { event.preventDefault(); return; }
	event.preventDefault();
	const response = await fetch(form.action, {
	  method: 'POST', body: new URLSearchParams(new FormData(form)),
	  headers: {'Content-Type': 'application/x-www-form-urlencoded'}, redirect: 'follow'
	});
	if (response.redirected) { window.location.assign(response.url); return; }
	window.alert((await response.text()).trim() || 'Action failed');
  });
})();
</script>
</head>
<body><div class="wrap">
<header class="head"><span class="mark">Interlock</span><span class="repo mono">{{.Root}}</span><span class="clock mono">{{.GeneratedAt}}</span></header>
<main id="live-status">{{template "status" .}}</main>
</div></body></html>{{end}}

{{define "status"}}
{{if .Flash}}<p class="flash" role="status">{{.Flash}}</p>{{end}}
<section class="indications">
  <div class="ind"><b>{{len .Sessions}}</b><span>Sessions</span></div>
  <div class="ind"><b>{{len .Runs}}</b><span>Runs</span></div>
  <div class="ind"><b>{{len .Leases}}</b><span>Files held</span></div>
  <div class="ind"><b>{{len .Requests}}</b><span>Handoffs</span></div>
  <div class="ind"><b>{{.Prevented}}</b><span>Collisions stopped</span></div>
</section>
{{if .Warning}}<p class="flash warn">{{.Warning}}</p>{{end}}

<h2>Runs</h2>
<div class="doctor control-only"><details><summary>Start an agent</summary>
<form method="post" action="/api/actions/run-start">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <select name="runner" aria-label="Runner">{{range .Runners}}<option value="{{.}}">{{.}}</option>{{end}}</select>
  <input name="name" maxlength="64" placeholder="Name (optional)">
  <input name="model" maxlength="64" placeholder="Model (optional)">
  <input name="team" maxlength="64" placeholder="Team (optional)">
  <input name="cwd" maxlength="512" placeholder="Directory (optional)">
  <textarea name="prompt" maxlength="32768" required placeholder="What should it do?"></textarea>
  <button type="submit">Start</button>
</form></details></div>
{{if .Runs}}<div class="scroll"><table>
<tr><th>State</th><th>Run</th><th class="wide">Directory</th><th class="num">Running for</th><th>Controls</th></tr>
{{range .Runs}}<tr>
  <td><span class="state {{if .Terminal}}ended{{else if .ForceRequired}}stopped{{else if .Paused}}waiting{{else}}proceeding{{end}}">{{.State}}</span>
    {{if .Summary}}<div class="meta">{{.Summary}}</div>{{end}}</td>
  <td><b>{{.Name}}</b> <span class="meta">{{.Runner}}</span><div class="meta mono">{{.ShortID}}</div></td>
  <td class="wide mono">{{.Dir}}{{if .Model}}<div class="meta">{{.Model}}</div>{{end}}</td>
  <td class="num">{{.Duration}}</td>
  <td><div class="actions control-only">
    {{if .Live}}
      {{if .Paused}}<form method="post" action="/api/actions/run-resume">
        <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.ID}}"><button type="submit">Resume</button>
      </form>{{else}}<form method="post" action="/api/actions/run-pause" data-confirm="Pause this agent and everything it started?">
        <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button type="submit">Pause</button>
      </form>{{end}}
      <form method="post" action="/api/actions/run-stop" data-confirm="Ask this agent to stop?">
        <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button type="submit">Stop</button>
      </form>
      {{if .ForceRequired}}<form method="post" action="/api/actions/run-force-stop" data-confirm="Kill this agent and everything it started? Work in progress may be lost.">
        <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button class="danger" type="submit">Force stop</button>
      </form>{{end}}
    {{else}}<form method="post" action="/api/actions/run-retry" data-confirm="Start a new run with the same instructions?">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button type="submit">Retry</button>
    </form>{{end}}
  </div></td>
</tr>{{end}}
</table></div>{{else}}<p class="empty">No agents started from here.</p>{{end}}

{{if .HasKeep}}
<h2>Next <em>from keep, already filtered against the files held below</em></h2>
{{if .Queue}}<div class="scroll"><table>
<tr><th class="num">Score</th><th>Item</th><th class="wide">Touches</th></tr>
{{range .Queue}}<tr>
  <td class="num mono score">{{.Score}}</td>
  <td><b>{{.Title}}</b><div class="meta mono">{{.ID}} · {{.Type}}</div></td>
  <td class="wide mono">{{if .Paths}}{{.Paths}}{{else}}<span class="meta">not declared</span>{{end}}</td>
</tr>{{end}}
</table></div>{{else}}<p class="empty">Nothing is workable right now. Either the queue is empty, or every item left touches a file someone is holding &mdash; <code>keep blocked</code> says which.</p>{{end}}
{{end}}

<h2>Agents</h2>
{{if .Sessions}}<div class="scroll"><table>
<tr><th>State</th><th>Agent</th><th class="wide">Recent area</th><th class="num">Holds</th><th class="num">Last seen</th><th>Controls</th></tr>
{{range .Sessions}}<tr>
  <td><span class="state {{.Aspect}}">{{.Aspect.Label}}</span><div class="meta">{{.Origin}}</div></td>
  <td><b>{{.Agent}}</b>{{if .Team}} <span class="meta">{{.Team}}</span>{{end}}<div class="meta mono">{{.ShortID}}</div></td>
  <td class="wide mono">{{range .Areas}}{{.}} {{else}}<span class="meta">nothing yet</span>{{end}}{{with .Latest}}<div class="meta">{{.Path}}</div>{{end}}</td>
  <td class="num">{{.Holds}}</td><td class="num">{{.LastSeen}}</td>
  <td><div class="actions control-only">
    {{if eq .Aspect "ended"}}<form method="post" action="/api/actions/session-clear" data-confirm="Remove this expired session and its state?">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="session_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button class="danger" type="submit">Clear</button>
    </form>{{end}}
    {{if .Holds}}<form method="post" action="/api/actions/lease-release" data-confirm="Release this session's file holds?">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="session_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button type="submit">Release holds</button>
    </form>{{end}}
    <details><summary>Request handoff</summary><form method="post" action="/api/actions/handoff-request">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="source_session_id" value="{{.ID}}">
      <select name="target_kind" aria-label="Target kind"><option value="agent">Agent</option><option value="team">Team</option></select>
      <input name="target" maxlength="64" required placeholder="Target"><textarea name="note" maxlength="1024" placeholder="Handoff note"></textarea><button type="submit">Request</button>
    </form></details>
  </div></td>
</tr>{{end}}
</table></div>{{else}}<p class="empty">No observed sessions.</p>{{end}}

<h2>Files held</h2>
{{if .Leases}}<div class="scroll"><table><tr><th>File</th><th>Held by</th><th class="num">Expires</th></tr>
{{range .Leases}}<tr><td class="mono">{{.Path}}</td><td>{{.Agent}} <span class="meta mono">{{.ShortID}}</span></td><td class="num">{{.Expires}}</td></tr>{{end}}
</table></div>{{else}}<p class="empty">Every file is clear.</p>{{end}}

<h2>Handoff requests</h2>
{{if .Requests}}<div class="scroll"><table><tr><th>Status</th><th>From</th><th>To</th><th>Note</th><th>Created</th><th></th></tr>
{{range .Requests}}<tr><td>{{if .Open}}open{{else}}resolved{{end}}</td><td class="mono">{{.Source}}</td><td>{{.Kind}}:{{.Target}}</td><td class="note">{{.Note}}</td><td class="num">{{.Created}}</td><td>
{{if .Open}}<form class="control-only" method="post" action="/api/actions/handoff-resolve" data-confirm="Resolve this handoff request?">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="request_id" value="{{.ID}}"><input type="hidden" name="confirmation" value="{{.ID}}"><button type="submit">Resolve</button></form>{{end}}</td></tr>{{end}}
</table></div>{{else}}<p class="empty">No handoff requests.</p>{{end}}

<h2>Installation</h2><div class="doctor control-only">
<form method="post" action="/api/actions/doctor-check"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Check</button></form>
<form method="post" action="/api/actions/doctor-smoke"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Verify hooks</button></form>
<form method="post" action="/api/actions/doctor-repair" data-confirm="Apply deterministic installation repairs?"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="confirmation" value="repair"><button type="submit">Repair</button></form>
</div>
{{if .Checks}}<div class="scroll"><table><tr><th>Result</th><th>Check</th><th>Detail</th></tr>{{range .Checks}}<tr><td class="{{.Severity}}">{{.Severity}}</td><td>{{.Subject}}</td><td class="meta">{{.Detail}}</td></tr>{{end}}</table></div>{{end}}

<h2>Activity</h2>
{{if .Events}}<div class="scroll"><table><tr><th class="num">Ago</th><th>Event</th><th>Target</th><th class="wide">Detail</th></tr>
{{range .Events}}<tr><td class="num">{{.Ago}}</td><td>{{.Label}}</td><td class="mono">{{.Path}}</td><td class="wide meta">{{.Agent}}: {{.Detail}}</td></tr>{{end}}</table></div>{{else}}<p class="empty">No coordination activity recorded.</p>{{end}}
{{end}}

{{define "runlog"}}<section class="runlog"><h3>{{.RunID}} {{.Stream}}</h3>
{{if .Truncated}}<p class="meta">Earlier output not shown.</p>{{end}}
<pre class="mono">{{.Text}}</pre></section>{{end}}
`))
