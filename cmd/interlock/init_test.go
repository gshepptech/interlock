package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gshepptech/interlock/internal/install"
)

// matches applies a Claude Code hook matcher the way Claude Code does: a
// pipe-separated list of tool names, matched whole.
func matches(matcher, tool string) bool {
	if matcher == "" {
		return true // no matcher means every tool
	}
	return slices.Contains(strings.Split(matcher, "|"), tool)
}

// The gap the first of today's bugs fell through.
//
// Every other test here calls the hook with an event name already chosen, which
// skips the matcher entirely. So a matcher that never fires for the tool that
// matters looks perfect from the inside. This reads the settings init actually
// wrote and asks whether they would fire at all.
func TestInstalledConfigActuallyFiresForEveryWritingTool(t *testing.T) {
	dir := repo(t, defaultConfig)
	if res := cli(t, dir, "init"); res.exit > 1 {
		t.Fatalf("init failed: %s%s", res.stdout, res.stderr)
	}

	raw, err := os.ReadFile(claudeSettings(dir))
	if err != nil {
		t.Fatalf("read installed settings: %v", err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("parse installed settings: %v", err)
	}

	// Bash is the one that matters: it is how an agent reaches the disk without
	// using an edit tool, and it is what after-the-fact verification depends on.
	for _, tool := range []string{"Write", "Edit", "Bash"} {
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			fired := false
			for _, group := range settings.Hooks[event] {
				if !matches(group.Matcher, tool) {
					continue
				}
				for _, h := range group.Hooks {
					if strings.Contains(h.Command, " hook "+event+" claude") {
						fired = true
					}
				}
			}
			if !fired {
				t.Errorf("the installed %s hook never fires for %s. "+
					"Anything armed by PreToolUse and not seen by PostToolUse "+
					"is never checked.", event, tool)
			}
		}
	}
}

// claudeSettings asks the installer where Claude Code's hooks go, rather than
// repeating the path. When that moved from settings.json to settings.local.json
// the hardcoded copies in these tests were the only thing that broke.
func claudeSettings(root string) string {
	for _, target := range install.Plan(root) {
		if target.Agent == "claude" {
			return target.File
		}
	}
	panic("no claude target in the plan")
}

// The hooks name an absolute path to one machine's binary. settings.json is the
// file Claude Code treats as shareable and people commit, so installing there
// means everyone who pulls the repo runs a command that does not exist on their
// machine.
func TestClaudeHooksGoInTheLocalSettingsFile(t *testing.T) {
	dir := t.TempDir()
	file := claudeSettings(dir)

	if filepath.Base(file) != "settings.local.json" {
		t.Errorf("claude hooks install into %s, want settings.local.json",
			filepath.Base(file))
	}

	// And the superseded location must still be searched, or an install from an
	// older version keeps firing with nothing in the current file to explain it.
	var claude install.Target
	for _, target := range install.Plan(dir) {
		if target.Agent == "claude" {
			claude = target
		}
	}
	if len(claude.Superseded) == 0 {
		t.Fatal("no superseded paths, so an older install can never be found or removed")
	}
	if !slices.ContainsFunc(claude.Superseded, func(p string) bool {
		return filepath.Base(p) == "settings.json"
	}) {
		t.Errorf("Superseded = %v, want the old settings.json", claude.Superseded)
	}
}

// Uninstalling has to clean the old location too.
func TestUninstallRemovesHooksLeftInTheOldLocation(t *testing.T) {
	dir := repo(t, defaultConfig)
	old := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// An install as an older version would have left it.
	legacy := `{"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[` +
		`{"type":"command","command":"/old/path/interlock hook PreToolUse claude"}]}]}}`
	if err := os.WriteFile(old, []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if res := cli(t, dir, "init"); res.exit > 1 {
		t.Fatalf("init: %s%s", res.stdout, res.stderr)
	}
	if res := cli(t, dir, "doctor"); !strings.Contains(res.stdout, "older version") {
		t.Errorf("doctor does not report hooks left in the old file:\n%s", res.stdout)
	}

	res := cli(t, dir, "init", "--uninstall")
	if !strings.Contains(res.stdout, "older version") {
		t.Errorf("uninstall does not mention cleaning the old file:\n%s", res.stdout)
	}
	raw, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("read old settings: %v", err)
	}
	if strings.Contains(string(raw), "interlock hook") {
		t.Errorf("our hooks are still in the old file: %s", raw)
	}
}
