// Package hookio turns a coding agent's hook payload into one shape.
//
// Every agent hands us a different envelope on stdin. Claude Code names the
// file it is about to write in tool_input.file_path, absolute and ready to use.
// Codex hands over an apply_patch blob with the paths embedded in diff headers,
// relative to cwd, and one patch can touch several files at once. Both were
// captured from real sessions; the fixtures in testdata are those payloads.
//
// Nothing agent-specific may escape this package. Everything downstream works
// on Event.
package hookio

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Agent identifies which tool invoked us. The payload does not say, because no
// vendor has a reason to tell us, so the hook config passes it as an argument.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// Event is the normalised form of one hook invocation.
type Event struct {
	Agent     Agent
	Name      string // SessionStart, PreToolUse, PostToolUse
	SessionID string
	TurnID    string // Codex only; empty for Claude Code
	CWD       string
	Model     string
	ToolName  string

	// Files the tool is about to touch, absolute and cleaned. Empty for events
	// that touch no files.
	Files []string

	// Command is the shell command when the tool runs one. An agent can edit a
	// file through the shell as easily as through its patch tool, so a caller
	// that only inspects Files has a hole in it.
	Command string

	Raw map[string]any
}

// wire is the union of fields the agents actually send. Both agree on
// session_id, cwd, hook_event_name, tool_name and tool_input, which is enough
// common ground to decode once.
type wire struct {
	SessionID     string          `json:"session_id"`
	TurnID        string          `json:"turn_id"`
	CWD           string          `json:"cwd"`
	Model         string          `json:"model"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// toolInput covers every field either agent uses to name a target. They are
// mutually exclusive in practice, so decoding them all and picking what is set
// avoids branching on tool names we have not seen yet.
type toolInput struct {
	FilePath     string `json:"file_path"`     // Claude: Write, Edit
	NotebookPath string `json:"notebook_path"` // Claude: NotebookEdit
	Command      string `json:"command"`       // Claude: Bash. Codex: apply_patch, shell
}

// Parse decodes a hook payload. A payload we cannot decode is an error rather
// than an empty Event, because the caller's whole job is deciding whether to
// block a write, and silently treating an unreadable payload as "touches no
// files" would fail open.
func Parse(agent Agent, raw []byte) (*Event, error) {
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("decode hook payload: %w", err)
	}

	ev := &Event{
		Agent:     agent,
		Name:      w.HookEventName,
		SessionID: w.SessionID,
		TurnID:    w.TurnID,
		CWD:       w.CWD,
		Model:     w.Model,
		ToolName:  w.ToolName,
	}

	if err := json.Unmarshal(raw, &ev.Raw); err != nil {
		return nil, fmt.Errorf("decode hook payload into map: %w", err)
	}

	if len(w.ToolInput) == 0 {
		return ev, nil
	}

	var in toolInput
	if err := json.Unmarshal(w.ToolInput, &in); err != nil {
		// tool_input is not an object for every tool. That is not fatal: it
		// means this call names no file we can read, and the caller should see
		// an Event with no Files rather than an error.
		return ev, nil
	}

	switch {
	case in.FilePath != "":
		ev.Files = []string{absolute(w.CWD, in.FilePath)}
	case in.NotebookPath != "":
		ev.Files = []string{absolute(w.CWD, in.NotebookPath)}
	case in.Command != "":
		if paths, ok := parseApplyPatch(in.Command); ok {
			ev.Files = absoluteAll(w.CWD, paths)
		} else {
			// A real shell command. We deliberately do not guess which files it
			// touches; the caller decides how paranoid to be.
			ev.Command = in.Command
		}
	}

	return ev, nil
}

// patch header prefixes, in the apply_patch envelope Codex sends.
const (
	patchBegin  = "*** Begin Patch"
	patchAdd    = "*** Add File: "
	patchUpdate = "*** Update File: "
	patchDelete = "*** Delete File: "
	patchMove   = "*** Move to: "
)

// parseApplyPatch pulls every path out of an apply_patch blob. It reports false
// when the command is not a patch at all, which is how a shell command is told
// apart from a patch without consulting the tool name: Codex has changed that
// name before and will again.
//
// A move counts as touching both the old path and the new one. Blocking only
// the source would let an agent rename a file out from under a peer.
func parseApplyPatch(command string) ([]string, bool) {
	if !strings.Contains(command, patchBegin) {
		return nil, false
	}

	var paths []string
	seen := make(map[string]bool)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}

	for line := range strings.SplitSeq(command, "\n") {
		switch {
		case strings.HasPrefix(line, patchAdd):
			add(strings.TrimPrefix(line, patchAdd))
		case strings.HasPrefix(line, patchUpdate):
			add(strings.TrimPrefix(line, patchUpdate))
		case strings.HasPrefix(line, patchDelete):
			add(strings.TrimPrefix(line, patchDelete))
		case strings.HasPrefix(line, patchMove):
			add(strings.TrimPrefix(line, patchMove))
		}
	}

	// A patch envelope naming no files is malformed. Report it as not-a-patch
	// so the caller falls back to treating it as an opaque command rather than
	// concluding it is safe.
	if len(paths) == 0 {
		return nil, false
	}
	return paths, true
}

func absolute(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

func absoluteAll(cwd string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, absolute(cwd, p))
	}
	return out
}
