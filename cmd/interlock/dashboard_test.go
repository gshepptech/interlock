package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/session"
)

func TestDashboardHTTPAcceptsLegacyAndCurrentStateButSkipsFutureState(t *testing.T) {
	dir := bare(t)
	store, _ := session.Open(dir)
	for _, id := range []string{"legacy", "current", "future"} {
		if err := store.Seen(id, "codex", "", "", dir); err != nil {
			t.Fatalf("Seen %s: %v", id, err)
		}
	}
	sessionDir := filepath.Join(dir, ".interlock", "sessions")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file := filepath.Join(sessionDir, entry.Name())
		raw, _ := os.ReadFile(file)
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		switch doc["id"] {
		case "legacy":
			delete(doc, "version")
		case "future":
			doc["version"] = float64(99)
		}
		raw, _ = json.Marshal(doc)
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			t.Fatalf("write version fixture: %v", err)
		}
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	sessions, err := readSessions(cfg)
	if err != nil {
		t.Fatalf("readSessions: %v", err)
	}
	view := dashboard.Build(cfg, sessions, nil, nil, time.Now())
	recorder := httptest.NewRecorder()
	dashboard.Handler(func() (dashboard.View, error) { return view, nil }).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "legacy") ||
		!strings.Contains(body, "current") || strings.Contains(body, "future") {
		t.Fatalf("dashboard version rendering code=%d body=%s", recorder.Code, body)
	}
}

func TestDashboardRefusesANonLoopbackAddress(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := cli(t, dir, "dashboard", "--addr", "0.0.0.0:7788")
	if res.exit == 0 {
		t.Fatal("the dashboard agreed to listen on 0.0.0.0")
	}
	if !strings.Contains(res.stderr, "loopback") {
		t.Errorf("refusal does not explain why: %s", res.stderr)
	}
}

func TestDashboardLoaderPinsItsStartupRoot(t *testing.T) {
	root := bare(t)
	nested := filepath.Join(root, "src", "api")
	cfg, err := config.Load(nested)
	if err != nil {
		t.Fatalf("initial config: %v", err)
	}
	load := dashboardLoad(cfg)
	if err := os.WriteFile(filepath.Join(nested, config.FileName), []byte(`{"teams":{}}`), 0o644); err != nil {
		t.Fatalf("write nearer config: %v", err)
	}
	reloaded, err := config.Load(nested)
	if err != nil || reloaded.Root == cfg.Root {
		t.Fatalf("fixture did not change config root: root=%q reloaded=%q err=%v", cfg.Root, reloaded.Root, err)
	}
	view, _, err := load()
	if err != nil || view.Root != cfg.Root {
		t.Fatalf("pinned load root=%q want=%q err=%v", view.Root, cfg.Root, err)
	}
}
