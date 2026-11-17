package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorFailsOnAnUnwiredRepoAndNamesTheProblem(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := cli(t, dir, "doctor")
	if res.exit == 0 {
		t.Fatalf("doctor passed on a repo with no hooks wired:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "not wired up") {
		t.Errorf("doctor does not say the hooks are missing:\n%s", res.stdout)
	}
	// The silent failure is the one worth naming explicitly.
	if !strings.Contains(res.stdout, "trust") {
		t.Errorf("doctor does not mention Codex repo trust:\n%s", res.stdout)
	}
}

func TestInitThenDoctorAgreeOnWhatIsWired(t *testing.T) {
	dir := repo(t, defaultConfig)

	if res := cli(t, dir, "init"); res.exit > 1 {
		t.Fatalf("init failed: %s%s", res.stdout, res.stderr)
	}
	res := cli(t, dir, "doctor")
	for _, agent := range []string{"claude hooks", "codex hooks"} {
		line := agent + " "
		if !strings.Contains(res.stdout, line) {
			t.Errorf("doctor does not report %s:\n%s", agent, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "not wired up") {
		t.Errorf("doctor still reports missing hooks after init:\n%s", res.stdout)
	}
}

func TestDoctorFixRepairsAndIsIdempotent(t *testing.T) {
	dir := repo(t, defaultConfig)

	first := cli(t, dir, "doctor", "--fix")
	if first.exit != 0 {
		t.Fatalf("doctor --fix failed: stdout=%s stderr=%s", first.stdout, first.stderr)
	}
	if !strings.Contains(first.stdout, "FIX") ||
		!strings.Contains(first.stdout, "Everything is wired up") {
		t.Errorf("doctor --fix did not report repairs and final health:\n%s", first.stdout)
	}

	second := cli(t, dir, "doctor", "--fix")
	if second.exit != 0 {
		t.Fatalf("second doctor --fix failed: stdout=%s stderr=%s",
			second.stdout, second.stderr)
	}
	if strings.Contains(second.stdout, "FIX") {
		t.Errorf("second doctor --fix was not idempotent:\n%s", second.stdout)
	}
}

func TestDoctorSmokeRunsInstalledCommandsAndLeavesNoSession(t *testing.T) {
	dir := repo(t, defaultConfig)
	if res := cli(t, dir, "init"); res.exit != 0 {
		t.Fatalf("init failed: %s%s", res.stdout, res.stderr)
	}

	res := cli(t, dir, "doctor", "--smoke")
	if res.exit != 0 {
		t.Fatalf("doctor --smoke failed: stdout=%s stderr=%s", res.stdout, res.stderr)
	}
	for _, agent := range []string{"claude smoke", "codex smoke"} {
		if !strings.Contains(res.stdout, agent) {
			t.Errorf("smoke output missing %q:\n%s", agent, res.stdout)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".interlock", "sessions"))
	if err != nil {
		t.Fatalf("read smoke sessions: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Errorf("smoke left session %s behind", entry.Name())
		}
	}
}

func TestDoctorFixRepairsHooksAfterBinaryMoves(t *testing.T) {
	dir := repo(t, defaultConfig)
	if res := cli(t, dir, "init"); res.exit != 0 {
		t.Fatalf("init failed: %s%s", res.stdout, res.stderr)
	}
	oldPath := "/old/location/interlock"
	for _, file := range []string{
		claudeSettings(dir),
		filepath.Join(dir, ".codex", "hooks.json"),
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		moved := strings.ReplaceAll(string(raw), binPath, oldPath)
		if moved == string(raw) {
			t.Fatalf("%s did not contain installed binary path", file)
		}
		if err := os.WriteFile(file, []byte(moved), 0o644); err != nil {
			t.Fatalf("move path in %s: %v", file, err)
		}
	}

	before := cli(t, dir, "doctor")
	if before.exit == 0 || !strings.Contains(before.stdout, "stale") {
		t.Fatalf("doctor did not detect moved binary: stdout=%s stderr=%s", before.stdout, before.stderr)
	}
	fixed := cli(t, dir, "doctor", "--fix")
	if fixed.exit != 0 {
		t.Fatalf("doctor --fix failed: stdout=%s stderr=%s", fixed.stdout, fixed.stderr)
	}
	for _, file := range []string{
		claudeSettings(dir),
		filepath.Join(dir, ".codex", "hooks.json"),
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read repaired %s: %v", file, err)
		}
		if strings.Contains(string(raw), oldPath) || !strings.Contains(string(raw), binPath) {
			t.Fatalf("hook path was not repaired in %s:\n%s", file, raw)
		}
	}
}
