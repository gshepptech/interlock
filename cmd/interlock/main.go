// Command interlock lets coding agents from different vendors work one repo
// without overwriting each other.
//
// It is invoked as a hook by each agent. On SessionStart it tells the agent what
// it owns and who else is here. On PreToolUse it blocks a write that lands in
// another team's territory, or on a shared file another agent currently holds,
// and explains why, so the agent redirects instead of failing.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/control"
)

const usage = `interlock - keep coding agents out of each other's way

  interlock hook <event> <agent>   run as an agent hook (reads payload on stdin)
  interlock codex [args...]        launch Codex so its hooks run and it is blocked
  interlock run <runner> "prompt"  start an agent Interlock owns and follow it
  interlock runs list|show|logs|pause|resume|stop|retry
                                   see and control the agents Interlock started
  interlock next                   what is workable now, if a work store says
  interlock check <path>...        may I edit this? exit 0 free, 2 held
  interlock status                 show territory and who holds what
  interlock cleanup [--dry-run]    remove state belonging to expired sessions
  interlock cleanup --session ID --confirm ID
                                   remove one expired session
  interlock lease release ID --confirm ID
                                   release one session's file holds
  interlock handoff request|list|show|resolve
                                   manage lightweight handoff requests
  interlock doctor [--fix] [--smoke]
                                   check, repair, and verify the installation
  interlock dashboard [--addr A]   serve a live view on 127.0.0.1 (default :7717)
  interlock init [--dry-run]       wire the hooks into each agent's settings
  interlock init --with-territory  also write a territory file, if you want rules
  interlock init --uninstall       take them back out

Events: SessionStart, PreToolUse, PostToolUse, SessionEnd, Stop
Agents: claude, codex
`

// Exit codes. 2 blocks the tool call on both Claude Code and Codex; verified
// against both.
const (
	exitOK    = 0
	exitBlock = 2
	exitUsage = 64
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}

	switch args[0] {
	case "hook":
		os.Exit(runHook(args[1:]))
	case "codex":
		os.Exit(runCodex(args[1:]))
	case "check":
		os.Exit(runCheck(args[1:]))
	case "status":
		os.Exit(runStatusCmd(args[1:]))
	case "cleanup":
		os.Exit(runCleanup(args[1:]))
	case "lease":
		os.Exit(runLease(args[1:]))
	case "handoff":
		os.Exit(runHandoff(args[1:]))
	case "doctor":
		os.Exit(runDoctor(args[1:]))
	case "dashboard":
		os.Exit(runDashboard(args[1:]))
	case "init":
		os.Exit(runInit(args[1:]))
	case "run":
		os.Exit(runRun(args[1:]))
	case "runs":
		os.Exit(runRuns(args[1:]))
	case "next":
		os.Exit(runNext(args[1:]))
	case "supervise":
		os.Exit(runSupervise(args[1:]))
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(exitUsage)
	}
}

// allow reports a problem without blocking. Always returns exitOK, so callers
// read as `return allow(...)` at every fail-open site.
func allow(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "interlock: "+format+" (allowing the call)\n", args...)
	return exitOK
}

func rel(root, path string) string {
	if trimmed, ok := strings.CutPrefix(path, root+"/"); ok {
		return trimmed
	}
	return path
}

func absAll(cwd string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
			continue
		}
		out = append(out, filepath.Join(cwd, p))
	}
	return out
}

func printControlResult(out, errOut io.Writer, result control.Result) {
	if result.Message != "" {
		fmt.Fprintln(out, result.Message)
	}
	if result.Warning != "" {
		fmt.Fprintln(errOut, "warning:", result.Warning)
	}
}

func currentRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return "", err
	}
	return cfg.Root, nil
}

func newControlService(root string) (control.Service, error) {
	executable, err := os.Executable()
	if err != nil {
		return control.Service{}, fmt.Errorf("cannot determine my own path: %w", err)
	}
	codexFile, err := codexConfigPath()
	if err != nil {
		return control.Service{}, fmt.Errorf("cannot locate Codex config: %w", err)
	}
	return control.Service{Root: root, Executable: executable, CodexFile: codexFile}, nil
}

func fallback(value, otherwise string) string {
	if value != "" {
		return value
	}
	return otherwise
}

func codexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}
