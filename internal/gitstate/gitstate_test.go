package gitstate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/state"
)

func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func write(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func commitAll(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestNewFileIsDetected(t *testing.T) {
	root := repo(t)

	before, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if before.Version != state.CurrentVersion {
		t.Errorf("snapshot version = %d, want %d", before.Version, state.CurrentVersion)
	}
	write(t, root, "src/api/users.go", "package api")
	after, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	if got := Changed(before, after); !slices.Contains(got, "src/api/users.go") {
		t.Errorf("Changed = %v, want src/api/users.go", got)
	}
}

// The case a status-only comparison misses: a file that was already dirty and is
// then modified again reads as "M" both times.
func TestSecondEditToAnAlreadyDirtyFileIsDetected(t *testing.T) {
	root := repo(t)
	write(t, root, "src/api/users.go", "package api")
	commitAll(t, root)

	// Make it dirty first, so it is already "M" in the before snapshot.
	write(t, root, "src/api/users.go", "package api // once")
	before, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if before.Files["src/api/users.go"].Status == "" {
		t.Fatal("the file is not dirty in the before snapshot, so this proves nothing")
	}

	// Ensure the modification time can actually differ.
	time.Sleep(10 * time.Millisecond)
	write(t, root, "src/api/users.go", "package api // twice, and longer")

	after, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := Changed(before, after); !slices.Contains(got, "src/api/users.go") {
		t.Errorf("Changed = %v, want the second edit detected", got)
	}
}

func TestUntouchedTreeReportsNothing(t *testing.T) {
	root := repo(t)
	write(t, root, "src/api/users.go", "package api")
	commitAll(t, root)

	before, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	after, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := Changed(before, after); len(got) != 0 {
		t.Errorf("Changed = %v on an untouched tree, want none", got)
	}
}

func TestSeveralFilesInOneCommandAreAllReported(t *testing.T) {
	root := repo(t)

	before, _ := Take(root)
	write(t, root, "src/api/a.go", "a")
	write(t, root, "src/components/b.tsx", "b")
	write(t, root, "styles/c.css", "c")
	after, _ := Take(root)

	got := Changed(before, after)
	for _, want := range []string{"src/api/a.go", "src/components/b.tsx", "styles/c.css"} {
		if !slices.Contains(got, want) {
			t.Errorf("Changed = %v, missing %s", got, want)
		}
	}
}

// "We do not know" must not read as "nothing happened", or a failed snapshot
// would silently clear an agent of a write it made.
func TestAMissingSnapshotReportsNothingRatherThanEverything(t *testing.T) {
	root := repo(t)
	write(t, root, "a.go", "a")
	after, _ := Take(root)

	if got := Changed(nil, after); len(got) != 0 {
		t.Errorf("Changed(nil, after) = %v, want none", got)
	}
	if got := Changed(after, nil); len(got) != 0 {
		t.Errorf("Changed(after, nil) = %v, want none", got)
	}
}

// A file the repo has chosen not to track is not one teams contend over.
func TestIgnoredFilesAreNotReported(t *testing.T) {
	root := repo(t)
	write(t, root, ".gitignore", "secret.txt\n")
	commitAll(t, root)

	before, _ := Take(root)
	write(t, root, "secret.txt", "shh")
	write(t, root, "tracked.go", "x")
	after, _ := Take(root)

	got := Changed(before, after)
	if slices.Contains(got, "secret.txt") {
		t.Errorf("Changed = %v, an ignored file should not be reported", got)
	}
	if !slices.Contains(got, "tracked.go") {
		t.Errorf("Changed = %v, want the untracked-but-not-ignored file", got)
	}
}

func TestOutsideAGitRepoIsAClearError(t *testing.T) {
	dir := t.TempDir()

	if Available(dir) {
		t.Error("Available said yes outside a repo")
	}
	if _, err := Take(dir); !errors.Is(err, ErrNotAvailable) {
		t.Errorf("Take error = %v, want ErrNotAvailable so the caller can fall back", err)
	}
}

func TestAvailableInARepo(t *testing.T) {
	if !Available(repo(t)) {
		t.Error("Available said no inside a git repo")
	}
}
