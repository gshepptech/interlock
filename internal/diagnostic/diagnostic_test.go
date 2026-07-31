package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/session"
)

func testRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func testOptions(t *testing.T) Options {
	t.Helper()
	root := testRepo(t)
	return Options{
		Root:       root,
		Executable: "/bin/interlock",
		CodexFile:  filepath.Join(root, "home", ".codex", "config.toml"),
	}
}

func TestCheckDoesNotCreateRuntimeState(t *testing.T) {
	opts := testOptions(t)
	report, err := (Service{}).Check(opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.HasFailures() {
		t.Fatal("uninstalled repository reported healthy")
	}
	if _, err := os.Stat(filepath.Join(opts.Root, ".interlock")); !os.IsNotExist(err) {
		t.Errorf("Check created runtime state: %v", err)
	}
}

func TestRepairIsCompleteAndIdempotent(t *testing.T) {
	opts := testOptions(t)
	service := Service{}

	first, err := service.Repair(opts, false)
	if err != nil {
		t.Fatalf("first Repair: %v", err)
	}
	changed := 0
	for _, repair := range first {
		if repair.Changed {
			changed++
		}
	}
	if changed != len(first) {
		t.Errorf("first repair changed %d of %d results: %#v", changed, len(first), first)
	}

	second, err := service.Repair(opts, false)
	if err != nil {
		t.Fatalf("second Repair: %v", err)
	}
	for _, repair := range second {
		if repair.Changed {
			t.Errorf("second repair changed %#v", repair)
		}
	}

	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.HasFailures() {
		t.Errorf("repaired install still has failures: %#v", report.Checks)
	}
}

func TestRepairDryRunDoesNotWrite(t *testing.T) {
	opts := testOptions(t)
	repairs, err := (Service{}).Repair(opts, true)
	if err != nil {
		t.Fatalf("Repair dry-run: %v", err)
	}
	for _, repair := range repairs {
		if !repair.Changed {
			t.Errorf("fresh dry-run did not report pending change: %#v", repair)
		}
	}
	for _, path := range []string{
		filepath.Join(opts.Root, ".claude"),
		filepath.Join(opts.Root, ".codex"),
		filepath.Join(opts.Root, ".gitignore"),
		opts.CodexFile,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("dry-run wrote %s: %v", path, err)
		}
	}
}

func TestPinnedConfigSurvivesDeclarationRemovalForCheckSmokeAndRepair(t *testing.T) {
	parent := testRepo(t)
	root := filepath.Join(parent, "nested")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir nested root: %v", err)
	}
	declaration := filepath.Join(root, config.FileName)
	if err := os.WriteFile(declaration, []byte(`{"teams":{}}`), 0o644); err != nil {
		t.Fatalf("write declaration: %v", err)
	}
	cfg, err := config.Load(root)
	if err != nil || cfg.Root != root {
		t.Fatalf("load pinned config: root=%q err=%v", cfg.Root, err)
	}
	opts := Options{
		Root: root, Config: cfg, Executable: "/bin/interlock",
		CodexFile: filepath.Join(parent, "home", ".codex", "config.toml"),
	}
	service := Service{}
	if _, err := service.Repair(opts, false); err != nil {
		t.Fatalf("initial repair: %v", err)
	}
	if err := os.Remove(declaration); err != nil {
		t.Fatalf("remove declaration: %v", err)
	}
	current, err := config.Load(root)
	if err != nil || current.Root != parent {
		t.Fatalf("hook resolution fixture root=%q want parent=%q err=%v", current.Root, parent, err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".claude")); err != nil {
		t.Fatalf("remove child hooks: %v", err)
	}

	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("pinned check: %v", err)
	}
	assertDiagnosticRoot(t, report, root)
	assertCheck(t, report, "config_drift", Fail, "boundary changed")
	if _, err := service.Repair(opts, false); err == nil || !strings.Contains(err.Error(), "boundary changed") {
		t.Fatalf("pinned repair drift error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("drifted repair mutated child hooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("repair escaped to parent: %v", err)
	}

	opts.Smoke = true
	smokeCalls := 0
	service.RunCommand = func(_ context.Context, dir, command string, stdin []byte) (
		[]byte, []byte, error) {
		smokeCalls++
		return nil, nil, errors.New("smoke must not execute after root drift")
	}
	report, err = service.Check(opts)
	if err != nil {
		t.Fatalf("pinned smoke: %v", err)
	}
	assertDiagnosticRoot(t, report, root)
	if smokeCalls != 0 {
		t.Fatalf("smoke executed %d hook commands after root drift", smokeCalls)
	}
	assertCheck(t, report, "claude_smoke", Fail, "not run")
	assertCheck(t, report, "codex_smoke", Fail, "not run")
	for _, path := range []string{
		filepath.Join(parent, ".interlock"), filepath.Join(parent, ".claude"), filepath.Join(parent, ".codex"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("drifted operation created parent state %s: %v", path, err)
		}
	}
}

