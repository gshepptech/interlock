package controlhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/session"
)

const testAuthority = "127.0.0.1:7717"

type fakeController struct{ calls int }

func (f *fakeController) done(action control.Action) (control.Result, error) {
	f.calls++
	return control.Result{Action: action, Outcome: control.Completed, Message: "done"}, nil
}

func (f *fakeController) ClearStaleSession(context.Context, control.Actor, string, string) (control.Result, error) {
	return f.done(control.CleanupSession)
}
func (f *fakeController) ReleaseSessionLeases(context.Context, control.Actor, string, string) (control.Result, error) {
	return f.done(control.ReleaseLeases)
}
func (f *fakeController) CheckInstallation(context.Context) (control.Result, error) {
	return f.done("")
}
func (f *fakeController) VerifyInstallation(context.Context, control.Actor) (control.Result, error) {
	return f.done(control.DoctorSmoke)
}
func (f *fakeController) RepairInstallation(context.Context, control.Actor, string, bool) (control.Result, error) {
	return f.done(control.DoctorRepair)
}
func (f *fakeController) RequestHandoff(context.Context, control.Actor, handoffrequest.Create) (control.Result, error) {
	return f.done(control.HandoffRequest)
}
func (f *fakeController) ResolveHandoff(context.Context, control.Actor, string, string) (control.Result, error) {
	return f.done(control.HandoffResolve)
}

func testServer(t *testing.T, controller Controller) *Server {
	t.Helper()
	server, err := New(controller, testAuthority, func() (dashboard.View, []handoffrequest.Item, error) {
		return dashboard.View{Root: "/repo", RefreshSecs: 3}, nil, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func request(method, path string, body url.Values) *http.Request {
	raw := ""
	if body != nil {
		raw = body.Encode()
	}
	r := httptest.NewRequest(method, "http://"+testAuthority+path, strings.NewReader(raw))
	r.Host = testAuthority
	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r
}

func validForm(server *Server) url.Values {
	return url.Values{"csrf": {server.CSRFToken()}, "session_id": {"s1"}, "confirmation": {"s1"}}
}

func TestPageAndFragmentRenderCompleteControlsWithSecurityHeaders(t *testing.T) {
	fake := &fakeController{}
	server := testServer(t, fake)
	server.load = func() (dashboard.View, []handoffrequest.Item, error) {
		view := dashboard.View{Root: "/repo", RefreshSecs: 3, Sessions: []dashboard.SessionView{{
			ID: "new-session", Agent: "codex", ShortID: "new-sess", Aspect: dashboard.Ended,
		}}}
		return view, nil, nil
	}
	for _, path := range []string{"/", "/api/status"} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		for _, want := range []string{
			`action="/api/actions/session-clear"`, `name="csrf"`,
			server.CSRFToken(), "new-session", "Request handoff",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s missing %q", path, want)
			}
		}
		for _, header := range []string{
			"Content-Security-Policy", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy",
		} {
			if recorder.Header().Get(header) == "" {
				t.Errorf("GET %s missing %s", path, header)
			}
		}
		if path == "/" && (!strings.Contains(body, `<noscript><style nonce=`) ||
			!strings.Contains(body, `.control-only{display:none!important}`)) {
			t.Fatal("full page does not hide mutation controls without JavaScript")
		}
	}
}

func TestEnvelopeRejectionsNeverInvokeController(t *testing.T) {
	tests := []struct {
		name   string
		build  func(*Server) *http.Request
		status int
	}{
		{"wrong host", func(s *Server) *http.Request {
			r := request(http.MethodPost, "/api/actions/lease-release", validForm(s))
			r.Host = "example.com:7717"
			return r
		}, 400},
		{"missing origin", func(s *Server) *http.Request {
			return request(http.MethodPost, "/api/actions/lease-release", validForm(s))
		}, 403},
		{"cross origin", func(s *Server) *http.Request {
			r := request(http.MethodPost, "/api/actions/lease-release", validForm(s))
			r.Header.Set("Origin", "https://example.com")
			return r
		}, 403},
		{"tokenless", func(s *Server) *http.Request {
			form := validForm(s)
			form.Del("csrf")
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 403},
		{"wrong token", func(s *Server) *http.Request {
			form := validForm(s)
			form.Set("csrf", "wrong")
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 403},
		{"non post", func(*Server) *http.Request { return request(http.MethodGet, "/api/actions/lease-release", nil) }, 405},
		{"wrong media", func(s *Server) *http.Request {
			r := request(http.MethodPost, "/api/actions/lease-release", validForm(s))
			r.Header.Set("Origin", "http://"+testAuthority)
			r.Header.Set("Content-Type", "application/json")
			return r
		}, 415},
		{"unknown field", func(s *Server) *http.Request {
			form := validForm(s)
			form.Set("command", "rm -rf")
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 400},
		{"invalid target", func(s *Server) *http.Request {
			form := validForm(s)
			form.Set("session_id", "bad id")
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 400},
		{"duplicate field", func(s *Server) *http.Request {
			form := validForm(s)
			form["session_id"] = []string{"s1", "s2"}
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 400},
		{"oversized", func(s *Server) *http.Request {
			form := validForm(s)
			form.Set("confirmation", strings.Repeat("x", maxBody))
			r := request(http.MethodPost, "/api/actions/lease-release", form)
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 413},
		{"unknown action", func(s *Server) *http.Request {
			r := request(http.MethodPost, "/api/actions/shell", validForm(s))
			r.Header.Set("Origin", "http://"+testAuthority)
			return r
		}, 404},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeController{}
			server := testServer(t, fake)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, test.build(server))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if fake.calls != 0 {
				t.Fatalf("controller invoked %d times", fake.calls)
			}
			if recorder.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("error response missing security headers")
			}
		})
	}
}

func TestValidActionInvokesControllerAndRedirects(t *testing.T) {
	fake := &fakeController{}
	server := testServer(t, fake)
	r := request(http.MethodPost, "/api/actions/lease-release", validForm(server))
	r.Header.Set("Origin", "http://"+testAuthority)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusSeeOther || fake.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, fake.calls, recorder.Body.String())
	}
	if recorder.Header().Get("Location") == "" || recorder.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("redirect headers = %#v", recorder.Header())
	}
}

