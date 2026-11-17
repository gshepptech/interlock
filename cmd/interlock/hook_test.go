package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Working alone in a repo nobody configured must be completely quiet. If the
// tool chatters when there is no conflict, it is noise on every tool call.
func TestUnconfiguredRepoIsSilentWhenNobodyElseIsThere(t *testing.T) {
	dir := bare(t)

	res := hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/api/users.go")))

	if res.exit != exitOK {
		t.Errorf("exit = %d, want 0 working alone", res.exit)
	}
	if res.stderr != "" || res.stdout != "" {
		t.Errorf("said something with no conflict: out=%q err=%q",
			res.stdout, res.stderr)
	}
}

// The whole point of the redesign: two agents coordinate with no configuration.
func TestTwoAgentsCoordinateWithNoConfigAtAll(t *testing.T) {
	dir := bare(t)
	contested := filepath.Join(dir, "src/api/users.go")

	first := hook(t, dir, "PreToolUse", "claude", write("claude-session", contested))
	if first.blocked() {
		t.Fatalf("the first agent into a free file was blocked: %s", first.stderr)
	}

	second := hook(t, dir, "PreToolUse", "codex", write("codex-session", contested))
	if !second.blocked() {
		t.Fatalf("exit = %d, want the second agent blocked from a file being worked on",
			second.exit)
	}
	// Naming the holder gives the blocked agent something to act on, and saying
	// it is about timing stops it reading the refusal as a permission problem
	// and looking for another way in.
	for _, want := range []string{"src/api/users.go", "claude", "timing"} {
		if !strings.Contains(second.stderr, want) {
			t.Errorf("block message missing %q: %s", want, second.stderr)
		}
	}
}

// Coordination must not mean queueing. Agents working in different places should
// never see each other's blocks.
func TestDifferentFilesDoNotContend(t *testing.T) {
	dir := bare(t)

	a := hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/api/users.go")))
	b := hook(t, dir, "PreToolUse", "codex",
		write("s2", filepath.Join(dir, "src/components/Card.tsx")))

	if a.blocked() || b.blocked() {
		t.Errorf("two agents in different files contended: a=%q b=%q", a.stderr, b.stderr)
	}
}

// An agent that knows where its peer is will usually stay out of the way without
// ever being refused. This is the half that needs no rules.
func TestSessionStartTellsAnAgentWhoElseIsWorking(t *testing.T) {
	dir := bare(t)

	// Codex arrives first and works in src/components.
	hook(t, dir, "PreToolUse", "codex",
		write("codex-session", filepath.Join(dir, "src/components/Card.tsx")))
	hook(t, dir, "PostToolUse", "codex",
		write("codex-session", filepath.Join(dir, "src/components/Card.tsx")))

	res := hook(t, dir, "SessionStart", "claude",
		map[string]any{"session_id": "claude-session"})

	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("no injection envelope: %v\n%s", err, res.stdout)
	}
	context := out.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"codex", "src/components"} {
		if !strings.Contains(context, want) {
			t.Errorf("the arriving agent is not told about %q:\n%s", want, context)
		}
	}
}

// A session must never be told it is contending with itself.
func TestASessionIsNotWarnedAboutItself(t *testing.T) {
	dir := bare(t)

	hook(t, dir, "PreToolUse", "claude",
		write("only-session", filepath.Join(dir, "src/api/users.go")))
	hook(t, dir, "PostToolUse", "claude",
		write("only-session", filepath.Join(dir, "src/api/users.go")))

	res := hook(t, dir, "SessionStart", "claude",
		map[string]any{"session_id": "only-session"})

	if strings.Contains(res.stdout, "Other agents are working") {
		t.Errorf("a lone session was told it has company:\n%s", res.stdout)
	}
}

func TestOwnTerritoryIsAllowedAndOthersAreBlocked(t *testing.T) {
	dir := repo(t, defaultConfig)

	own := hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/api/users.go")))
	if own.blocked() {
		t.Errorf("blocked in its own territory: %s", own.stderr)
	}

	other := hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/components/Button.tsx")))
	if !other.blocked() {
		t.Fatalf("exit = %d, want %d for another team's territory", other.exit, exitBlock)
	}
	// The message is the product. A refusal an agent cannot act on is a failure.
	for _, want := range []string{"src/components/Button.tsx", "frontend", "backend"} {
		if !strings.Contains(other.stderr, want) {
			t.Errorf("block message missing %q: %s", want, other.stderr)
		}
	}
}

func TestSessionStartInjectsTheTerritoryMap(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := hook(t, dir, "SessionStart", "claude", map[string]any{"session_id": "s1"})
	if res.exit != exitOK {
		t.Fatalf("exit = %d, want 0", res.exit)
	}

	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("stdout is not the injection envelope: %v\n%s", err, res.stdout)
	}
	// A mismatched event name makes the agent ignore the payload entirely.
	if out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart",
			out.HookSpecificOutput.HookEventName)
	}
	for _, want := range []string{"backend", "src/api/**", "frontend"} {
		if !strings.Contains(out.HookSpecificOutput.AdditionalContext, want) {
			t.Errorf("injected context missing %q: %s",
				want, out.HookSpecificOutput.AdditionalContext)
		}
	}
}

