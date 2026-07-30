package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeTarget(root string) Target {
	for _, t := range Plan(root) {
		if t.Agent == "claude" {
			return t
		}
	}
	panic("no claude target in the plan")
}

func readDoc(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v\n%s", file, err, raw)
	}
	return doc
}

func TestApplyCreatesSettingsWhenThereAreNone(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	change, err := Apply(target, "/bin/interlock", false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Changed() {
		t.Fatal("nothing changed on a fresh repo")
	}
	if change.Backup != "" {
		t.Errorf("backed up %q, but there was no file to back up", change.Backup)
	}

	doc := readDoc(t, target.File)
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks object: %v", doc)
	}
	for event := range target.Events {
		if _, ok := hooks[event]; !ok {
			t.Errorf("event %q was not installed", event)
		}
	}
}

// The property that matters most: the user's own settings survive.
func TestApplyPreservesEverythingElse(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	if err := os.MkdirAll(filepath.Dir(target.File), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
	  "model": "opus",
	  "permissions": {"allow": ["Bash(git:*)"]},
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-guard"}]}
	    ],
	    "Notification": [
	      {"hooks": [{"type": "command", "command": "say done"}]}
	    ]
	  }
	}`
	if err := os.WriteFile(target.File, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	change, err := Apply(target, "/bin/interlock", false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	doc := readDoc(t, target.File)
	if doc["model"] != "opus" {
		t.Errorf("model = %v, want opus preserved", doc["model"])
	}
	if _, ok := doc["permissions"]; !ok {
		t.Error("permissions were dropped")
	}

	hooks := doc["hooks"].(map[string]any)
	if _, ok := hooks["Notification"]; !ok {
		t.Error("an unrelated hook event was dropped")
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("PreToolUse has %d entries, want the user's plus ours", len(pre))
	}
	// The user's guard must still be there, and first.
	if !strings.Contains(mustJSON(t, pre[0]), "my-own-guard") {
		t.Errorf("the user's own hook is not first: %s", mustJSON(t, pre[0]))
	}

	// Modifying an existing file must leave a copy of it.
	if change.Backup == "" {
		t.Fatal("no backup was made when modifying an existing file")
	}
	backup := readDoc(t, change.Backup)
	backupHooks := backup["hooks"].(map[string]any)
	if len(backupHooks["PreToolUse"].([]any)) != 1 {
		t.Error("the backup does not hold the file as it was before")
	}
}

// Running init twice is normal. Installing twice would double every hook.
func TestApplyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := Apply(target, "/bin/interlock", false)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if second.Changed() {
		t.Errorf("second Apply changed %v, want nothing", second.Added)
	}

	hooks := readDoc(t, target.File)["hooks"].(map[string]any)
	if got := len(hooks["PreToolUse"].([]any)); got != 1 {
		t.Errorf("PreToolUse has %d entries after two installs, want 1", got)
	}
}

// Moving the binary must not orphan the entries and cause a duplicate install.
func TestApplyRecognisesItsOwnEntryAfterTheBinaryMoves(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	if _, err := Apply(target, "/old/path/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	again, err := Apply(target, "/new/place/interlock", false)
	if err != nil {
		t.Fatalf("Apply after move: %v", err)
	}
	if again.Changed() {
		t.Errorf("reinstalled after the binary moved: %v", again.Added)
	}
}

func TestDryRunReportsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	change, err := Apply(target, "/bin/interlock", true)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Changed() {
		t.Error("dry run reported no change, but nothing is installed")
	}
	if _, err := os.Stat(target.File); !os.IsNotExist(err) {
		t.Error("dry run wrote the file")
	}
}

// Overwriting settings we cannot parse would throw away hand-written work.
func TestBrokenSettingsAreRefusedNotOverwritten(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	if err := os.MkdirAll(filepath.Dir(target.File), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	broken := `{"hooks": {oh dear`
	if err := os.WriteFile(target.File, []byte(broken), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Apply(target, "/bin/interlock", false); err == nil {
		t.Fatal("Apply succeeded on unparseable settings")
	}

	raw, err := os.ReadFile(target.File)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != broken {
		t.Error("the unparseable file was modified")
	}
}

// Anything that writes into a user's file owes them a clean way out.
func TestRemoveTakesOnlyOurHooksBackOut(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	if err := os.MkdirAll(filepath.Dir(target.File), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target.File, []byte(`{
	  "model": "opus",
	  "hooks": {"PreToolUse": [
	    {"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-guard"}]}
	  ]}
	}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	change, err := Remove(target, false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !change.Changed() {
		t.Fatal("Remove reported no change")
	}

	doc := readDoc(t, target.File)
	if doc["model"] != "opus" {
		t.Error("Remove dropped unrelated settings")
	}
	hooks := doc["hooks"].(map[string]any)
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok || len(pre) != 1 {
		t.Fatalf("PreToolUse = %v, want just the user's own hook", hooks["PreToolUse"])
	}
	if !strings.Contains(mustJSON(t, pre[0]), "my-own-guard") {
		t.Error("Remove took out the wrong hook")
	}
	// Events that held only our hook should be gone, not left as empty arrays.
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("an empty array was left behind after removal")
	}
}

