package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runner"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/territory"
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

	v := Build(cfg, sessions, leases, nil, now)

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

// The aspect is the one thing a person scanning the page reads, so each of the
// states has to come from something real.
func TestAspectReflectsWhatEachAgentIsDoing(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	v := Build(cfg, []*session.Session{
		{ID: "busy", Agent: "claude", Team: "backend", Started: now, LastSeen: now},
		{ID: "recently-quiet", Agent: "codex", Team: "frontend", Started: now,
			LastSeen: now.Add(-time.Minute)},
		{ID: "quiet", Agent: "codex", Team: "frontend", Started: now,
			LastSeen: now.Add(-5 * time.Minute)},
		{ID: "ended", Agent: "claude", Team: "backend", Started: now,
			LastSeen: now.Add(-session.IdleAfter - time.Minute)},
		{ID: "hitwall", Agent: "codex", Team: "frontend", Started: now, LastSeen: now},
	}, nil, []journal.Entry{
		{At: now.Add(-10 * time.Second), Kind: journal.BlockedLease,
			SessionID: "hitwall", Path: "a.go"},
	}, now)

	byID := map[string]SessionView{}
	for _, s := range v.Sessions {
		byID[s.ShortID] = s
	}
	if got := byID["busy"].Aspect; got != Proceeding {
		t.Errorf("a just-seen session shows %q, want proceeding", got)
	}
	if got := byID["recently"].Aspect; got != Waiting {
		t.Errorf("a session quiet for one minute shows %q, want waiting", got)
	}
	if got := byID["quiet"].Aspect; got != Waiting {
		t.Errorf("a session quiet for five minutes shows %q, want waiting", got)
	}
	if got := byID["hitwall"].Aspect; got != Stopped {
		t.Errorf("a session blocked ten seconds ago shows %q, want stopped", got)
	}
	if got := byID["ended"].Aspect; got != Ended {
		t.Errorf("an expired observation shows %q, want ended", got)
	}
	// The words on the page stay plain, whatever the visual language is.
	if got := Stopped.Label(); got != "blocked" {
		t.Errorf("Stopped.Label() = %q, want the plain word", got)
	}
	if got := Proceeding.Label(); got != "recent" {
		t.Errorf("Proceeding.Label() = %q, want recency rather than certainty", got)
	}
	if got := Waiting.Label(); got != "quiet" {
		t.Errorf("Waiting.Label() = %q, want quiet rather than waiting", got)
	}
	if got := Ended.Label(); got != "ended" {
		t.Errorf("Ended.Label() = %q, want ended", got)
	}
	if !strings.Contains(byID["busy"].Expires, "in ") ||
		!strings.Contains(byID["ended"].Expires, "ago") {
		t.Errorf("expiry labels are not explicit: busy=%q ended=%q",
			byID["busy"].Expires, byID["ended"].Expires)
	}
}

// A block long past should not leave an agent looking stuck.
func TestAnOldBlockDoesNotKeepAnAgentStopped(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	v := Build(cfg,
		[]*session.Session{{ID: "s1", Agent: "claude", Started: now, LastSeen: now}},
		nil,
		[]journal.Entry{{At: now.Add(-30 * time.Minute), Kind: journal.BlockedLease,
			SessionID: "s1", Path: "a.go"}},
		now)

	if got := v.Sessions[0].Aspect; got != Proceeding {
		t.Errorf("aspect = %q after a half-hour-old block, want proceeding", got)
	}
}

// The directories an agent is in say which part of the repo is busy, which is
// more use at a glance than a list of files.
func TestWorkingAreasAreDeduplicatedDirectories(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	v := Build(cfg, []*session.Session{{
		ID: "s1", Agent: "claude", Started: now, LastSeen: now,
		Recent: []session.Touch{
			{Path: "src/api/users.go", At: now},
			{Path: "src/api/orders.go", At: now},
			{Path: "src/types/api.ts", At: now},
		},
	}}, nil, nil, now)

	areas := v.Sessions[0].Areas
	if len(areas) != 2 {
		t.Fatalf("areas = %v, want two distinct directories", areas)
	}
	if areas[0] != "src/api" || areas[1] != "src/types" {
		t.Errorf("areas = %v, want src/api then src/types", areas)
	}
}

