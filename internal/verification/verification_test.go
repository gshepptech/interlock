package verification

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gshepptech/interlock/internal/state"
)

func repo(t *testing.T) string {
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

func TestOpenIsReadOnlyAndArmConsumeIsVersioned(t *testing.T) {
	root := repo(t)
	store := Open(root)
	if _, err := os.Stat(filepath.Join(root, ".interlock")); !os.IsNotExist(err) {
		t.Errorf("Open mutated repository: %v", err)
	}
	if err := store.Arm("s1"); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	snapshot, err := store.Consume("s1")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if snapshot.Version != state.CurrentVersion {
		t.Errorf("version = %d, want %d", snapshot.Version, state.CurrentVersion)
	}
	if _, err := os.Stat(store.pathFor("s1")); !os.IsNotExist(err) {
		t.Errorf("Consume left snapshot behind: %v", err)
	}
}

func TestFutureSnapshotIsNeverConsumedRemovedOrOverwritten(t *testing.T) {
	root := repo(t)
	store := Open(root)
	if err := os.MkdirAll(store.dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := store.pathFor("future")
	original := []byte("{\"version\":99,\"files\":{}}")
	if err := os.WriteFile(file, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for name, fn := range map[string]func() error{
		"Arm": func() error { return store.Arm("future") },
		"Consume": func() error {
			_, err := store.Consume("future")
			return err
		},
		"Remove": func() error {
			_, err := store.Remove("future")
			return err
		},
	} {
		err := fn()
		var unsupported *state.UnsupportedVersionError
		if !errors.As(err, &unsupported) {
			t.Errorf("%s error = %v, want UnsupportedVersionError", name, err)
		}
		got, readErr := os.ReadFile(file)
		if readErr != nil || string(got) != string(original) {
			t.Fatalf("%s changed future snapshot: got=%q err=%v", name, got, readErr)
		}
	}
}