func TestSharedFileIsHeldByOneSessionAtATime(t *testing.T) {
	dir := repo(t, defaultConfig)
	shared := filepath.Join(dir, "src/types/api.ts")

	first := hook(t, dir, "PreToolUse", "claude", write("s1", shared))
	if first.blocked() {
		t.Fatalf("first session blocked from a free shared file: %s", first.stderr)
	}

	// Re-entry is the normal case: one agent editing across several tool calls.
	again := hook(t, dir, "PreToolUse", "claude", write("s1", shared))
	if again.blocked() {
		t.Errorf("a session was blocked from a file it already holds: %s", again.stderr)
	}

	// A different session, even on the same team, has to wait.
	second := hook(t, dir, "PreToolUse", "claude", write("s2", shared))
	if !second.blocked() {
		t.Fatalf("exit = %d, want the second session blocked", second.exit)
	}
	if !strings.Contains(second.stderr, "src/types/api.ts") {
		t.Errorf("block message does not name the file: %s", second.stderr)
	}
}

func TestLeaseIsReleasedOnSessionEnd(t *testing.T) {
	dir := repo(t, defaultConfig)
	shared := filepath.Join(dir, "src/types/api.ts")

	hook(t, dir, "PreToolUse", "claude", write("s1", shared))
	hook(t, dir, "SessionEnd", "claude", map[string]any{"session_id": "s1"})

	after := hook(t, dir, "PreToolUse", "claude", write("s2", shared))
	if after.blocked() {
		t.Errorf("the file is still held after its session ended: %s", after.stderr)
	}
}

func TestShellWriteIntoAnotherTerritoryIsBlocked(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := hook(t, dir, "PreToolUse", "claude",
		shell("s1", "sed -i '' 's/a/b/' src/components/Button.tsx"))

	if !res.blocked() {
		t.Fatalf("exit = %d, want a shell write into another territory blocked", res.exit)
	}
	if !strings.Contains(res.stderr, "src/components/Button.tsx") {
		t.Errorf("block message does not name the file: %s", res.stderr)
	}
}

// Blocking reads would stop normal work and the tool would be switched off.
func TestReadOnlyShellCommandsAreNotBlocked(t *testing.T) {
	dir := repo(t, defaultConfig)

	for _, command := range []string{
		"ls -la src/components",
		"cat src/components/Button.tsx",
		"grep -rn func src/components",
		"git status",
	} {
		res := hook(t, dir, "PreToolUse", "claude", shell("s1", command))
		if res.blocked() {
			t.Errorf("blocked a read: %q\n%s", command, res.stderr)
		}
	}
}

// The whole after-the-fact path, end to end through the real binary. This is the
// wiring that was silently dead: PreToolUse armed a snapshot and PostToolUse
// never ran because its matcher did not cover Bash.
func TestOpaqueWriteIsCaughtAfterTheFactAndCanThenBeReverted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := repo(t, defaultConfig)
	const session = "s1"
	// A command whose target cannot be read from the command line.
	opaque := shell(session, `python3 -c "..." > $TARGET`)

	armed := hook(t, dir, "PreToolUse", "claude", opaque)
	if armed.blocked() {
		t.Fatalf("an opaque command was blocked outright: %s", armed.stderr)
	}

	// The command runs and writes where it should not have.
	escaped := filepath.Join(dir, "src/components/Widget.tsx")
	if err := os.WriteFile(escaped, []byte("stolen"), 0o644); err != nil {
		t.Fatalf("simulate the write: %v", err)
	}

	checked := hook(t, dir, "PostToolUse", "claude", opaque)
	if !strings.Contains(checked.stdout, "src/components/Widget.tsx") {
		t.Fatalf("PostToolUse did not report the escaped write.\nout=%q\nerr=%q",
			checked.stdout, checked.stderr)
	}
	// It has to reach the model, not just stderr, or the agent cannot act on it.
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(checked.stdout), &out); err != nil {
		t.Fatalf("PostToolUse output is not an injection envelope: %v\n%s",
			err, checked.stdout)
	}
	if out.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Errorf("hookEventName = %q, want PostToolUse, or the agent ignores it",
			out.HookSpecificOutput.HookEventName)
	}

	// The bug that followed: the tool demanded a fix and then blocked the fix.
	revert := hook(t, dir, "PreToolUse", "claude",
		shell(session, "rm src/components/Widget.tsx"))
	if revert.blocked() {
		t.Fatalf("the revert this tool asked for was blocked by this tool: %s",
			revert.stderr)
	}

	// The grant must be no wider than the mistake.
	neighbour := hook(t, dir, "PreToolUse", "claude",
		write(session, filepath.Join(dir, "src/components/Other.tsx")))
	if !neighbour.blocked() {
		t.Error("the remediation grant leaked to a neighbouring path")
	}
	elsewhere := hook(t, dir, "PreToolUse", "claude",
		shell("different-session", "rm src/components/Widget.tsx"))
	if !elsewhere.blocked() {
		t.Error("the remediation grant leaked to another session")
	}
}

