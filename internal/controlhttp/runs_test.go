package controlhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runcontrol"
)

const testRunID = "run-0123456789abcdef01234567"

type fakeRuns struct {
	calls  []string
	launch runcontrol.Start
	logs   managedrun.LogChunk
}

func (f *fakeRuns) done(name string) (runcontrol.Result, error) {
	f.calls = append(f.calls, name)
	return runcontrol.Result{Outcome: control.Completed, Message: "done"}, nil
}

func (f *fakeRuns) Launch(_ context.Context, _ control.Actor, input runcontrol.Start) (runcontrol.Result, error) {
	f.launch = input
	return f.done("launch")
}
func (f *fakeRuns) Pause(context.Context, control.Actor, string, string) (runcontrol.Result, error) {
	return f.done("pause")
}
func (f *fakeRuns) Resume(context.Context, control.Actor, string) (runcontrol.Result, error) {
	return f.done("resume")
}
func (f *fakeRuns) Stop(context.Context, control.Actor, string, string) (runcontrol.Result, error) {
	return f.done("stop")
}
func (f *fakeRuns) ForceStop(context.Context, control.Actor, string, string) (runcontrol.Result, error) {
	return f.done("force")
}
func (f *fakeRuns) Retry(context.Context, control.Actor, string, string) (runcontrol.Result, error) {
	return f.done("retry")
}
func (f *fakeRuns) Logs(context.Context, string, managedrun.Stream, managedrun.Cursor, int64) (
	managedrun.LogChunk, managedrun.Cursor, error) {
	f.calls = append(f.calls, "logs")
	return f.logs, managedrun.Cursor{}, nil
}

func runServer(t *testing.T) (*Server, *fakeRuns) {
	t.Helper()
	server := testServer(t, &fakeController{})
	runs := &fakeRuns{}
	server.AddRuns(runs)
	return server, runs
}

func post(t *testing.T, server *Server, path string, body url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := request(http.MethodPost, path, body)
	r.Header.Set("Origin", "http://"+testAuthority)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestRunActionsReachTheServiceAndRedirect(t *testing.T) {
	cases := []struct {
		path string
		body url.Values
		want string
	}{
		{"/api/actions/run-pause",
			url.Values{"run_id": {testRunID}, "confirmation": {testRunID}}, "pause"},
		{"/api/actions/run-resume", url.Values{"run_id": {testRunID}}, "resume"},
		{"/api/actions/run-stop",
			url.Values{"run_id": {testRunID}, "confirmation": {testRunID}}, "stop"},
		{"/api/actions/run-force-stop",
			url.Values{"run_id": {testRunID}, "confirmation": {testRunID}}, "force"},
		{"/api/actions/run-retry",
			url.Values{"run_id": {testRunID}, "confirmation": {testRunID}}, "retry"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			server, runs := runServer(t)
			body := tc.body
			body.Set("csrf", server.CSRFToken())
			w := post(t, server, tc.path, body)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if len(runs.calls) != 1 || runs.calls[0] != tc.want {
				t.Fatalf("calls = %v", runs.calls)
			}
		})
	}
}

func TestLaunchAcceptsOnlyTheFieldsItNeeds(t *testing.T) {
	server, runs := runServer(t)
	body := url.Values{
		"csrf": {server.CSRFToken()}, "runner": {"codex"}, "name": {"api"},
		"model": {"a-model"}, "team": {"backend"}, "cwd": {"server/api"},
		"prompt": {"Implement the invitation API."},
	}
	if w := post(t, server, "/api/actions/run-start", body); w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if runs.launch.Options.Runner != "codex" || runs.launch.Options.Name != "api" ||
		runs.launch.Options.WorkingDir != "server/api" {
		t.Fatalf("launch = %#v", runs.launch)
	}
	if runs.launch.Prompt != "Implement the invitation API." {
		t.Fatalf("prompt = %q", runs.launch.Prompt)
	}
	// Nothing on this surface may name a command, an executable, or an
	// environment value. A field the form does not define is refused outright.
	for _, extra := range []string{"env", "args", "command", "executable", "interactive"} {
		rejected := url.Values{"csrf": {server.CSRFToken()}, "runner": {"codex"},
			"prompt": {"go"}, extra: {"anything"}}
		if w := post(t, server, "/api/actions/run-start", rejected); w.Code != http.StatusBadRequest {
			t.Errorf("field %q was accepted (status %d)", extra, w.Code)
		}
	}
}

func TestRunActionsRefuseMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		path string
		body url.Values
	}{
		{"unknown runner", "/api/actions/run-start",
			url.Values{"runner": {"gemini"}, "prompt": {"go"}}},
		{"no prompt", "/api/actions/run-start", url.Values{"runner": {"codex"}, "prompt": {"  "}}},
		{"climbing directory", "/api/actions/run-start",
			url.Values{"runner": {"codex"}, "prompt": {"go"}, "cwd": {"../etc"}}},
		{"absolute directory", "/api/actions/run-start",
			url.Values{"runner": {"codex"}, "prompt": {"go"}, "cwd": {"/etc"}}},
		{"name with a space", "/api/actions/run-start",
			url.Values{"runner": {"codex"}, "prompt": {"go"}, "name": {"the api"}}},
		{"invalid run id", "/api/actions/run-stop",
			url.Values{"run_id": {"../escape"}, "confirmation": {"../escape"}}},
		{"short run id", "/api/actions/run-pause",
			url.Values{"run_id": {"run-1"}, "confirmation": {"run-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, runs := runServer(t)
			body := tc.body
			body.Set("csrf", server.CSRFToken())
			if w := post(t, server, tc.path, body); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if len(runs.calls) != 0 {
				t.Fatalf("a malformed request reached the service: %v", runs.calls)
			}
		})
	}
}

