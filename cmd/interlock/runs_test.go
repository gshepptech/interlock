package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests start real agents. The agent is a fake, but everything around it
// is real: the binary, the detached supervisor, the process group, the socket,
// and the files on disk. That is the point. The failure this whole phase risks
// is a run that looks controlled and is not, and no amount of package-level
// testing can tell you whether a process actually detached.

const fakeAgentScript = `#!/bin/sh
cat > "$FAKE_PROMPT"
echo '{"type":"item.completed","item":{"type":"command"}}'
case "$FAKE_MODE" in
  quick)
    echo '{"type":"turn.completed"}'
    ;;
  linger)
    trap 'exit 0' INT
    while :; do sleep 0.05 & wait $!; done
    ;;
esac
`

type agentRepo struct {
	dir    string
	env    []string
	prompt string
}

func fakeAgentRepo(t *testing.T, mode string) agentRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed runs are a unix feature in this release")
	}
	dir := repo(t, defaultConfig)
	agent := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(agent, []byte(fakeAgentScript), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(t.TempDir(), "prompt-received")
	return agentRepo{
		dir:    dir,
		prompt: prompt,
		env: []string{
			"HOME=" + dir,
			"INTERLOCK_CODEX_BIN=" + agent,
			"FAKE_MODE=" + mode,
			"FAKE_PROMPT=" + prompt,
		},
	}
}

func (a agentRepo) run(t *testing.T, args ...string) result {
	t.Helper()
	return cliEnv(t, a.dir, a.env, args...)
}