func assertCheck(t *testing.T, report Report, id string, severity Severity, detail string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			if check.Severity != severity || !strings.Contains(check.Detail, detail) {
				t.Fatalf("check %s = %#v", id, check)
			}
			return
		}
	}
	t.Fatalf("diagnostic report has no %s check", id)
}

func assertDiagnosticRoot(t *testing.T, report Report, root string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == "repo" {
			if check.Detail != root {
				t.Fatalf("diagnostic root = %q, want %q", check.Detail, root)
			}
			return
		}
	}
	t.Fatal("diagnostic report has no repo check")
}

func TestMalformedCodexConfigIsNotMarkedRepairable(t *testing.T) {
	opts := testOptions(t)
	service := Service{}
	if _, err := service.Repair(opts, false); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if err := os.WriteFile(opts.CodexFile, []byte("[broken"), 0o644); err != nil {
		t.Fatalf("corrupt Codex config: %v", err)
	}
	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	found := 0
	for _, check := range report.Checks {
		if check.ID != "codex_repo_trust" && check.ID != "codex_hook_trust" {
			continue
		}
		found++
		if check.Severity != Fail || check.Repairable {
			t.Errorf("malformed trust check = %#v", check)
		}
	}
	if found != 2 {
		t.Fatalf("found %d Codex trust checks, want 2", found)
	}
}

func TestSmokeUsesInstalledCommandsAndCleansSessions(t *testing.T) {
	opts := testOptions(t)
	service := Service{}
	if _, err := service.Repair(opts, false); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	opts.Smoke = true

	var commands []string
	service.RunCommand = func(_ context.Context, dir, command string, stdin []byte) (
		[]byte, []byte, error) {
		commands = append(commands, command)
		var payload map[string]any
		if err := json.Unmarshal(stdin, &payload); err != nil {
			return nil, nil, err
		}
		sessionID, _ := payload["session_id"].(string)
		cwd, _ := payload["cwd"].(string)
		agent := "claude"
		if strings.Contains(command, " codex ") {
			agent = "codex"
		}
		store, err := session.Open(dir)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, store.Seen(sessionID, agent, "", "", cwd)
	}

	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("Check smoke: %v", err)
	}
	for _, check := range report.Checks {
		if strings.HasSuffix(check.ID, "_smoke") && check.Severity != OK {
			t.Errorf("smoke check failed: %#v", check)
		}
	}
	if len(commands) != 2 {
		t.Fatalf("ran %d commands, want one per target", len(commands))
	}
	for _, command := range commands {
		if !strings.Contains(command, "# interlock-managed:v1") {
			t.Errorf("smoke did not execute exact managed command: %q", command)
		}
	}
	store, _ := session.Open(opts.Root)
	active, err := store.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("smoke sessions were not cleaned up: %+v", active)
	}
}

func TestSmokeFailsWhenSyntheticSessionCleanupFails(t *testing.T) {
	opts := testOptions(t)
	service := Service{}
	if _, err := service.Repair(opts, false); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	opts.Smoke = true
	service.RunCommand = func(_ context.Context, dir, command string, stdin []byte) (
		[]byte, []byte, error) {
		var payload map[string]any
		if err := json.Unmarshal(stdin, &payload); err != nil {
			return nil, nil, err
		}
		agent := "claude"
		if strings.Contains(command, " codex ") {
			agent = "codex"
		}
		store, _ := session.Open(dir)
		return nil, nil, store.Seen(payload["session_id"].(string), agent, "", "", dir)
	}
	service.EndSession = func(string, string) error { return errors.New("injected cleanup failure") }

	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, check := range report.Checks {
		if strings.HasSuffix(check.ID, "_smoke") &&
			(check.Severity != Fail || !strings.Contains(check.Detail, "cleanup failed")) {
			t.Errorf("smoke cleanup failure was hidden: %#v", check)
		}
	}
}

func TestRunShellTimeoutKillsSpawnedProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-finished")
	command := fmt.Sprintf("(sleep 0.3; echo child > '%s') & wait", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := runShell(ctx, t.TempDir(), command, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runShell error = %v, want deadline exceeded", err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("spawned child survived timeout: %v", err)
	}
}

func TestCheckReportsFutureStateWithoutChangingIt(t *testing.T) {
	opts := testOptions(t)
	service := Service{}
	if _, err := service.Repair(opts, false); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	store, _ := session.Open(opts.Root)
	if err := store.Seen("future", "codex", "", "", opts.Root); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	dir := filepath.Join(opts.Root, ".interlock", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	file := ""
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			file = filepath.Join(dir, entry.Name())
			break
		}
	}
	if file == "" {
		t.Fatal("session record not found")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	doc["version"] = float64(99)
	original, _ := json.Marshal(doc)
	if err := os.WriteFile(file, original, 0o644); err != nil {
		t.Fatalf("write future: %v", err)
	}

	report, err := service.Check(opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "state_schema" {
			found = check.Severity == Fail && strings.Contains(check.Detail, "version 99")
		}
	}
	if !found {
		t.Errorf("future state not reported: %#v", report.Checks)
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(original) {
		t.Fatal("Check changed future state")
	}
}