func TestSafeMessageHidesInternalDetails(t *testing.T) {
	err := &control.Error{Kind: control.Internal, Err: errors.New("/Users/me/private/config.toml: permission denied")}
	result := control.Result{Message: err.Error()}
	if got := safeMessage(result, err); got != "control action failed" {
		t.Fatalf("safeMessage = %q", got)
	}
}

func TestFetchFollowDoesNotConsumeFlashBeforeNavigation(t *testing.T) {
	server := testServer(t, &fakeController{})
	r := request(http.MethodPost, "/api/actions/lease-release", validForm(server))
	r.Header.Set("Origin", "http://"+testAuthority)
	post := httptest.NewRecorder()
	server.ServeHTTP(post, r)
	location := post.Header().Get("Location")
	if post.Code != http.StatusSeeOther || location == "" {
		t.Fatalf("redirect = %d %q", post.Code, location)
	}

	get := func(mode string) string {
		recorder := httptest.NewRecorder()
		r := request(http.MethodGet, location, nil)
		if mode != "" {
			r.Header.Set("Sec-Fetch-Mode", mode)
		}
		server.ServeHTTP(recorder, r)
		return recorder.Body.String()
	}
	if body := get("cors"); !strings.Contains(body, "done") {
		t.Fatal("fetch-follow response did not contain flash")
	}
	if body := get("navigate"); !strings.Contains(body, "done") {
		t.Fatal("navigation did not receive flash")
	}
	if body := get("navigate"); strings.Contains(body, `role="status">done`) {
		t.Fatal("flash survived its visible navigation read")
	}
}

func TestFlashStoreIsBoundedPrunedAndOneRead(t *testing.T) {
	server := testServer(t, &fakeController{})
	now := time.Now()
	server.now = func() time.Time { return now }
	first := server.putFlash("first")
	if got := server.takeFlash(first); got != "first" || server.takeFlash(first) != "" {
		t.Fatalf("flash one-read behavior = %q", got)
	}
	for range maxFlashes {
		if id := server.putFlash("full"); id == "" {
			t.Fatal("fill flash returned no id")
		}
	}
	if id := server.putFlash("overflow"); id == "" {
		t.Fatal("full flash store did not evict an older value")
	}
	if len(server.flashes) != maxFlashes {
		t.Fatalf("flash store size = %d", len(server.flashes))
	}
	now = now.Add(flashExpiry + time.Second)
	if id := server.putFlash("after prune"); id == "" || len(server.flashes) != 1 {
		t.Fatalf("expired flashes were not pruned: id=%q size=%d", id, len(server.flashes))
	}
}

