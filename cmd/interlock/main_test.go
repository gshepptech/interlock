package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run the real binary against real payloads in real repositories.
//
// That is deliberate, and it is a reaction to how the two worst bugs in this
// tool were found. Both lived in the wiring between packages, both were invisible
// to every unit test, and both took a live agent session to surface: a
// PostToolUse matcher that did not cover the tool PreToolUse had armed against,
// and a guard that blocked the very revert it had just demanded. Neither is
// reachable by testing a package in isolation, because neither was inside one.

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "interlock-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "interlock")

	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout string
	stderr string
	exit   int
}

// blocked is the only outcome that stops an agent, so it gets a name.
func (r result) blocked() bool { return r.exit == exitBlock }

const defaultConfig = `{
  "teams": {
    "backend":  {"owns": ["src/api/**"],        "shared": ["src/types/**"], "agents": ["claude"]},
    "frontend": {"owns": ["src/components/**"], "agents": ["codex"]}
  }
}`

// repo makes a git repository with a territory declaration. git is real here
// because the after-the-fact checks shell out to it.
func repo(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	// macOS temp dirs are symlinked through /private, and the hook resolves the
	// cwd it is given. Matching them keeps path comparisons honest.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	if err := os.WriteFile(filepath.Join(dir, ".interlock.json"),
		[]byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	for _, sub := range []string{"src/api", "src/components", "src/types"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if _, err := exec.LookPath("git"); err == nil {
		for _, args := range [][]string{
			{"init", "-q"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Run()
		}
	}
	return dir
}

func hook(t *testing.T, dir, event, agent string, payload map[string]any) result {
	t.Helper()
	payload["cwd"] = dir
	payload["hook_event_name"] = event

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	cmd := exec.Command(binPath, "hook", event, agent)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(raw))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	return res
}

// write is a PreToolUse payload for Claude Code's edit tool.
func write(session, path string) map[string]any {
	return map[string]any{
		"session_id": session,
		"tool_name":  "Write",
		"tool_input": map[string]any{"file_path": path, "content": "x"},
	}
}

// shell is a PreToolUse payload for a shell command.
func shell(session, command string) map[string]any {
	return map[string]any{
		"session_id": session,
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": command},
	}
}

func cli(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return res
}

// bare is a repo with no declaration at all. This is the ordinary case: the tool
// is meant to work without being configured.
func bare(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, sub := range []string{"src/api", "src/components"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = dir
		cmd.Run()
	}
	return dir
}

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
	// The message has to say it is about timing, or an agent reads it as a
	// permission problem and looks for another way in.
	for _, want := range []string{"src/api/users.go", "another agent", "timing"} {
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

func TestDoctorFailsOnAnUnwiredRepoAndNamesTheProblem(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := cli(t, dir, "doctor")
	if res.exit == 0 {
		t.Fatalf("doctor passed on a repo with no hooks wired:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "not wired up") {
		t.Errorf("doctor does not say the hooks are missing:\n%s", res.stdout)
	}
	// The silent failure is the one worth naming explicitly.
	if !strings.Contains(res.stdout, "trust") {
		t.Errorf("doctor does not mention Codex repo trust:\n%s", res.stdout)
	}
}

func TestInitThenDoctorAgreeOnWhatIsWired(t *testing.T) {
	dir := repo(t, defaultConfig)

	if res := cli(t, dir, "init"); res.exit > 1 {
		t.Fatalf("init failed: %s%s", res.stdout, res.stderr)
	}
	res := cli(t, dir, "doctor")
	for _, agent := range []string{"claude hooks", "codex hooks"} {
		line := agent + " "
		if !strings.Contains(res.stdout, line) {
			t.Errorf("doctor does not report %s:\n%s", agent, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "not wired up") {
		t.Errorf("doctor still reports missing hooks after init:\n%s", res.stdout)
	}
}

func TestDashboardRefusesANonLoopbackAddress(t *testing.T) {
	dir := repo(t, defaultConfig)

	res := cli(t, dir, "dashboard", "--addr", "0.0.0.0:7788")
	if res.exit == 0 {
		t.Fatal("the dashboard agreed to listen on 0.0.0.0")
	}
	if !strings.Contains(res.stderr, "loopback") {
		t.Errorf("refusal does not explain why: %s", res.stderr)
	}
}

func TestStatusShowsTerritoryAndHeldFiles(t *testing.T) {
	dir := repo(t, defaultConfig)
	hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/types/api.ts")))

	res := cli(t, dir, "status")
	for _, want := range []string{"backend", "src/api/**", "src/types/api.ts"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("status missing %q:\n%s", want, res.stdout)
		}
	}
}

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

	raw, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
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
					if strings.Contains(h.Command, "interlock hook") {
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
