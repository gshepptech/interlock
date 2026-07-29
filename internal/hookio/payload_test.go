package hookio

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The four fixtures are payloads captured from real Claude Code and Codex
// sessions, with only the machine-specific paths shortened to /repo. If a
// vendor changes its envelope, these tests are how we find out.

func load(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestClaudeWriteNamesOneAbsoluteFile(t *testing.T) {
	ev, err := Parse(AgentClaude, load(t, "claude-pretooluse-write.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if ev.Name != "PreToolUse" {
		t.Errorf("Name = %q, want PreToolUse", ev.Name)
	}
	if ev.ToolName != "Write" {
		t.Errorf("ToolName = %q, want Write", ev.ToolName)
	}
	want := []string{"/repo/FORBIDDEN.txt"}
	if !slices.Equal(ev.Files, want) {
		t.Errorf("Files = %v, want %v", ev.Files, want)
	}
	if ev.Command != "" {
		t.Errorf("Command = %q, want empty for a file write", ev.Command)
	}
	// Claude Code sends no turn_id. Asserting it stays empty documents the
	// difference rather than leaving it as a surprise.
	if ev.TurnID != "" {
		t.Errorf("TurnID = %q, want empty for Claude Code", ev.TurnID)
	}
}

// The whole reason this package exists: Codex buries the path in a diff.
func TestCodexApplyPatchYieldsTheFileFromTheDiff(t *testing.T) {
	ev, err := Parse(AgentCodex, load(t, "codex-pretooluse-applypatch.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if ev.ToolName != "apply_patch" {
		t.Errorf("ToolName = %q, want apply_patch", ev.ToolName)
	}
	want := []string{"/repo/FORBIDDEN.txt"}
	if !slices.Equal(ev.Files, want) {
		t.Errorf("Files = %v, want %v", ev.Files, want)
	}
	// A patch that resolved to files must not also be reported as an opaque
	// command, or a caller would inspect it twice by two different rules.
	if ev.Command != "" {
		t.Errorf("Command = %q, want empty once the patch parsed", ev.Command)
	}
	if ev.TurnID == "" {
		t.Error("TurnID is empty, want the turn_id Codex sends")
	}
	if ev.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", ev.Model)
	}
}

// Both agents agree on this much, and the rest of the system leans on it.
func TestSessionStartCarriesSessionAndCWDForBothAgents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		agent   Agent
		fixture string
	}{
		{"claude", AgentClaude, "claude-sessionstart.json"},
		{"codex", AgentCodex, "codex-sessionstart.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := Parse(tc.agent, load(t, tc.fixture))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.Name != "SessionStart" {
				t.Errorf("Name = %q, want SessionStart", ev.Name)
			}
			if ev.SessionID == "" {
				t.Error("SessionID is empty")
			}
			if ev.CWD != "/repo" {
				t.Errorf("CWD = %q, want /repo", ev.CWD)
			}
			if len(ev.Files) != 0 {
				t.Errorf("Files = %v, want none on SessionStart", ev.Files)
			}
		})
	}
}

// One Codex patch can touch several files. A caller that assumed one file per
// call would wave through every file after the first.
func TestApplyPatchWithSeveralFiles(t *testing.T) {
	payload := []byte(`{
	  "cwd": "/repo",
	  "hook_event_name": "PreToolUse",
	  "tool_name": "apply_patch",
	  "tool_input": {"command": "*** Begin Patch\n*** Update File: src/api/users.go\n@@\n-old\n+new\n*** Add File: src/api/users_test.go\n+package api\n*** Delete File: src/api/legacy.go\n*** End Patch\n"}
	}`)

	ev, err := Parse(AgentCodex, payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []string{
		"/repo/src/api/users.go",
		"/repo/src/api/users_test.go",
		"/repo/src/api/legacy.go",
	}
	if !slices.Equal(ev.Files, want) {
		t.Errorf("Files = %v, want %v", ev.Files, want)
	}
}

// A rename touches the destination as much as the source. Blocking only the
// source would let an agent move a file out from under a peer that holds it.
func TestApplyPatchMoveClaimsBothPaths(t *testing.T) {
	payload := []byte(`{
	  "cwd": "/repo",
	  "tool_name": "apply_patch",
	  "tool_input": {"command": "*** Begin Patch\n*** Update File: old/name.go\n*** Move to: new/name.go\n@@\n-a\n+b\n*** End Patch\n"}
	}`)

	ev, err := Parse(AgentCodex, payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []string{"/repo/old/name.go", "/repo/new/name.go"}
	if !slices.Equal(ev.Files, want) {
		t.Errorf("Files = %v, want %v", ev.Files, want)
	}
}

// An agent can edit a file with sed as easily as with its patch tool. The shell
// command has to survive to the caller, or territory enforcement has a hole.
func TestShellCommandIsCarriedNotSilentlyDropped(t *testing.T) {
	payload := []byte(`{
	  "cwd": "/repo",
	  "tool_name": "shell",
	  "tool_input": {"command": "sed -i '' 's/a/b/' src/api/users.go"}
	}`)

	ev, err := Parse(AgentCodex, payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(ev.Files) != 0 {
		t.Errorf("Files = %v, want none: we do not guess paths out of shell", ev.Files)
	}
	if ev.Command == "" {
		t.Error("Command is empty, want the shell command preserved")
	}
}

// A patch envelope naming nothing is malformed. Reporting "no files" would read
// downstream as "safe to allow", so it degrades to an opaque command instead.
func TestEmptyPatchEnvelopeIsTreatedAsOpaqueCommand(t *testing.T) {
	payload := []byte(`{
	  "cwd": "/repo",
	  "tool_name": "apply_patch",
	  "tool_input": {"command": "*** Begin Patch\n*** End Patch\n"}
	}`)

	ev, err := Parse(AgentCodex, payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ev.Files) != 0 {
		t.Errorf("Files = %v, want none", ev.Files)
	}
	if ev.Command == "" {
		t.Error("Command is empty, want the unparseable patch preserved for inspection")
	}
}

func TestRelativePathIsResolvedAgainstCWD(t *testing.T) {
	payload := []byte(`{
	  "cwd": "/repo/sub",
	  "tool_name": "Write",
	  "tool_input": {"file_path": "../top.txt"}
	}`)

	ev, err := Parse(AgentClaude, payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"/repo/top.txt"}
	if !slices.Equal(ev.Files, want) {
		t.Errorf("Files = %v, want %v", ev.Files, want)
	}
}

// Failing open here would mean allowing a write we could not inspect.
func TestUnreadablePayloadIsAnError(t *testing.T) {
	if _, err := Parse(AgentClaude, []byte("not json")); err == nil {
		t.Fatal("Parse succeeded on garbage, want an error so callers cannot fail open")
	}
}

// tool_input is not an object for every tool. That is normal, not an error.
func TestNonObjectToolInputIsNotAnError(t *testing.T) {
	ev, err := Parse(AgentClaude, []byte(`{"tool_name":"Odd","tool_input":"a string"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ev.Files) != 0 || ev.Command != "" {
		t.Errorf("got Files=%v Command=%q, want both empty", ev.Files, ev.Command)
	}
}