// runIDFrom pulls the identifier out of the "run: ID" line the start command
// prints, because everything afterwards is addressed by it.
func runIDFrom(t *testing.T, res result) string {
	t.Helper()
	for line := range strings.SplitSeq(res.stdout, "\n") {
		if id, ok := strings.CutPrefix(line, "run: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("no run id in output:\n%s%s", res.stdout, res.stderr)
	return ""
}

func TestRunStartsAnAgentAndFollowsItToTheEnd(t *testing.T) {
	repo := fakeAgentRepo(t, "quick")
	res := repo.run(t, "run", "codex", "Implement the invitation API.",
		"--name", "api", "--env", "FAKE_MODE", "--env", "FAKE_PROMPT")
	if res.exit != 0 {
		t.Fatalf("run exited %d:\n%s%s", res.exit, res.stdout, res.stderr)
	}
	id := runIDFrom(t, res)

	// The prompt reached the agent on stdin rather than through its arguments.
	prompt, err := os.ReadFile(repo.prompt)
	if err != nil || string(prompt) != "Implement the invitation API." {
		t.Fatalf("prompt delivered = %q err=%v", prompt, err)
	}
	// Following printed what the agent said.
	if !strings.Contains(res.stdout, "turn.completed") {
		t.Errorf("output did not follow the agent:\n%s", res.stdout)
	}

	list := repo.run(t, "runs", "list")
	if !strings.Contains(list.stdout, id) || !strings.Contains(list.stdout, "completed") {
		t.Fatalf("runs list:\n%s", list.stdout)
	}
	show := repo.run(t, "runs", "show", id)
	for _, want := range []string{"completed", "codex", "api"} {
		if !strings.Contains(show.stdout, want) {
			t.Errorf("runs show is missing %q:\n%s", want, show.stdout)
		}
	}
	logs := repo.run(t, "runs", "logs", id)
	if !strings.Contains(logs.stdout, "item.completed") {
		t.Fatalf("runs logs:\n%s", logs.stdout)
	}

	// The action is in the audit trail, and the prompt is not.
	entries := journalEntries(t, repo.dir)
	found := false
	for _, entry := range entries {
		if entry["action"] == "run_start" && entry["target"] == id {
			found = true
			if entry["version"] != float64(3) {
				t.Errorf("run audit row version = %v, want 3", entry["version"])
			}
		}
		for _, value := range entry {
			if text, ok := value.(string); ok && strings.Contains(text, "invitation API") {
				t.Errorf("the prompt reached the journal: %q", text)
			}
		}
	}
	if !found {
		t.Fatalf("no run_start row in the journal: %#v", entries)
	}
}

// The claim that matters most: the agent belongs to a supervisor, not to the
// terminal that asked for it. The command below returns immediately and the
// agent is still there afterwards.
func TestARunOutlivesTheCommandThatStartedIt(t *testing.T) {
	repo := fakeAgentRepo(t, "linger")
	res := repo.run(t, "run", "codex", "keep going",
		"--env", "FAKE_MODE", "--env", "FAKE_PROMPT", "--no-follow")
	if res.exit != 0 {
		t.Fatalf("run exited %d:\n%s%s", res.exit, res.stdout, res.stderr)
	}
	id := runIDFrom(t, res)

	list := repo.run(t, "runs", "list")
	if !strings.Contains(list.stdout, "running") {
		t.Fatalf("the agent did not survive the command that started it:\n%s", list.stdout)
	}

	// Pause and resume reach the live process through its supervisor.
	if res := repo.run(t, "runs", "pause", id, "--confirm", id); res.exit != 0 {
		t.Fatalf("pause exited %d:\n%s%s", res.exit, res.stdout, res.stderr)
	}
	if list := repo.run(t, "runs", "list"); !strings.Contains(list.stdout, "paused") {
		t.Fatalf("runs list after pause:\n%s", list.stdout)
	}
	if res := repo.run(t, "runs", "resume", id); res.exit != 0 {
		t.Fatalf("resume exited %d:\n%s%s", res.exit, res.stdout, res.stderr)
	}

	if res := repo.run(t, "runs", "stop", id, "--confirm", id); res.exit != 0 {
		t.Fatalf("stop exited %d:\n%s%s", res.exit, res.stdout, res.stderr)
	}
	waitForTerminalRun(t, repo, id)

	// A finished run can be tried again, and the new run is a new record.
	retry := repo.run(t, "runs", "retry", id, "--confirm", id)
	if retry.exit != 0 {
		t.Fatalf("retry exited %d:\n%s%s", retry.exit, retry.stdout, retry.stderr)
	}
	second := runIDFrom(t, retry)
	if second == id {
		t.Error("retry reused the record; the failure is part of the history")
	}
	repo.run(t, "runs", "stop", second, "--confirm", second)
	waitForTerminalRun(t, repo, second)
}

func TestLifecycleCommandsInsistOnTheRunId(t *testing.T) {
	repo := fakeAgentRepo(t, "linger")
	res := repo.run(t, "run", "codex", "keep going", "--env", "FAKE_MODE", "--no-follow")
	id := runIDFrom(t, res)
	t.Cleanup(func() {
		repo.run(t, "runs", "stop", id, "--force", "--confirm", id)
	})

	for _, args := range [][]string{
		{"runs", "pause", id},
		{"runs", "stop", id, "--confirm", "something-else"},
		{"runs", "stop", id, "--force", "--confirm", "something-else"},
		{"runs", "retry", id, "--confirm", "something-else"},
	} {
		res := repo.run(t, args...)
		if res.exit == 0 {
			t.Errorf("%v succeeded without the run id", args)
		}
		if !strings.Contains(res.stdout+res.stderr, "confirmation") {
			t.Errorf("%v did not explain what was missing:\n%s%s", args, res.stdout, res.stderr)
		}
	}
}

// An option the agent cannot honour is refused before anything is created, so a
// typing mistake does not leave a failed run in the history.
func TestRunRefusesAnInteractiveAgentWithoutCreatingOne(t *testing.T) {
	repo := fakeAgentRepo(t, "quick")
	res := repo.run(t, "run", "codex", "talk to me", "--interactive")
	if res.exit == 0 {
		t.Fatalf("interactive run succeeded:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout+res.stderr, "interactively") {
		t.Errorf("the refusal did not say why:\n%s%s", res.stdout, res.stderr)
	}
	if list := repo.run(t, "runs", "list"); !strings.Contains(list.stdout, "no managed runs") {
		t.Fatalf("a refused launch left a record:\n%s", list.stdout)
	}
}

func TestRunsRejectsUnknownArgumentsAndRuns(t *testing.T) {
	repo := fakeAgentRepo(t, "quick")
	if res := repo.run(t, "runs", "show", "run-000000000000000000000000"); res.exit == 0 {
		t.Error("runs show of an unknown run succeeded")
	}
	if res := repo.run(t, "runs", "nonsense"); res.exit != exitUsage {
		t.Errorf("unknown subcommand exited %d, want usage", res.exit)
	}
	if res := repo.run(t, "run", "gemini", "go"); res.exit == 0 {
		t.Error("run with an unknown runner succeeded")
	}
}

func waitForTerminalRun(t *testing.T, repo agentRepo, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		show := repo.run(t, "runs", "show", id)
		for _, state := range []string{"completed", "failed", "lost", "stopped"} {
			if strings.Contains(show.stdout, "state: "+state) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never finished:\n%s", id, show.stdout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func journalEntries(t *testing.T, dir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".interlock", "journal.jsonl"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var entries []map[string]any
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse journal line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}
