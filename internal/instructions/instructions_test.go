package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

func TestApplyCreatesTheFileWhenThereIsNone(t *testing.T) {
	root := t.TempDir()

	change, err := Apply(root, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Added {
		t.Error("did not report adding the section")
	}
	body := readFile(t, root)
	if !strings.Contains(body, "interlock check") {
		t.Errorf("the instruction does not name the command:\n%s", body)
	}
	if !strings.Contains(body, startMarker) || !strings.Contains(body, endMarker) {
		t.Error("the block is not delimited, so it can never be updated or removed")
	}
}

// The file belongs to the user and usually already says things that matter.
func TestApplyPreservesEverythingTheUserWrote(t *testing.T) {
	root := t.TempDir()
	existing := "# My project\n\nAlways run the tests before committing.\n\n## Style\n\nTabs.\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	change, err := Apply(root, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body := readFile(t, root)
	for _, keep := range []string{"# My project", "Always run the tests", "## Style", "Tabs."} {
		if !strings.Contains(body, keep) {
			t.Errorf("lost the user's own text %q:\n%s", keep, body)
		}
	}
	if change.Backup == "" {
		t.Error("modified an existing file without keeping a copy")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	root := t.TempDir()

	if _, err := Apply(root, false); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first := readFile(t, root)
	second, err := Apply(root, false)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if second.Changed() {
		t.Errorf("second Apply reported a change: %+v", second)
	}
	if readFile(t, root) != first {
		t.Error("second Apply rewrote the file")
	}
	if strings.Count(readFile(t, root), startMarker) != 1 {
		t.Error("the block was added twice")
	}
}

// An older section must be replaced in place, not appended beside the new one.
func TestApplyReplacesAnOutdatedBlock(t *testing.T) {
	root := t.TempDir()
	stale := "# Notes\n\n" + startMarker + "\nold and wrong\n" + endMarker + "\n\nkeep me\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(stale), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	change, err := Apply(root, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Updated {
		t.Errorf("reported %+v, want an update", change)
	}
	body := readFile(t, root)
	if strings.Contains(body, "old and wrong") {
		t.Errorf("the stale block survived:\n%s", body)
	}
	if strings.Count(body, startMarker) != 1 {
		t.Errorf("ended up with more than one block:\n%s", body)
	}
	for _, keep := range []string{"# Notes", "keep me"} {
		if !strings.Contains(body, keep) {
			t.Errorf("lost %q while replacing:\n%s", keep, body)
		}
	}
}

func TestRemoveTakesOnlyOurBlock(t *testing.T) {
	root := t.TempDir()
	existing := "# My project\n\nAlways run the tests.\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Apply(root, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	change, err := Remove(root, false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !change.Removed {
		t.Error("did not report removing the section")
	}
	body := readFile(t, root)
	if strings.Contains(body, "interlock check") || strings.Contains(body, startMarker) {
		t.Errorf("our block survived:\n%s", body)
	}
	if !strings.Contains(body, "Always run the tests") {
		t.Errorf("removing took the user's text with it:\n%s", body)
	}
}

// A file that held nothing but our block is ours to clean up, not an empty
// artefact to leave behind.
func TestRemoveDeletesAFileThatWasOnlyOurs(t *testing.T) {
	root := t.TempDir()
	if _, err := Apply(root, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := Remove(root, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); !os.IsNotExist(err) {
		t.Error("left an empty instructions file the user never created")
	}
}

// An unterminated marker means someone edited by hand and left it broken.
// Rewriting from it would eat the rest of the document.
func TestUnterminatedBlockIsNotTreatedAsOurs(t *testing.T) {
	root := t.TempDir()
	broken := "# Notes\n\n" + startMarker + "\nthey deleted the end marker\n\nimportant user text\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(broken), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Apply(root, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body := readFile(t, root)
	if !strings.Contains(body, "important user text") {
		t.Errorf("ate the rest of the document:\n%s", body)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()

	change, err := Apply(root, true)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Added {
		t.Error("dry run did not report what it would do")
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); !os.IsNotExist(err) {
		t.Error("dry run created the file")
	}
}

func TestInstalledReportsPresenceAndCurrency(t *testing.T) {
	root := t.TempDir()

	if present, _ := Installed(root); present {
		t.Error("reported present before anything was written")
	}
	if _, err := Apply(root, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	present, current := Installed(root)
	if !present || !current {
		t.Errorf("present=%v current=%v, want both true", present, current)
	}

	stale := "# x\n\n" + startMarker + "\nold\n" + endMarker + "\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(stale), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	present, current = Installed(root)
	if !present || current {
		t.Errorf("present=%v current=%v, want present but not current", present, current)
	}
}

// The exit codes are the contract; the prose has to state them or a model has
// nothing to act on.
func TestBodyStatesTheContract(t *testing.T) {
	for _, want := range []string{"interlock check", "exit code 0", "Exit code 2", "blocked"} {
		if !strings.Contains(strings.ToLower(Body), strings.ToLower(want)) {
			t.Errorf("the instruction never mentions %q:\n%s", want, Body)
		}
	}
}
