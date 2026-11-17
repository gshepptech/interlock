package main

import (
	"path/filepath"
	"testing"
)

// An agent must not be blocked by its own claim.
//
// With hooks live, the hook claims a file a moment before the model runs the
// check, so without this the agent reads its own claim and refuses its own
// work. A real Codex session did exactly that, reporting the file "held by
// another codex session" and declining to edit it.
func TestCheckIgnoresTheCallersOwnClaim(t *testing.T) {
	dir := bare(t)
	target := filepath.Join(dir, "src/api/users.go")

	// codex claims it, the way its PreToolUse hook would.
	if res := hook(t, dir, "PreToolUse", "codex", write("codex-session", target)); res.blocked() {
		t.Fatalf("claiming a free file was blocked: %s", res.stderr)
	}

	t.Run("a stranger is told it is held", func(t *testing.T) {
		res := cli(t, dir, "check", "src/api/users.go")
		if res.exit != exitBlock {
			t.Errorf("exit = %d, want %d\n%s", res.exit, exitBlock, res.stdout)
		}
	})

	t.Run("the same session is not", func(t *testing.T) {
		res := cliEnv(t, dir, []string{"INTERLOCK_SESSION=codex-session"},
			"check", "src/api/users.go")
		if res.exit != exitOK {
			t.Errorf("a session was blocked by its own claim: exit=%d\n%s",
				res.exit, res.stdout)
		}
	})

	t.Run("the same agent is not", func(t *testing.T) {
		res := cliEnv(t, dir, []string{"INTERLOCK_AGENT=codex"},
			"check", "src/api/users.go")
		if res.exit != exitOK {
			t.Errorf("an agent was blocked by its own claim: exit=%d\n%s",
				res.exit, res.stdout)
		}
	})

	t.Run("a different agent still is", func(t *testing.T) {
		res := cliEnv(t, dir, []string{"INTERLOCK_AGENT=claude"},
			"check", "src/api/users.go")
		if res.exit != exitBlock {
			t.Errorf("exit = %d, want another agent still blocked\n%s",
				res.exit, res.stdout)
		}
	})
}

func TestCheckReportsFreeFilesAndUsage(t *testing.T) {
	dir := bare(t)

	if res := cli(t, dir, "check", "src/api/anything.go"); res.exit != exitOK {
		t.Errorf("a free file reported exit=%d\n%s", res.exit, res.stdout)
	}
	if res := cli(t, dir, "check"); res.exit != exitUsage {
		t.Errorf("no arguments reported exit=%d, want usage", res.exit)
	}
}