// The prompt is the one field with a larger budget, and it is still bounded.
func TestLaunchBoundsThePromptAndTheBody(t *testing.T) {
	server, runs := runServer(t)
	body := url.Values{"csrf": {server.CSRFToken()}, "runner": {"codex"},
		"prompt": {strings.Repeat("a", maxPrompt+1)}}
	if w := post(t, server, "/api/actions/run-start", body); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized prompt returned %d", w.Code)
	}
	huge := url.Values{"csrf": {server.CSRFToken()}, "runner": {"codex"},
		"prompt": {strings.Repeat("a", maxStartBody)}}
	if w := post(t, server, "/api/actions/run-start", huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body returned %d", w.Code)
	}
	if len(runs.calls) != 0 {
		t.Fatalf("an oversized request reached the service: %v", runs.calls)
	}
}

func TestRunActionsKeepTheExistingEnvelope(t *testing.T) {
	server, runs := runServer(t)
	valid := url.Values{"csrf": {server.CSRFToken()}, "run_id": {testRunID},
		"confirmation": {testRunID}}

	t.Run("cross origin", func(t *testing.T) {
		r := request(http.MethodPost, "/api/actions/run-stop", valid)
		r.Header.Set("Origin", "http://evil.example")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d", w.Code)
		}
	})
	t.Run("no token", func(t *testing.T) {
		body := url.Values{"run_id": {testRunID}, "confirmation": {testRunID}}
		if w := post(t, server, "/api/actions/run-stop", body); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d", w.Code)
		}
	})
	t.Run("wrong method", func(t *testing.T) {
		r := request(http.MethodGet, "/api/actions/run-stop", nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", w.Code)
		}
	})
	t.Run("foreign host", func(t *testing.T) {
		r := request(http.MethodPost, "/api/actions/run-stop", valid)
		r.Header.Set("Origin", "http://"+testAuthority)
		r.Host = "interlock.example"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", w.Code)
		}
	})
	if len(runs.calls) != 0 {
		t.Fatalf("a refused request reached the service: %v", runs.calls)
	}
}

func TestLogsAreBoundedAndEscaped(t *testing.T) {
	server, runs := runServer(t)
	runs.logs = managedrun.LogChunk{
		Bytes:     []byte("<script>alert(1)</script>\nplain"),
		Truncated: true,
	}
	r := request(http.MethodGet, "/api/runs/"+testRunID+"/logs?stream=stdout&bytes=100", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("agent output reached the page unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "Earlier output not shown") {
		t.Fatalf("log fragment = %s", body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}

	for _, path := range []string{
		"/api/runs/../../etc/logs",
		"/api/runs/run-1/logs",
		"/api/runs/" + testRunID + "/logs?stream=prompt",
		"/api/runs/" + testRunID + "/logs?bytes=0",
		"/api/runs/" + testRunID + "/logs?other=1",
		"/api/runs/" + testRunID + "/prompt",
	} {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, request(http.MethodGet, path, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%s was served", path)
		}
	}
}

// A repository where nothing was ever launched should not offer controls for
// processes that do not exist.
func TestRunRoutesDoNotExistWithoutARunController(t *testing.T) {
	server := testServer(t, &fakeController{})
	body := url.Values{"csrf": {server.CSRFToken()}, "run_id": {testRunID},
		"confirmation": {testRunID}}
	if w := post(t, server, "/api/actions/run-stop", body); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request(http.MethodGet, "/api/runs/"+testRunID+"/logs", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("log status = %d", w.Code)
	}
}

func TestThePageOffersControlsThatMatchTheRunState(t *testing.T) {
	server, _ := runServer(t)
	server.load = func() (dashboard.View, []handoffrequest.Item, error) {
		return dashboard.View{Root: "/repo", RefreshSecs: 3, Runs: []dashboard.RunView{
			{ID: testRunID, ShortID: "run-0123", State: "running", Runner: "codex",
				Name: "api", Live: true},
			{ID: "run-89abcdef0123456789abcdef", ShortID: "run-89ab", State: "stopping",
				Runner: "claude", Name: "web", Live: true, Stopping: true, ForceRequired: true},
			{ID: "run-fedcba98765432100fedcba9", ShortID: "run-fedc", State: "failed",
				Runner: "codex", Name: "old", Terminal: true},
		}}, nil, nil
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request(http.MethodGet, "/", nil))
	page := w.Body.String()

	for _, want := range []string{
		"/api/actions/run-start", "/api/actions/run-pause", "/api/actions/run-stop",
		"/api/actions/run-force-stop", "/api/actions/run-retry", testRunID,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Force is offered only where a graceful stop has already been refused, and
	// retry only where there is nothing left running.
	if strings.Count(page, "/api/actions/run-force-stop") != 1 {
		t.Error("force stop is offered for a run that has not ignored a stop")
	}
	if strings.Count(page, "/api/actions/run-retry") != 1 {
		t.Error("retry is offered for a run that is still going")
	}
}
