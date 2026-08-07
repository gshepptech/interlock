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

// Moving the binary must update our entries in place rather than duplicate them
// or leave hooks pointing at an executable that no longer exists.
func TestApplyRepairsItsOwnEntryAfterTheBinaryMoves(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	if _, err := Apply(target, "/old/path/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	again, err := Apply(target, "/new/place/interlock", false)
	if err != nil {
		t.Fatalf("Apply after move: %v", err)
	}
	if len(again.Added) != 0 || len(again.Updated) != len(target.Events) {
		t.Errorf("change = %#v, want every event updated in place", again)
	}
	hooks := readDoc(t, target.File)["hooks"].(map[string]any)
	if got := len(hooks["PreToolUse"].([]any)); got != 1 {
		t.Errorf("PreToolUse has %d entries after repair, want 1", got)
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

func codexTarget(root string) Target {
	for _, t := range Plan(root) {
		if t.Agent == "codex" {
			return t
		}
	}
	panic("no codex target in the plan")
}

func TestCodexTrustEntriesMatchCodexHashes(t *testing.T) {
	root := t.TempDir()
	target := codexTarget(root)
	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries, err := CodexTrustEntries(target)
	if err != nil {
		t.Fatalf("CodexTrustEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d trust entries, want 3: %#v", len(entries), entries)
	}

	want := map[string]string{
		filepath.Join(root, ".codex/hooks.json") + ":post_tool_use:0:0": "sha256:160d0dff29043324eef8866a02980ad110ab4c2808aa3a8a29cb5a31931d0194",
		filepath.Join(root, ".codex/hooks.json") + ":pre_tool_use:0:0":  "sha256:fc0f71122a7e2dbe3075864c08553237b9c72492d8d708e7f6f2bc331c17093a",
		filepath.Join(root, ".codex/hooks.json") + ":session_start:0:0": "sha256:e3368389ca4edc0595af81ccd00c0f1b32774c7d4cd89cc549ba725025d2aaee",
	}
	for _, entry := range entries {
		if want[entry.Key] != entry.Hash {
			t.Errorf("%s hash = %q, want %q", entry.Key, entry.Hash, want[entry.Key])
		}
		delete(want, entry.Key)
	}
	for key := range want {
		t.Errorf("missing trust entry %s", key)
	}
}

func TestTrustCodexConfigAddsRepoAndHookTrust(t *testing.T) {
	root := t.TempDir()
	target := codexTarget(root)
	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, err := CodexTrustEntries(target)
	if err != nil {
		t.Fatalf("CodexTrustEntries: %v", err)
	}

	file := filepath.Join(t.TempDir(), ".codex", "config.toml")
	change, err := TrustCodexConfig(file, root, entries, false)
	if err != nil {
		t.Fatalf("TrustCodexConfig: %v", err)
	}
	if !change.ProjectTrusted {
		t.Error("project trust was not reported")
	}
	if len(change.HooksTrusted) != len(entries) {
		t.Errorf("trusted %d hooks, want %d", len(change.HooksTrusted), len(entries))
	}
	ok, detail := CodexTrustStatus(file, root, entries)
	if !ok {
		t.Fatalf("CodexTrustStatus failed: %s", detail)
	}

	again, err := TrustCodexConfig(file, root, entries, false)
	if err != nil {
		t.Fatalf("second TrustCodexConfig: %v", err)
	}
	if again.Changed() {
		t.Errorf("second TrustCodexConfig changed config: %#v", again)
	}
}

func TestTrustCodexConfigUpdatesStaleHookHash(t *testing.T) {
	root := t.TempDir()
	target := codexTarget(root)
	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, err := CodexTrustEntries(target)
	if err != nil {
		t.Fatalf("CodexTrustEntries: %v", err)
	}
	file := filepath.Join(t.TempDir(), "config.toml")
	header := `[hooks.state."` + entries[0].Key + `"]`
	if err := os.WriteFile(file, []byte(header+"\ntrusted_hash = \"sha256:old\"\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	change, err := TrustCodexConfig(file, root, entries[:1], false)
	if err != nil {
		t.Fatalf("TrustCodexConfig: %v", err)
	}
	if len(change.HooksUpdated) != 1 {
		t.Fatalf("updated %d hooks, want 1: %#v", len(change.HooksUpdated), change)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "sha256:old") {
		t.Errorf("stale hash still present:\n%s", raw)
	}
}

func TestTrustCodexConfigIgnoresTableTextInsideMultilineString(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	root := "/repo"
	original := "note = '''\n[projects.\"/repo\"]\ntrust_level = \"trusted\"\n'''\n"
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	status := InspectCodexProjectTrust(file, root)
	if status.State != TrustMissing {
		t.Fatalf("fake table state = %s, want missing: %s", status.State, status.Detail)
	}
	if _, err := TrustCodexConfig(file, root, nil, false); err != nil {
		t.Fatalf("TrustCodexConfig: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), original) {
		t.Fatalf("multiline value was not preserved:\n%s", raw)
	}
	if status = InspectCodexProjectTrust(file, root); status.State != TrustHealthy {
		t.Fatalf("repaired state = %s: %s", status.State, status.Detail)
	}
}

func TestTrustCodexConfigUpdatesEquivalentDottedKeysSemantically(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	root := "/repo"
	entry := CodexTrustEntry{Key: "/repo/.codex/hooks.json:session_start:0:0", Hash: "sha256:new"}
	body := "projects.\"/repo\".trust_level = \"untrusted\"\n" +
		"hooks.state.\"" + entry.Key + "\".trusted_hash = \"sha256:old\"\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	change, err := TrustCodexConfig(file, root, []CodexTrustEntry{entry}, false)
	if err != nil {
		t.Fatalf("TrustCodexConfig: %v", err)
	}
	if !change.ProjectTrusted || len(change.HooksUpdated) != 1 {
		t.Fatalf("change = %#v", change)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Count(string(raw), "trust_level") != 1 || strings.Count(string(raw), "trusted_hash") != 1 {
		t.Fatalf("semantic keys were duplicated:\n%s", raw)
	}
	if ok, detail := CodexTrustStatus(file, root, []CodexTrustEntry{entry}); !ok {
		t.Fatalf("generated TOML failed semantic verification: %s\n%s", detail, raw)
	}
}

func TestInspectCodexTrustMarksMalformedAndSymlinkUnrepairable(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.toml")
	if err := os.WriteFile(malformed, []byte("[broken"), 0o644); err != nil {
		t.Fatalf("seed malformed: %v", err)
	}
	status := InspectCodexProjectTrust(malformed, "/repo")
	if status.State != TrustMalformed || status.Repairable() {
		t.Fatalf("malformed status = %#v", status)
	}

	real := filepath.Join(dir, "real.toml")
	link := filepath.Join(dir, "link.toml")
	if err := os.WriteFile(real, []byte(""), 0o644); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	status = InspectCodexProjectTrust(link, "/repo")
	if status.State != TrustMalformed || status.Repairable() {
		t.Fatalf("symlink status = %#v", status)
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

func TestInspectReportsMissingStaleAndHealthy(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)

	missing, err := Inspect(target, "/new/interlock")
	if err != nil {
		t.Fatalf("Inspect missing: %v", err)
	}
	for _, event := range missing.Events {
		if event.State != Missing {
			t.Errorf("fresh %s state = %s, want missing", event.Event, event.State)
		}
	}

	if _, err := Apply(target, "/old/interlock", false); err != nil {
		t.Fatalf("Apply old: %v", err)
	}
	stale, err := Inspect(target, "/new/interlock")
	if err != nil {
		t.Fatalf("Inspect stale: %v", err)
	}
	for _, event := range stale.Events {
		if event.State != Stale {
			t.Errorf("old %s state = %s, want stale", event.Event, event.State)
		}
	}

	if _, err := Apply(target, "/new/interlock", false); err != nil {
		t.Fatalf("repair: %v", err)
	}
	healthy, err := Inspect(target, "/new/interlock")
	if err != nil {
		t.Fatalf("Inspect healthy: %v", err)
	}
	for _, event := range healthy.Events {
		if event.State != Healthy {
			t.Errorf("repaired %s state = %s (%s)",
				event.Event, event.State, event.Detail)
		}
	}
}

func TestCommandsQuoteExecutablePathsWithWhitespaceAndQuotes(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	exe := filepath.Join(root, "it's tools", "interlock")
	if _, err := Apply(target, exe, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	status, err := Inspect(target, exe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	for _, event := range status.Events {
		if event.State != Healthy {
			t.Errorf("%s state = %s (%s)", event.Event, event.State, event.Detail)
		}
	}
	raw, err := os.ReadFile(target.File)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), managedMarker) {
		t.Errorf("installed command lacks ownership marker: %s", raw)
	}
}

func TestApplyRefusesMixedOwnedAndUserHandlers(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	if err := os.MkdirAll(filepath.Dir(target.File), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	group := map[string]any{
		"matcher": claudeWriteTools,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCommand("/bin/interlock", "PreToolUse", "claude"),
			},
			map[string]any{"type": "command", "command": "my-own-guard"},
		},
	}
	doc := map[string]any{"hooks": map[string]any{"PreToolUse": []any{group}}}
	original, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(target.File, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Apply(target, "/bin/interlock", false); err == nil {
		t.Fatal("Apply repaired a mixed group")
	}
	raw, err := os.ReadFile(target.File)
	if err != nil || string(raw) != string(original) {
		t.Fatalf("mixed group changed: %s err=%v", raw, err)
	}
}

func TestTrustCodexConfigRefusesMalformedTOML(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[projects.\"broken\"\ntrust_level =")
	if err := os.WriteFile(file, original, 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := TrustCodexConfig(file, "/repo", nil, false); err == nil {
		t.Fatal("TrustCodexConfig accepted malformed TOML")
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != string(original) {
		t.Fatalf("malformed TOML changed: %q", raw)
	}
}

func TestSafeWritesPreserveModesAndRefuseSymlinks(t *testing.T) {
	root := t.TempDir()
	target := claudeTarget(root)
	if err := os.MkdirAll(filepath.Dir(target.File), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target.File, []byte("{\"model\":\"opus\"}"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Apply(target, "/bin/interlock", false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(target.File)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", info.Mode().Perm())
	}

	symlinkRoot := t.TempDir()
	real := filepath.Join(symlinkRoot, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	symlinkTarget := claudeTarget(symlinkRoot)
	if err := os.MkdirAll(filepath.Dir(symlinkTarget.File), 0o755); err != nil {
		t.Fatalf("mkdir symlink target: %v", err)
	}
	if err := os.Symlink(real, symlinkTarget.File); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Apply(symlinkTarget, "/bin/interlock", false); err == nil {
		t.Fatal("Apply followed a settings symlink")
	}
}

func TestWriteExpectedDetectsObservedConcurrentEdit(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	original, err := readSnapshot(file)
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	if err := os.WriteFile(file, []byte("user edit"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	err = writeExpected(file, []byte("interlock edit"), original)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("writeExpected error = %v, want detected change", err)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "user edit" {
		t.Fatalf("user edit was overwritten: %q", raw)
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

// Uninstalling used to leave hook trust behind, so every repository Interlock
// was ever installed into kept an entry in the user's global Codex config
// forever. Sixty stale entries pointing at deleted directories is what prompted
// this test.
func TestUninstallWithdrawsCodexHookTrust(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")

	// A config holding our entries plus things that belong to the user.
	seed := `model = "gpt-5.5"

[projects."/some/repo"]
trust_level = "trusted"

[hooks.state."/some/repo/.codex/hooks.json:pre_tool_use:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."/some/repo/.codex/hooks.json:post_tool_use:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."/somebody/elses/.codex/hooks.json:pre_tool_use:0:0"]
trusted_hash = "sha256:ccc"

[mcp_servers.playwright]
command = "npx"
`
	if err := os.WriteFile(config, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	entries := []CodexTrustEntry{
		{Key: "/some/repo/.codex/hooks.json:pre_tool_use:0:0", Hash: "sha256:aaa"},
		{Key: "/some/repo/.codex/hooks.json:post_tool_use:0:0", Hash: "sha256:bbb"},
	}

	change, err := UntrustCodexConfig(config, entries, false)
	if err != nil {
		t.Fatalf("UntrustCodexConfig: %v", err)
	}
	if len(change.HooksRemoved) != 2 {
		t.Errorf("removed %v, want both of ours", change.HooksRemoved)
	}

	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)

	for _, gone := range []string{"sha256:aaa", "sha256:bbb"} {
		if strings.Contains(got, gone) {
			t.Errorf("our trust entry %s survived:\n%s", gone, got)
		}
	}
	// Another repository's trust is not ours to withdraw.
	if !strings.Contains(got, "sha256:ccc") {
		t.Errorf("removed a trust entry for a different repository:\n%s", got)
	}
	// Neither is anything else in the user's config.
	for _, keep := range []string{`model = "gpt-5.5"`, "mcp_servers.playwright", `command = "npx"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("unrelated setting %q was lost:\n%s", keep, got)
		}
	}
	// Project trust is deliberately left: the user may have set it themselves.
	if !strings.Contains(got, `[projects."/some/repo"]`) {
		t.Errorf("project trust was withdrawn, which is not ours to decide:\n%s", got)
	}
	// And the result has to still be readable by Codex.
	if _, err := decodeTOML(raw); err != nil {
		t.Errorf("produced invalid TOML: %v\n%s", err, got)
	}
}

func TestUntrustIsANoOpWhenNothingIsOurs(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	seed := "model = \"gpt-5.5\"\n"
	if err := os.WriteFile(config, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	change, err := UntrustCodexConfig(config,
		[]CodexTrustEntry{{Key: "/gone/.codex/hooks.json:pre_tool_use:0:0", Hash: "sha256:x"}}, false)
	if err != nil {
		t.Fatalf("UntrustCodexConfig: %v", err)
	}
	if change.Changed() {
		t.Errorf("reported a change: %+v", change)
	}
	raw, _ := os.ReadFile(config)
	if string(raw) != seed {
		t.Errorf("modified a config with nothing of ours in it:\n%s", raw)
	}
}

func TestUntrustDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	seed := "[hooks.state.\"/some/repo/.codex/hooks.json:pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:aaa\"\n"
	if err := os.WriteFile(config, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	change, err := UntrustCodexConfig(config,
		[]CodexTrustEntry{{Key: "/some/repo/.codex/hooks.json:pre_tool_use:0:0", Hash: "sha256:aaa"}}, true)
	if err != nil {
		t.Fatalf("UntrustCodexConfig: %v", err)
	}
	if len(change.HooksRemoved) != 1 {
		t.Errorf("dry run reported %v, want the entry it would remove", change.HooksRemoved)
	}
	raw, _ := os.ReadFile(config)
	if string(raw) != seed {
		t.Errorf("dry run wrote to the file:\n%s", raw)
	}
}