func TestRemoveOnAnUntouchedRepoIsANoOp(t *testing.T) {
	root := t.TempDir()
	change, err := Remove(claudeTarget(root), false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if change.Changed() {
		t.Errorf("Remove changed %v on a repo with no settings", change.Removed)
	}
}

// Codex has no session-end event, so promising one in the plan would install a
// hook that never fires and imply leases are released on exit.
func TestCodexPlanHasNoSessionEnd(t *testing.T) {
	for _, target := range Plan(t.TempDir()) {
		if target.Agent != "codex" {
			continue
		}
		if _, ok := target.Events["SessionEnd"]; ok {
			t.Error("codex plan includes SessionEnd, which Codex does not emit")
		}
		for event, matcher := range target.Events {
			if matcher != "" {
				t.Errorf("codex event %q has matcher %q; Codex fires for every tool",
					event, matcher)
			}
		}
	}
}

func TestGitignoreAddsOnceAndOnlyOnce(t *testing.T) {
	root := t.TempDir()

	added, err := Gitignore(root, ".interlock/")
	if err != nil || !added {
		t.Fatalf("first Gitignore: added=%v err=%v", added, err)
	}
	again, err := Gitignore(root, ".interlock/")
	if err != nil {
		t.Fatalf("second Gitignore: %v", err)
	}
	if again {
		t.Error("the same line was added twice")
	}

	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if strings.Count(string(raw), ".interlock/") != 1 {
		t.Errorf(".gitignore = %q, want one entry", raw)
	}
}

// Appending to a file with no trailing newline must not join two entries.
func TestGitignoreAppendsSafelyToAFileWithoutATrailingNewline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Gitignore(root, ".interlock/"); err != nil {
		t.Fatalf("Gitignore: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "node_modules.interlock") {
		t.Errorf("entries were joined: %q", raw)
	}
	if !strings.Contains(string(raw), "node_modules\n") {
		t.Errorf("existing entry was mangled: %q", raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// The bug this exists to prevent: PreToolUse snapshots the tree before a shell
// command whose targets cannot be read in advance, and PostToolUse is what
// compares it afterwards. When PostToolUse left Bash out, the snapshot was armed
// and nothing ever looked at it, so the whole after-the-fact check was dead
// while every unit test still passed.
func TestArmAndVerifyEventsCoverTheSameTools(t *testing.T) {
	for _, target := range Plan(t.TempDir()) {
		pre, hasPre := target.Events["PreToolUse"]
		post, hasPost := target.Events["PostToolUse"]
		if !hasPre || !hasPost {
			t.Errorf("%s: needs both PreToolUse and PostToolUse, has pre=%v post=%v",
				target.Agent, hasPre, hasPost)
			continue
		}
		if pre != post {
			t.Errorf("%s: PreToolUse matcher %q does not match PostToolUse %q. "+
				"Anything armed by one and not seen by the other is never checked.",
				target.Agent, pre, post)
		}
	}
}

// Every tool that can write has to reach the guard, whichever route it takes.
func TestClaudeMatcherCoversEveryWritingTool(t *testing.T) {
	matcher := claudeTarget(t.TempDir()).Events["PreToolUse"]
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash"} {
		if !strings.Contains(matcher, tool) {
			t.Errorf("matcher %q does not cover %s", matcher, tool)
		}
	}
}
