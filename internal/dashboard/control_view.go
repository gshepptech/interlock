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
:root{--panel:#fff;--deck:#e8ebed;--ink:#14181b;--muted:#5b666d;--faint:#8b959b;--rule:#ccd3d7;--hard:#aab4ba;--ok:#1c7a4b;--warn:#a56700;--bad:#bd1f2d}
@media(prefers-color-scheme:dark){:root{--panel:#1b2024;--deck:#111518;--ink:#e6ebee;--muted:#98a4ab;--faint:#6d787f;--rule:#2b3238;--hard:#3d464d;--ok:#43b47c;--warn:#d99a2b;--bad:#e0616c}}
*{box-sizing:border-box;letter-spacing:0}body{margin:0;background:var(--deck);color:var(--ink);font:13px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;font-variant-numeric:tabular-nums}.wrap{max-width:1220px;margin:auto;padding:0 16px 52px}.mono,code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.head{display:flex;align-items:baseline;gap:18px;padding:18px 0 14px;border-bottom:2px solid var(--hard)}.mark{font-weight:750;text-transform:uppercase}.repo{flex:1;color:var(--muted);font-size:12px;word-break:break-all}.clock,.meta{color:var(--faint);font-size:11px}.indications{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));background:var(--panel);border-bottom:1px solid var(--rule)}.ind{padding:13px 15px;border-right:1px solid var(--rule)}.ind b{display:block;font:500 28px/1 ui-monospace,monospace}.ind span,h2{font-size:10px;font-weight:700;text-transform:uppercase;color:var(--faint)}h2{margin:28px 0 0;padding:0 0 7px;border-bottom:1px solid var(--rule)}.scroll{overflow:auto}table{width:100%;border-collapse:collapse;background:var(--panel)}th,td{text-align:left;padding:9px 11px;border-bottom:1px solid var(--rule);vertical-align:top}th{font-size:10px;text-transform:uppercase;color:var(--faint);white-space:nowrap}.num{text-align:right;white-space:nowrap}.state{display:inline-flex;align-items:center;gap:7px;font-weight:650}.state:before{content:"";width:9px;height:9px;border-radius:50%;background:currentColor}.proceeding,.ok{color:var(--ok)}.waiting,.warn{color:var(--warn)}.stopped,.fail{color:var(--bad)}.ended{color:var(--faint)}.empty,.flash{margin:0;background:var(--panel);padding:14px;color:var(--muted)}.flash{border-left:3px solid var(--ok);margin-top:12px}.actions{display:flex;flex-wrap:wrap;gap:5px;justify-content:flex-end}form{margin:0}button{border:1px solid var(--hard);background:var(--panel);color:var(--ink);padding:5px 8px;border-radius:4px;font:inherit;cursor:pointer}button:hover{border-color:var(--ink)}button.danger{color:var(--bad)}details{min-width:180px}details form{display:grid;gap:6px;padding:8px 0}input,select,textarea{width:100%;border:1px solid var(--hard);background:var(--panel);color:var(--ink);padding:6px;border-radius:3px;font:inherit}textarea{min-height:58px;resize:vertical}.doctor{display:flex;flex-wrap:wrap;gap:7px;padding:10px;background:var(--panel)}.note{max-width:420px;white-space:pre-wrap}.hide{display:none}@media(max-width:700px){.wide{display:none}.wrap{padding:0 8px 36px}th,td{padding:8px}.actions{justify-content:flex-start}}
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