// An empty repo must say why nothing is showing, not just look broken.
func TestNoSessionsProducesAnExplanation(t *testing.T) {
	cfg := loadConfig(t, sample)
	v := Build(cfg, nil, nil, nil, time.Now())

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
		[]journal.Entry{{
			At: now.Add(-30 * time.Second), Kind: journal.BlockedTerritory,
			Agent: "codex", Team: "frontend", Path: "src/api/orders.go",
			Detail: `belongs to "backend"`,
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
		"abcdef12", "src/components/**", "codex", "src/api/orders.go",
		"blocked, wrong territory", "Interlock",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// No external fetches, so the page cannot phone home. The inline script only
	// catches a throttled background tab back up to current local state.
	for _, forbidden := range []string{"src=", "href=\"http://", "http://", "https://", "//cdn", "fetch("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page contains %q, and it must be self-contained with no external fetches", forbidden)
		}
	}
	if !strings.Contains(body, "visibilitychange") {
		t.Error("dashboard does not refresh when a backgrounded tab becomes visible")
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
		[]journal.Entry{{At: now, Kind: journal.BlockedTerritory, Path: nasty,
			Agent: nasty, Team: "backend", Detail: nasty}},
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

// The tool is invisible when it works, so the page has to say what it prevented.
// A miss must be counted apart from a save, or the page flatters itself.
func TestPreventedAndEscapedAreCountedSeparately(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	v := Build(cfg, nil, nil, []journal.Entry{
		{At: now, Kind: journal.BlockedTerritory, Path: "a.go", Team: "backend"},
		{At: now, Kind: journal.BlockedLease, Path: "b.ts", Team: "frontend"},
		{At: now, Kind: journal.EscapedGuard, Path: "c.go", Team: "backend"},
	}, now)

	if v.Prevented != 2 {
		t.Errorf("Prevented = %d, want 2", v.Prevented)
	}
	if v.Escaped != 1 {
		t.Errorf("Escaped = %d, want 1", v.Escaped)
	}
	if len(v.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(v.Events))
	}
	for _, e := range v.Events {
		if e.Label == "" {
			t.Errorf("event %+v has no label, so it renders as nothing", e)
		}
	}
	if !v.Events[2].Escaped {
		t.Error("the escaped write is not marked, so it reads as a success")
	}
	if v.Events[0].Escaped {
		t.Error("a block is marked as escaped")
	}
}

// An empty feed is ambiguous: nothing collided, or nothing is wired up. The page
// has to say so rather than imply success.
func TestEmptyFeedExplainsTheAmbiguity(t *testing.T) {
	now := time.Now()
	cfg := loadConfig(t, sample)

	view := Build(cfg,
		[]*session.Session{{ID: "s1", Agent: "claude", Team: "backend",
			Started: now, LastSeen: now}},
		nil, nil, now)

	rec := httptest.NewRecorder()
	Handler(func() (View, error) { return view, nil }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "doctor") {
		t.Error("an empty feed does not point at doctor, so a broken install looks like a quiet one")
	}
}

// A session Interlock started and a session it merely noticed are different
// claims, and the page has to keep them apart: an observed agent can be watched
// and blocked but never paused or stopped, because Interlock does not own it.
func TestSessionsSayWhetherInterlockStartedThem(t *testing.T) {
	now := time.Now()
	cfg := &config.Config{Root: "/repo", Map: &territory.Map{}}
	sessions := []*session.Session{
		{ID: "observed-1", Agent: "claude", LastSeen: now},
		{ID: "managed-1", Agent: "codex", LastSeen: now, RunID: "run-0123456789abcdef01234567"},
	}
	view := Build(cfg, sessions, nil, nil, now)
	if len(view.Sessions) != 2 {
		t.Fatalf("sessions = %#v", view.Sessions)
	}
	if view.Sessions[0].Managed() || view.Sessions[0].Origin() != "observed" {
		t.Errorf("first session = %#v", view.Sessions[0])
	}
	if !view.Sessions[1].Managed() || view.Sessions[1].Origin() != "managed" {
		t.Errorf("second session = %#v", view.Sessions[1])
	}
}

func TestRunViewsReportTheRecordRatherThanInferAnything(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	code := 2
	records := []managedrun.Record{
		{
			Spec: managedrun.Spec{
				ID: "run-0123456789abcdef01234567", CreatedAt: now.Add(-2 * time.Minute),
				Options: runner.Options{Runner: runner.Codex, Name: "api", Model: "a-model"},
			},
			Status: managedrun.Status{State: managedrun.Paused, StartedAt: now.Add(-90 * time.Second)},
		},
		{
			Spec: managedrun.Spec{
				ID: "run-89abcdef0123456789abcdef", CreatedAt: now.Add(-time.Hour),
				Options: runner.Options{Runner: runner.Claude}, RetryOf: "run-0123456789abcdef01234567",
			},
			Status: managedrun.Status{
				State: managedrun.Failed, ExitCode: &code,
				StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-59 * time.Minute),
			},
		},
	}
	views := RunViews(records, now)
	if len(views) != 2 {
		t.Fatalf("views = %#v", views)
	}
	if !views[0].Live || !views[0].Paused || views[0].Terminal {
		t.Errorf("paused run = %#v", views[0])
	}
	if views[0].Name != "api" || views[0].Runner != "codex" || views[0].Dir != "." {
		t.Errorf("paused run = %#v", views[0])
	}
	if !views[1].Terminal || views[1].Live {
		t.Errorf("failed run = %#v", views[1])
	}
	if views[1].Describe != "failed with exit code 2" {
		t.Errorf("describe = %q", views[1].Describe)
	}
	if views[1].RetryOf != "run-0123456789abcdef01234567" {
		t.Errorf("retry of = %q", views[1].RetryOf)
	}
}
