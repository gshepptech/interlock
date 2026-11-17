package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd.Env = append(os.Environ(), "HOME="+dir)
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

// cliEnv runs the binary with extra environment.
func cliEnv(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
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
