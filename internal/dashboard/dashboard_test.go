package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcoord/internal/config"
	"agentcoord/internal/lease"
	"agentcoord/internal/session"
)

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

const sample = `{
  "teams": {
    "backend":  {"owns": ["src/api/**"], "shared": ["src/types/**"], "agents": ["claude"]},
    "frontend": {"owns": ["src/components/**"], "agents": ["codex"]}
  }
}`

func TestBuildCountsActiveSessionsAndHeldFilesPerTeam(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	sessions := []*session.Session{
		{ID: "s1", Agent: "claude", Team: "backend", Started: now.Add(-5 * time.Minute), LastSeen: now},
		{ID: "s2", Agent: "codex", Team: "frontend", Started: now.Add(-time.Minute), LastSeen: now},
		{ID: "s3", Agent: "claude", Team: "backend", Started: now, LastSeen: now},
	}
	leases := []*lease.Lease{
		{Path: "src/types/api.ts", Team: "backend", Agent: "claude",
			SessionID: "s1", Acquired: now.Add(-90 * time.Second)},
	}

	v := Build(cfg, sessions, leases, now)

	byName := map[string]TeamView{}
	for _, tv := range v.Teams {
		byName[tv.Name] = tv
	}
	if got := byName["backend"].Active; got != 2 {
		t.Errorf("backend active = %d, want 2", got)
	}
	if got := byName["frontend"].Active; got != 1 {
		t.Errorf("frontend active = %d, want 1", got)
	}
	if got := byName["backend"].Holding; got != 1 {
		t.Errorf("backend holding = %d, want 1", got)
	}
	if got := byName["frontend"].Holding; got != 0 {
		t.Errorf("frontend holding = %d, want 0", got)
	}
	if len(v.Sessions) != 3 || len(v.Leases) != 1 {
		t.Errorf("sessions=%d leases=%d, want 3 and 1", len(v.Sessions), len(v.Leases))
	}
}

// A session quiet for a while is probably thinking. Showing it differently is
// how a stalled agent becomes visible.
func TestQuietSessionIsMarkedIdle(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	v := Build(cfg, []*session.Session{
		{ID: "busy", Agent: "claude", Team: "backend", Started: now, LastSeen: now},
		{ID: "quiet", Agent: "codex", Team: "frontend", Started: now,
			LastSeen: now.Add(-5 * time.Minute)},
	}, nil, now)

	byID := map[string]SessionView{}
	for _, s := range v.Sessions {
		byID[s.ShortID] = s
	}
	if byID["busy"].Idle {
		t.Error("an active session was marked idle")
	}
	if !byID["quiet"].Idle {
		t.Error("a session quiet for five minutes was not marked idle")
	}
}

// An empty repo must say why nothing is showing, not just look broken.
func TestNoSessionsProducesAnExplanation(t *testing.T) {
	cfg := loadConfig(t, sample)
	v := Build(cfg, nil, nil, time.Now())

	if v.Warning == "" {
		t.Error("no warning when nothing has run; an empty page reads as broken")
	}
	if !strings.Contains(v.Warning, "hooks") {
		t.Errorf("warning does not mention hooks, which is the likely cause: %q", v.Warning)
	}
}

func TestHandlerRendersTheState(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)
	view := Build(cfg,
		[]*session.Session{{
			ID: "abcdef123456", Agent: "claude", Team: "backend", Model: "opus",
			Started: now.Add(-time.Hour), LastSeen: now,
			Recent: []session.Touch{{Path: "src/api/users.go", Tool: "Write", At: now}},
		}},
		[]*lease.Lease{{
			Path: "src/types/api.ts", Team: "backend", Agent: "claude",
			SessionID: "abcdef123456", Acquired: now.Add(-time.Minute),
		}},
		now)

	rec := httptest.NewRecorder()
	Handler(func() (View, error) { return view, nil }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want html", ct)
	}
	// A live view that gets cached is a lying view.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"claude", "backend", "opus", "src/api/users.go", "src/types/api.ts",
		"abcdef12", "src/components/**", "codex",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// No script and no external fetch, so the page cannot phone home.
	for _, forbidden := range []string{"<script", "http://", "https://", "//cdn"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page contains %q, and it must be self-contained with no script", forbidden)
		}
	}
}

// Paths and model names come out of another tool's session files. They are not
// trusted input.
func TestRenderedStateIsEscaped(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)
	nasty := `<script>alert('x')</script>`

	view := Build(cfg,
		[]*session.Session{{
			ID: "s1", Agent: nasty, Team: "backend", Model: nasty,
			Started: now, LastSeen: now,
			Recent: []session.Touch{{Path: nasty, At: now}},
		}},
		[]*lease.Lease{{Path: nasty, Team: "backend", Agent: nasty,
			SessionID: "s1", Acquired: now}},
		now)

	rec := httptest.NewRecorder()
	Handler(func() (View, error) { return view, nil }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert") {
		t.Fatal("a script tag from session data reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the hostile string is missing entirely; it should be present but escaped")
	}
}

func TestHandlerReportsAReadFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(func() (View, error) { return View{}, os.ErrPermission }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so a broken state is visible", rec.Code)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(func() (View, error) { return View{}, nil }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../etc/passwd", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSinceIsReadable(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		then time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "30s"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-90 * time.Minute), "1h30m"},
		{time.Time{}, "unknown"},
	} {
		if got := since(now, tc.then); got != tc.want {
			t.Errorf("since(%v) = %q, want %q", tc.then, got, tc.want)
		}
	}
}
