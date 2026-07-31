package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/state"
)

func TestCleanupRechecksCrossProcessResume(t *testing.T) {
	if os.Getenv("INTERLOCK_SESSION_HELPER") == "resume" {
		store, err := Open(os.Getenv("INTERLOCK_SESSION_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Seen("shared-session", "codex", "", "", os.Getenv("INTERLOCK_SESSION_ROOT")); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	old := time.Now().Add(-IdleAfter - time.Minute)
	record := Session{
		Version: state.CurrentVersion, ID: "shared-session", Agent: "claude",
		Started: old, LastSeen: old, CWD: root,
	}
	if err := os.MkdirAll(store.dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(store.pathFor(record.ID), raw, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cutoff := time.Now().Add(-IdleAfter)
	stale, err := store.Stale(cutoff)
	if err != nil || len(stale) != 1 {
		t.Fatalf("Stale = %+v err=%v", stale, err)
	}

	child := exec.Command(os.Args[0], "-test.run=TestCleanupRechecksCrossProcessResume$")
	child.Env = append(os.Environ(),
		"INTERLOCK_SESSION_HELPER=resume",
		"INTERLOCK_SESSION_ROOT="+root,
	)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("resume child: %v\n%s", err, out)
	}

	cleaned := false
	seen, removed, err := store.CleanupStale(record.ID, cutoff, func() error {
		cleaned = true
		return nil
	})
	if err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if removed || cleaned {
		t.Fatal("cleanup removed a session resumed by another process")
	}
	if seen == nil || seen.Agent != "codex" {
		t.Fatalf("seen = %+v, want resumed Codex session", seen)
	}
}
