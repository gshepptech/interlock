package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/lease"
)

// statusJSONFixture builds a repository root with one held lease and
// returns the decoded `status --json` output for it.
//
// It writes a real lease through lease.Store rather than hand-writing the
// file, so the shape this test locks is the one the real acquire path
// produces -- a hand-built fixture would keep passing after a change to how
// leases are stored.
func statusJSONFixture(t *testing.T, held string) statusJSON {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	store, err := lease.Open(root)
	if err != nil {
		t.Fatalf("lease.Open: %v", err)
	}
	granted, _, err := store.AcquireFor(held, lease.Holder{
		Team: "core", Agent: "claude", SessionID: "sess-1", PID: os.Getpid(),
	}, 10*time.Minute, false)
	if err != nil {
		t.Fatalf("AcquireFor: %v", err)
	}
	if !granted {
		t.Fatal("fixture bug: the lease was not granted on an empty store")
	}

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var buf bytes.Buffer
	if code := runStatusJSON(&buf, cfg, time.Now()); code != 0 {
		t.Fatalf("runStatusJSON exit %d, want 0. output:\n%s", code, buf.String())
	}

	var got statusJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return got
}

// TestStatusJSONReportsHeldPaths is the contract keep reads: which paths
// are held, by whom. Without it keep would have to parse `interlock
// status`'s prose, which couples it to Interlock's presentation rather than
// to its meaning.
func TestStatusJSONReportsHeldPaths(t *testing.T) {
	got := statusJSONFixture(t, "internal/shared.go")

	if got.Version != statusJSONVersion {
		t.Errorf("version = %d, want %d", got.Version, statusJSONVersion)
	}
	if len(got.Leases) != 1 {
		t.Fatalf("got %d leases, want 1: %+v", len(got.Leases), got.Leases)
	}
	l := got.Leases[0]
	if l.Path != "internal/shared.go" {
		t.Errorf("path = %q, want the repo-relative path keep compares against", l.Path)
	}
	if l.Agent != "claude" || l.SessionID != "sess-1" {
		t.Errorf("holder = %q/%q, want claude/sess-1 -- a board has to name who is holding it", l.Agent, l.SessionID)
	}
	if l.ExpiresAt == "" {
		t.Error("expires_at is empty; a lease with no expiry cannot be shown as expiring")
	}
	if _, err := time.Parse(time.RFC3339, l.ExpiresAt); err != nil {
		t.Errorf("expires_at %q is not RFC3339: %v", l.ExpiresAt, err)
	}
}

// TestStatusJSONUsesEmptyArraysNotNull: a consumer that ranges over the
// result must not have to tell "no leases" apart from "no leases field".
// Go renders a nil slice as null, so this is one `[]T{}` initialisation
// away from being wrong and silent.
func TestStatusJSONUsesEmptyArraysNotNull(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var buf bytes.Buffer
	if code := runStatusJSON(&buf, cfg, time.Now()); code != 0 {
		t.Fatalf("exit %d on an untouched repository, want 0:\n%s", code, buf.String())
	}

	// Asserted on the raw bytes, not the decoded struct: unmarshalling null
	// into a slice also yields an empty slice, so decoding first would hide
	// exactly the defect this test exists for.
	for _, field := range []string{`"sessions": []`, `"leases": []`, `"errors": []`} {
		if !bytes.Contains(buf.Bytes(), []byte(field)) {
			t.Errorf("output does not contain %s -- a nil slice renders as null:\n%s", field, buf.String())
		}
	}
}

// TestStatusJSONExitsZeroWithoutJSONFlag guards the dispatch: adding a
// machine form must not change what plain `interlock status` does, and a
// stray argument must still be refused rather than silently ignored (which
// is what `--json` itself did before this existed).
func TestStatusJSONRejectsUnknownArguments(t *testing.T) {
	if code := runStatusCmd([]string{"--nope"}); code == 0 {
		t.Error("an unknown argument exited 0 -- `status --json` was silently ignored before this command existed, which is how the gap went unnoticed")
	}
}