func TestCompletedActionRedirectsWhenFlashDeliveryFails(t *testing.T) {
	fake := &fakeController{}
	server := testServer(t, fake)
	server.newToken = func(int) (string, error) { return "", errors.New("entropy unavailable") }
	r := request(http.MethodPost, "/api/actions/lease-release", validForm(server))
	r.Header.Set("Origin", "http://"+testAuthority)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" || fake.calls != 1 {
		t.Fatalf("completed action = status %d location %q calls %d body %s",
			recorder.Code, recorder.Header().Get("Location"), fake.calls, recorder.Body.String())
	}
}

func TestCompletedActionEvictsFlashAtCapacity(t *testing.T) {
	fake := &fakeController{}
	server := testServer(t, fake)
	for range maxFlashes {
		if id := server.putFlash("older"); id == "" {
			t.Fatal("fill flash returned no id")
		}
	}
	r := request(http.MethodPost, "/api/actions/lease-release", validForm(server))
	r.Header.Set("Origin", "http://"+testAuthority)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusSeeOther || fake.calls != 1 || len(server.flashes) != maxFlashes {
		t.Fatalf("completed action = status %d calls %d flashes %d body %s",
			recorder.Code, fake.calls, len(server.flashes), recorder.Body.String())
	}
}

func TestStatusRefreshAddsControlsForNewSession(t *testing.T) {
	fake := &fakeController{}
	server := testServer(t, fake)
	sessions := []dashboard.SessionView{}
	server.load = func() (dashboard.View, []handoffrequest.Item, error) {
		return dashboard.View{Root: "/repo", RefreshSecs: 3, Sessions: sessions}, nil, nil
	}
	get := func() string {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request(http.MethodGet, "/api/status", nil))
		return recorder.Body.String()
	}
	if strings.Contains(get(), "late-session") {
		t.Fatal("late session appeared before fixture")
	}
	sessions = append(sessions, dashboard.SessionView{
		ID: "late-session", Agent: "claude", ShortID: "late-ses", Aspect: dashboard.Ended,
		Started: time.Now().Format(time.RFC3339),
	})
	body := get()
	if !strings.Contains(body, "late-session") || !strings.Contains(body, "/api/actions/session-clear") {
		t.Fatalf("refreshed controls missing: %s", body)
	}
}

func TestRealControllerStaleCleanupAndLeaseReleaseFlows(t *testing.T) {
	root := t.TempDir()
	sessions, _ := session.Open(root)
	leases, _ := lease.Open(root)
	now := time.Now()
	for _, id := range []string{"stale", "active"} {
		if err := sessions.Seen(id, "codex", "", "", root); err != nil {
			t.Fatalf("Seen %s: %v", id, err)
		}
		if granted, _, err := leases.Acquire(id+".go", lease.Holder{SessionID: id}); err != nil || !granted {
			t.Fatalf("Acquire %s: granted=%v err=%v", id, granted, err)
		}
	}
	dir := filepath.Join(root, ".interlock", "sessions")
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		raw, _ := os.ReadFile(file)
		var record map[string]any
		_ = json.Unmarshal(raw, &record)
		if record["id"] != "stale" {
			continue
		}
		record["last_seen"] = now.Add(-session.IdleAfter - time.Minute)
		raw, _ = json.Marshal(record)
		_ = os.WriteFile(file, raw, 0o644)
	}
	controller := control.Service{Root: root, Now: func() time.Time { return now }}
	server := testServer(t, controller)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", server.CSRFToken())
		r := request(http.MethodPost, path, form)
		r.Header.Set("Origin", "http://"+testAuthority)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, r)
		return recorder
	}

	cleared := post("/api/actions/session-clear", url.Values{
		"session_id": {"stale"}, "confirmation": {"stale"},
	})
	if cleared.Code != http.StatusSeeOther {
		t.Fatalf("clear status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if _, err := sessions.Get("stale"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale session remains: %v", err)
	}
	activeLeases, _ := leases.Active()
	if len(activeLeases) != 1 || activeLeases[0].SessionID != "active" {
		t.Fatalf("leases after clear = %#v", activeLeases)
	}

	released := post("/api/actions/lease-release", url.Values{
		"session_id": {"active"}, "confirmation": {"active"},
	})
	if released.Code != http.StatusSeeOther {
		t.Fatalf("release status=%d body=%s", released.Code, released.Body.String())
	}
	if _, err := sessions.Get("active"); err != nil {
		t.Fatalf("lease release removed active session: %v", err)
	}
	activeLeases, _ = leases.Active()
	if len(activeLeases) != 0 {
		t.Fatalf("lease release left %#v", activeLeases)
	}
}
