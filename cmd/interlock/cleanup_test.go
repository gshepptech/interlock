package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/cleanup"
	"github.com/gshepptech/interlock/internal/session"
)

func TestCleanupResultRenderingDistinguishesOutcomes(t *testing.T) {
	for _, test := range []struct {
		outcome cleanup.Outcome
		want    string
	}{
		{cleanup.Removed, "removed stale session"},
		{cleanup.SkippedActive, "kept session"},
		{cleanup.Failed, "failed to clean stale session"},
	} {
		var out bytes.Buffer
		printCleanupResult(&out, cleanup.SessionResult{
			ID: "s1", Agent: "codex", Outcome: test.outcome, Error: "broken",
		}, false)
		if !strings.Contains(out.String(), test.want) {
			t.Errorf("%s output = %q, want %q", test.outcome, out.String(), test.want)
		}
	}
}

func TestCleanupDryRunThenRemovesExpiredSessionAndLease(t *testing.T) {
	dir := bare(t)
	path := filepath.Join(dir, "src/api/users.go")
	if res := hook(t, dir, "PreToolUse", "claude", write("stale-session", path)); res.exit != 0 {
		t.Fatalf("hook failed: %s", res.stderr)
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
		_ = json.Unmarshal(raw, &doc)
		doc["last_seen"] = time.Now().Add(-session.IdleAfter - time.Minute)
		raw, _ = json.Marshal(doc)
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			t.Fatalf("age session: %v", err)
		}
	}

	dry := cli(t, dir, "cleanup", "--dry-run")
	if dry.exit != 0 || !strings.Contains(dry.stdout, "would remove stale session") {
		t.Fatalf("cleanup dry-run: stdout=%s stderr=%s", dry.stdout, dry.stderr)
	}
	if status := cli(t, dir, "status"); !strings.Contains(status.stdout, "src/api/users.go") {
		t.Fatal("dry-run removed the lease")
	}

	actual := cli(t, dir, "cleanup")
	if actual.exit != 0 || !strings.Contains(actual.stdout, "removed stale session") {
		t.Fatalf("cleanup: stdout=%s stderr=%s", actual.stdout, actual.stderr)
	}
	if status := cli(t, dir, "status"); strings.Contains(status.stdout, "src/api/users.go") {
		t.Fatal("cleanup left the stale lease")
	}
}

func TestCleanupRejectsUnpairedAndDuplicateTargetFlags(t *testing.T) {
	dir := bare(t)
	path := filepath.Join(dir, "src/api/users.go")
	if res := hook(t, dir, "PreToolUse", "claude", write("stale-session", path)); res.exit != 0 {
		t.Fatalf("hook failed: %s", res.stderr)
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
		_ = json.Unmarshal(raw, &doc)
		doc["last_seen"] = time.Now().Add(-session.IdleAfter - time.Minute)
		raw, _ = json.Marshal(doc)
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			t.Fatalf("age session: %v", err)
		}
	}
	for _, args := range [][]string{
		{"cleanup", "--confirm", "stale-session"},
		{"cleanup", "--session", "stale-session"},
		{"cleanup", "--session", "", "--confirm", "stale-session"},
		{"cleanup", "--session", "stale-session", "--confirm", ""},
		{"cleanup", "--session", "", "--confirm", ""},
		{"cleanup", "--session", "stale-session", "--session", "other", "--confirm", "stale-session"},
		{"cleanup", "--session", "stale-session", "--confirm", "stale-session", "--confirm", "other"},
	} {
		result := cli(t, dir, args...)
		if result.exit != exitUsage {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, result.exit, result.stdout, result.stderr)
		}
		if status := cli(t, dir, "status"); !strings.Contains(status.stdout, "src/api/users.go") {
			t.Fatalf("%v mutated state", args)
		}
	}
}
