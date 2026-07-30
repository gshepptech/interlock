// Package install wires the hooks into each agent's own settings.
//
// These are files the user owns and may have edited by hand, so three properties
// matter more than convenience:
//
//   - Merge, never replace. Everything already in the file survives.
//   - Idempotent. Running init twice does not install the hooks twice.
//   - Atomic. A crash mid-write cannot leave an agent with unparseable settings,
//     which would stop it starting at all.
//
// A backup is written the first time a file is modified, because the failure this
// cannot rule out is a merge that is valid JSON and still wrong.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// marker identifies entries this tool owns. Matching on the subcommand rather
// than on the full path means moving the binary does not orphan its entries.
const marker = "agentcoord hook"

// Target is one agent's settings file and the hooks that belong in it.
type Target struct {
	Agent string
	File  string
	// Events maps a hook event to the matcher it needs. An empty matcher means
	// the agent does not support them, or that every tool should fire.
	Events map[string]string
}

// Plan describes what wiring each agent needs.
//
// Claude Code matches hooks by tool name, so PreToolUse has to name Bash as well
// as the edit tools: an agent that shells out to sed would otherwise skip the
// check entirely. Codex fires its hooks for every tool, so it needs no matcher.
//
// Codex exposes no session-end event, which is why it gets none here and why its
// leases are freed by the time to live instead.
func Plan(root string) []Target {
	return []Target{
		{
			Agent: "claude",
			File:  filepath.Join(root, ".claude", "settings.json"),
			Events: map[string]string{
				"SessionStart": "",
				"PreToolUse":   "Write|Edit|MultiEdit|NotebookEdit|Bash",
				"PostToolUse":  "Write|Edit|MultiEdit|NotebookEdit",
				"SessionEnd":   "",
			},
		},
		{
			Agent: "codex",
			File:  filepath.Join(root, ".codex", "hooks.json"),
			Events: map[string]string{
				"SessionStart": "",
				"PreToolUse":   "",
				"PostToolUse":  "",
			},
		},
	}
}

// Change reports what happened to one file.
type Change struct {
	File    string
	Added   []string
	Removed []string
	Backup  string
}

// Changed reports whether anything was written.
func (c Change) Changed() bool { return len(c.Added) > 0 || len(c.Removed) > 0 }

// Apply merges the target's hooks into its settings file. It reports what it
// added; an empty result means everything was already wired.
//
// dryRun computes the same answer without writing, so a user can see the change
// before it happens to a file they own.
func Apply(t Target, exe string, dryRun bool) (Change, error) {
	change := Change{File: t.File}

	doc, existed, err := read(t.File)
	if err != nil {
		return change, err
	}

	hooks := childObject(doc, "hooks")

	for event, matcher := range t.Events {
		command := fmt.Sprintf("%s hook %s %s", exe, event, t.Agent)

		list, _ := hooks[event].([]any)
		if hasMarker(list) {
			continue
		}
		hooks[event] = append(list, entry(matcher, command))
		change.Added = append(change.Added, event)
	}

	sortStrings(change.Added)
	if !change.Changed() || dryRun {
		return change, nil
	}

	doc["hooks"] = hooks
	if existed {
		backup, err := backupOnce(t.File)
		if err != nil {
			return change, err
		}
		change.Backup = backup
	}
	if err := write(t.File, doc); err != nil {
		return change, err
	}
	return change, nil
}

// Remove takes this tool's hooks back out and leaves everything else alone.
// Anything that installs into a user's file owes them an uninstall.
func Remove(t Target, dryRun bool) (Change, error) {
	change := Change{File: t.File}

	doc, existed, err := read(t.File)
	if err != nil || !existed {
		return change, err
	}

	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return change, nil
	}

	for event := range t.Events {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(list))
		for _, item := range list {
			if entryHasMarker(item) {
				continue
			}
			kept = append(kept, item)
		}
		if len(kept) == len(list) {
			continue
		}
		change.Removed = append(change.Removed, event)
		if len(kept) == 0 {
			// An empty array left behind is litter in someone else's file.
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}

	sortStrings(change.Removed)
	if !change.Changed() || dryRun {
		return change, nil
	}

	if len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return change, write(t.File, doc)
}

// entry builds one hook entry in the shape both agents accept. The nesting is
// theirs, not ours: a list of matcher groups, each holding a list of commands.
func entry(matcher, command string) map[string]any {
	e := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
	if matcher != "" {
		e["matcher"] = matcher
	}
	return e
}

func hasMarker(list []any) bool {
	return slices.ContainsFunc(list, entryHasMarker)
}

// entryHasMarker looks for our command anywhere inside an entry, so a
// hand-reordered or hand-edited entry is still recognised as ours.
func entryHasMarker(item any) bool {
	group, ok := item.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := group["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hook, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hook["command"].(string); ok && strings.Contains(cmd, marker) {
			return true
		}
	}
	return false
}

// read loads a settings file. A missing file is not an error: it means we are the
// first to write one.
func read(file string) (doc map[string]any, existed bool, err error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", file, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, true, nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Refusing is the only safe move. Overwriting settings we cannot parse
		// would throw away work the user did by hand.
		return nil, true, fmt.Errorf(
			"%s is not valid JSON, so it will not be modified: %w", file, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, true, nil
}

func childObject(doc map[string]any, key string) map[string]any {
	if existing, ok := doc[key].(map[string]any); ok {
		return existing
	}
	return map[string]any{}
}

func write(file string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(file), err)
	}

	enc, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", file, err)
	}
	enc = append(enc, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(file), ".agentcoord-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install %s: %w", file, err)
	}
	return nil
}

// backupOnce copies the file aside the first time we touch it, and never
// overwrites an existing backup. A second run must not clobber the copy of the
// file as it was before this tool ever ran.
func backupOnce(file string) (string, error) {
	backup := file + ".agentcoord-backup"
	if _, err := os.Stat(backup); err == nil {
		return backup, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s for backup: %w", file, err)
	}
	if err := os.WriteFile(backup, raw, 0o644); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return backup, nil
}

// Gitignore adds a line to the repo's .gitignore if it is not already covered.
// Leases and session state are local runtime state, and a tool that quietly adds
// untracked files to someone's repo without saying so is a nuisance.
func Gitignore(root, line string) (bool, error) {
	file := filepath.Join(root, ".gitignore")

	raw, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read .gitignore: %w", err)
	}
	for existing := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(existing) == line {
			return false, nil
		}
	}

	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		return false, fmt.Errorf("write .gitignore: %w", err)
	}
	return true, nil
}

func sortStrings(s []string) { slices.Sort(s) }
