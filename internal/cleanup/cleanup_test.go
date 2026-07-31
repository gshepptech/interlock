package cleanup

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/state"
	"github.com/gshepptech/interlock/internal/verification"
)

func cleanupRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func ageSession(t *testing.T, root, id string, lastSeen time.Time) {
	t.Helper()
	dir := filepath.Join(root, ".interlock", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		var candidate session.Session
		if err := json.Unmarshal(raw, &candidate); err != nil || candidate.ID != id {
			continue
		}
		candidate.LastSeen = lastSeen
		raw, _ = json.Marshal(candidate)
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			t.Fatalf("age session: %v", err)
		}
		return
	}
	t.Fatalf("session %s not found", id)
}

func TestRunRemovesOnlyStaleSessionArtifacts(t *testing.T) {
	root := cleanupRepo(t)
	now := time.Now()
	sessions, _ := session.Open(root)
	leases, _ := lease.Open(root)
	remedies, _ := remedy.Open(root)
	pending := verification.Open(root)

	for _, id := range []string{"stale", "active"} {
		if err := sessions.Seen(id, "codex", "", "", root); err != nil {
			t.Fatalf("Seen %s: %v", id, err)
		}
		if _, _, err := leases.Acquire(id+".go", lease.Holder{
			SessionID: id, Agent: "codex",
		}); err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
		if err := remedies.Grant(id, []string{id + ".go"}); err != nil {
			t.Fatalf("Grant %s: %v", id, err)
		}
		if err := pending.Arm(id); err != nil {
			t.Fatalf("Arm %s: %v", id, err)
		}
	}
	ageSession(t, root, "stale", now.Add(-session.IdleAfter-time.Minute))

	result, err := (Service{Root: root, Now: func() time.Time { return now }}).Run(false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Sessions) != 1 || !result.Sessions[0].SessionRemoved ||
		result.Sessions[0].Outcome != Removed ||
		result.Sessions[0].LeasesReleased != 1 ||
		!result.Sessions[0].RemedyCleared || !result.Sessions[0].PendingRemoved {
		t.Fatalf("result = %#v", result)
	}
	if _, err := sessions.Get("stale"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale session remains: %v", err)
	}
	if _, err := sessions.Get("active"); err != nil {
		t.Errorf("active session removed: %v", err)
	}
	if !remedies.Allowed("active", "active.go") {
		t.Error("active remedy was removed")
	}
	if removed, err := pending.Remove("active"); err != nil || !removed {
		t.Errorf("active pending snapshot was removed: removed=%v err=%v", removed, err)
	}
}

func TestDryRunReportsWithoutRemoving(t *testing.T) {
	root := cleanupRepo(t)
	now := time.Now()
	sessions, _ := session.Open(root)
	if err := sessions.Seen("stale", "claude", "", "", root); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	ageSession(t, root, "stale", now.Add(-session.IdleAfter-time.Minute))

	result, err := (Service{Root: root, Now: func() time.Time { return now }}).Run(true)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].SessionRemoved {
		t.Fatalf("result = %#v", result)
	}
	if result.Sessions[0].Outcome != Pending {
		t.Fatalf("dry-run outcome = %q, want pending", result.Sessions[0].Outcome)
	}
	if _, err := sessions.Get("stale"); err != nil {
		t.Errorf("dry-run removed stale session: %v", err)
	}
}

func TestRunSessionTargetsOneStaleSessionAndClassifiesOtherStates(t *testing.T) {
	root := cleanupRepo(t)
	now := time.Now()
	sessions, _ := session.Open(root)
	for _, id := range []string{"stale-one", "stale-two", "active"} {
		if err := sessions.Seen(id, "codex", "", "", root); err != nil {
			t.Fatalf("Seen %s: %v", id, err)
		}
	}
	for _, id := range []string{"stale-one", "stale-two"} {
		ageSession(t, root, id, now.Add(-session.IdleAfter-time.Minute))
	}
	service := Service{Root: root, Now: func() time.Time { return now }}

	removed, err := service.RunSession("stale-one")
	if err != nil || removed.Outcome != Removed {
		t.Fatalf("remove stale-one = %#v err=%v", removed, err)
	}
	if _, err := sessions.Get("stale-two"); err != nil {
		t.Fatalf("targeted cleanup removed stale-two: %v", err)
	}
	active, err := service.RunSession("active")
	if err != nil || active.Outcome != SkippedActive {
		t.Fatalf("active result = %#v err=%v", active, err)
	}
	missing, err := service.RunSession("missing")
	if err != nil || missing.Outcome != NotFound {
		t.Fatalf("missing result = %#v err=%v", missing, err)
	}
}

func TestFutureArtifactKeepsSessionForRetry(t *testing.T) {
	root := cleanupRepo(t)
	now := time.Now()
	sessions, _ := session.Open(root)
	remedies, _ := remedy.Open(root)
	if err := sessions.Seen("stale", "claude", "", "", root); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := remedies.Grant("stale", []string{"a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	ageSession(t, root, "stale", now.Add(-session.IdleAfter-time.Minute))

	entries, err := os.ReadDir(filepath.Join(root, ".interlock", "remedy"))
	if err != nil {
		t.Fatalf("read remedy: %v", err)
	}
	file := ""
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".json" {
			file = filepath.Join(root, ".interlock", "remedy", entry.Name())
			break
		}
	}
	if file == "" {
		t.Fatal("remedy grant not found")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read grant: %v", err)
	}
	var grant map[string]any
	_ = json.Unmarshal(raw, &grant)
	grant["version"] = state.CurrentVersion + 1
	raw, _ = json.Marshal(grant)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatalf("write future grant: %v", err)
	}

	_, err = (Service{Root: root, Now: func() time.Time { return now }}).Run(false)
	var unsupported *state.UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Run error = %v, want UnsupportedVersionError", err)
	}
	if _, err := sessions.Get("stale"); err != nil {
		t.Errorf("session removed after partial cleanup failure: %v", err)
	}
}
