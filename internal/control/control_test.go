package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/session"
)

func controlRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	return root
}

func service(t *testing.T) Service {
	t.Helper()
	root := controlRepo(t)
	return Service{
		Root: root, Executable: "/bin/interlock",
		CodexFile: filepath.Join(root, "home", ".codex", "config.toml"),
	}
}

func latestAudit(t *testing.T, root string) journal.Entry {
	t.Helper()
	store, _ := journal.Open(root)
	entries, err := store.Recent(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Recent audit = %#v err=%v", entries, err)
	}
	return entries[0]
}

func TestConfirmationIsEnforcedAndAuditedInsideService(t *testing.T) {
	service := service(t)
	result, err := service.ReleaseSessionLeases(
		context.Background(), ActorDashboard, "session-1", "wrong")
	if err == nil || KindOf(err) != Invalid || result.Outcome != RejectedConfirmation {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	audit := latestAudit(t, service.Root)
	if audit.Actor != ActorDashboard || audit.Action != ReleaseLeases ||
		audit.Target != "session-1" || audit.Result != RejectedConfirmation {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestReleaseLeasesIsIdempotentAndCannotTouchAnotherSession(t *testing.T) {
	service := service(t)
	store, _ := lease.Open(service.Root)
	for _, id := range []string{"s1", "s2"} {
		if granted, _, err := store.Acquire(id+".go", lease.Holder{SessionID: id}); err != nil || !granted {
			t.Fatalf("Acquire %s: granted=%v err=%v", id, granted, err)
		}
	}
	result, err := service.ReleaseSessionLeases(context.Background(), ActorCLI, "s1", "s1")
	if err != nil || result.Outcome != Completed {
		t.Fatalf("Release = %#v err=%v", result, err)
	}
	active, _ := store.Active()
	if len(active) != 1 || active[0].SessionID != "s2" {
		t.Fatalf("active leases = %#v", active)
	}
	result, err = service.ReleaseSessionLeases(context.Background(), ActorCLI, "s1", "s1")
	if err != nil || result.Outcome != NoChange {
		t.Fatalf("idempotent Release = %#v err=%v", result, err)
	}
}

func TestCompletedMutationWithAuditFailureReturnsSuccessWarning(t *testing.T) {
	service := service(t)
	service.AppendAudit = func(journal.Entry) error { return errors.New("disk full") }
	result, err := service.ReleaseSessionLeases(context.Background(), ActorCLI, "s1", "s1")
	if err != nil || result.Outcome != CompletedAuditWarning ||
		!strings.Contains(result.Warning, "disk full") || !result.Successful() {
		t.Fatalf("result = %#v err=%v", result, err)
	}
}

func TestCheckInstallationStaysReadOnlyAndSmokeIsAudited(t *testing.T) {
	service := service(t)
	result, err := service.CheckInstallation(context.Background())
	if err != nil || result.Diagnostic == nil || result.Outcome != Completed {
		t.Fatalf("CheckInstallation = %#v err=%v", result, err)
	}
	if !result.Diagnostic.HasFailures() || !result.Successful() {
		t.Fatalf("unhealthy read-only check should remain a completed query: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(service.Root, ".interlock")); !os.IsNotExist(err) {
		t.Fatalf("read-only check created state: %v", err)
	}
	result, err = service.VerifyInstallation(context.Background(), ActorCLI)
	if err != nil || result.Action != DoctorSmoke {
		t.Fatalf("VerifyInstallation = %#v err=%v", result, err)
	}
	audit := latestAudit(t, service.Root)
	if audit.Action != DoctorSmoke || audit.Actor != ActorCLI {
		t.Fatalf("smoke audit = %#v", audit)
	}
}

func TestHandoffCreateDeduplicateAndResolveUseSharedService(t *testing.T) {
	service := service(t)
	input := handoffrequest.Create{
		SourceSession: "s1", TargetKind: handoffrequest.Agent,
		Target: "codex", Note: "continue this",
	}
	created, err := service.RequestHandoff(context.Background(), ActorDashboard, input)
	if err != nil || created.Outcome != Completed || created.Handoff == nil {
		t.Fatalf("RequestHandoff = %#v err=%v", created, err)
	}
	duplicate, err := service.RequestHandoff(context.Background(), ActorDashboard, input)
	if err != nil || duplicate.Outcome != NoChange ||
		duplicate.Handoff.Request.ID != created.Handoff.Request.ID {
		t.Fatalf("duplicate = %#v err=%v", duplicate, err)
	}
	id := created.Handoff.Request.ID
	rejected, err := service.ResolveHandoff(context.Background(), ActorCLI, id, "wrong")
	if err == nil || rejected.Outcome != RejectedConfirmation {
		t.Fatalf("rejected resolve = %#v err=%v", rejected, err)
	}
	resolved, err := service.ResolveHandoff(context.Background(), ActorCLI, id, id)
	if err != nil || resolved.Outcome != Completed || resolved.Handoff.Open() {
		t.Fatalf("resolved = %#v err=%v", resolved, err)
	}
}

func TestRepairWithSmokeRejectsPinnedRootDriftBeforeMutation(t *testing.T) {
	parent := controlRepo(t)
	root := filepath.Join(parent, "nested")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	declaration := filepath.Join(root, config.FileName)
	if err := os.WriteFile(declaration, []byte(`{"teams":{}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := os.Remove(declaration); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	service := Service{
		Root: root, Config: cfg, Executable: "/bin/interlock",
		CodexFile: filepath.Join(parent, "home", ".codex", "config.toml"),
	}
	result, err := service.RepairInstallation(context.Background(), ActorDashboard, "repair", true)
	if KindOf(err) != Internal || result.Outcome != Failed || len(result.Repairs) != 0 {
		t.Fatalf("RepairInstallation = %#v err=%v kind=%s", result, err, KindOf(err))
	}
	for _, path := range []string{
		filepath.Join(parent, ".claude"), filepath.Join(parent, ".codex"), filepath.Join(parent, ".interlock"),
		filepath.Join(root, ".claude"), filepath.Join(root, ".codex"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("drifted repair created %s: %v", path, statErr)
		}
	}
}

func TestHandoffStorageFailureIsInternalAndSafe(t *testing.T) {
	service := service(t)
	blockingPath := filepath.Join(service.Root, ".interlock", "handoffs")
	if err := os.MkdirAll(filepath.Dir(blockingPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	result, err := service.RequestHandoff(context.Background(), ActorDashboard, handoffrequest.Create{
		SourceSession: "s1", TargetKind: handoffrequest.Agent, Target: "codex",
	})
	if KindOf(err) != Internal || result.Outcome != Failed {
		t.Fatalf("RequestHandoff = %#v err=%v kind=%s", result, err, KindOf(err))
	}
	if strings.Contains(result.Message, service.Root) || result.Message != "handoff request could not be stored" {
		t.Fatalf("unsafe result message %q", result.Message)
	}
}

func TestClearAllProducesOneAggregateAudit(t *testing.T) {
	service := service(t)
	now := time.Now()
	service.Now = func() time.Time { return now }
	store, _ := session.Open(service.Root)
	for _, id := range []string{"stale-1", "stale-2"} {
		if err := store.Seen(id, "codex", "", "", service.Root); err != nil {
			t.Fatalf("Seen: %v", err)
		}
	}
	dir := filepath.Join(service.Root, ".interlock", "sessions")
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		raw, _ := os.ReadFile(file)
		var record map[string]any
		_ = json.Unmarshal(raw, &record)
		record["last_seen"] = now.Add(-session.IdleAfter - time.Minute)
		raw, _ = json.Marshal(record)
		_ = os.WriteFile(file, raw, 0o644)
	}
	result, err := service.ClearAllStale(context.Background(), ActorCLI)
	if err != nil || result.Outcome != Completed || result.Cleanup.Removed != 2 {
		t.Fatalf("ClearAllStale = %#v err=%v", result, err)
	}
	journalStore, _ := journal.Open(service.Root)
	audits, err := journalStore.Recent(10)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits = %#v err=%v", audits, err)
	}
	if audits[0].Action != CleanupAll || audits[0].Attempted != 2 || audits[0].Removed != 2 {
		t.Fatalf("aggregate audit = %#v", audits[0])
	}
}