// The journal is what makes the tool's work visible, so a block that is not
// recorded is a block nobody can see.
func TestBlocksAreRecordedInTheJournal(t *testing.T) {
	dir := repo(t, defaultConfig)

	hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/components/Button.tsx")))

	raw, err := os.ReadFile(filepath.Join(dir, ".interlock", "journal.jsonl"))
	if err != nil {
		t.Fatalf("no journal was written: %v", err)
	}
	if !strings.Contains(string(raw), "blocked_territory") ||
		!strings.Contains(string(raw), "src/components/Button.tsx") {
		t.Errorf("journal does not record the block: %s", raw)
	}
}

// The rule the whole design rests on: only a real violation stops work.
func TestBadInputAllowsTheCallInsteadOfBlockingIt(t *testing.T) {
	dir := repo(t, defaultConfig)

	t.Run("unreadable payload", func(t *testing.T) {
		cmd := exec.Command(binPath, "hook", "PreToolUse", "claude")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("this is not json")
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() == exitBlock {
			t.Errorf("a malformed payload blocked the call: %s", out)
		}
	})

	t.Run("unparseable config", func(t *testing.T) {
		broken := repo(t, `{"teams": {oh dear`)
		res := hook(t, broken, "PreToolUse", "claude",
			write("s1", filepath.Join(broken, "src/api/users.go")))
		if res.blocked() {
			t.Errorf("a broken config blocked the call: %s", res.stderr)
		}
		if res.stderr == "" {
			t.Error("a broken config was allowed silently, so nobody would ever fix it")
		}
	})
}

// A session with no team has nothing to enforce against, and must not be stopped.
func TestAgentWithNoTeamIsNotBlocked(t *testing.T) {
	dir := repo(t, `{"teams": {"backend": {"owns": ["src/api/**"], "agents": ["claude"]}}}`)

	res := hook(t, dir, "PreToolUse", "cursor",
		write("s1", filepath.Join(dir, "src/api/users.go")))
	if res.blocked() {
		t.Errorf("an agent with no team was blocked: %s", res.stderr)
	}
}

// The environment override is the only way to put two sessions of one vendor on
// different teams.
func TestTeamEnvOverrideChangesEnforcement(t *testing.T) {
	dir := repo(t, defaultConfig)
	payload := write("s1", filepath.Join(dir, "src/components/Button.tsx"))
	payload["cwd"] = dir
	payload["hook_event_name"] = "PreToolUse"
	raw, _ := json.Marshal(payload)

	cmd := exec.Command(binPath, "hook", "PreToolUse", "claude")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "INTERLOCK_TEAM=frontend")
	cmd.Stdin = strings.NewReader(string(raw))
	out, _ := cmd.CombinedOutput()

	if cmd.ProcessState.ExitCode() == exitBlock {
		t.Errorf("claude on the frontend team was blocked from frontend files: %s", out)
	}
}

// Every vendor-specific bug this tool has had began with an envelope changing
// shape, and a hook payload cannot be reproduced by hand: it arrives on stdin
// inside the agent's own process tree. This is how the next one gets caught.
func TestHookPayloadsCanBeCapturedForDiagnosis(t *testing.T) {
	dir := repo(t, defaultConfig)
	dump := filepath.Join(t.TempDir(), "payloads.jsonl")

	payload := `{"session_id":"s1","cwd":"` + dir +
		`","hook_event_name":"PreToolUse","tool_name":"Write",` +
		`"tool_input":{"file_path":"` + dir + `/src/api/users.go","content":"x"}}`
	cmd := exec.Command(binPath, "hook", "PreToolUse", "claude")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "INTERLOCK_HOOK_DUMP="+dump)
	cmd.Stdin = strings.NewReader(payload)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run hook: %v", err)
		}
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var record struct {
		At      string          `json:"at"`
		Agent   string          `json:"agent"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &record); err != nil {
		t.Fatalf("parse dump %q: %v", raw, err)
	}
	if record.Agent != "claude" || record.Event != "PreToolUse" || record.At == "" {
		t.Fatalf("record = %#v", record)
	}
	// The payload is kept exactly as it arrived. A capture that reformatted it
	// would be useless for finding out what an agent actually sent.
	if string(record.Payload) != payload {
		t.Fatalf("payload = %s\nwant %s", record.Payload, payload)
	}

	// Nothing is written when nobody asked for it.
	quiet := exec.Command(binPath, "hook", "PreToolUse", "claude")
	quiet.Dir = dir
	quiet.Env = append(os.Environ(), "HOME="+dir)
	quiet.Stdin = strings.NewReader(payload)
	_ = quiet.Run()
	entries, err := os.ReadDir(filepath.Dir(dump))
	if err != nil || len(entries) != 1 {
		t.Fatalf("dump directory holds %d files, want 1 (err=%v)", len(entries), err)
	}
}
