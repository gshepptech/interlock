// Command agentcoord lets coding agents from different vendors work one repo
// without overwriting each other.
//
// It is invoked as a hook by each agent. On SessionStart it tells the agent who
// else is here and what it owns. On PreToolUse it blocks a write that lands in
// another team's territory, and explains why, so the agent redirects instead of
// failing.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"agentcoord/internal/config"
	"agentcoord/internal/hookio"
	"agentcoord/internal/territory"
)

const usage = `agentcoord - keep coding agents out of each other's way

  agentcoord hook <event> <agent>   run as an agent hook (reads payload on stdin)
  agentcoord status                 show the territory map for this repo
  agentcoord init                   write starter config and hook wiring

Events: SessionStart, PreToolUse, PostToolUse
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
	case "status":
		os.Exit(runStatus())
	case "init":
		os.Exit(runInit())
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(exitUsage)
	}
}

// runHook is the hot path. Its one hard rule: this tool must never be the reason
// someone's work stopped. A territory violation is the only thing that blocks.
// Anything else that goes wrong, a payload we cannot read, a config we cannot
// parse, a repo with no config at all, allows the call and says so on stderr.
// The cost of allowing is a collision the user could already have had. The cost
// of blocking on our own bug is a session that cannot write to its own repo.
func runHook(args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, "usage: agentcoord hook <event> <agent>\n")
		return exitUsage
	}
	event, agent := args[0], hookio.Agent(args[1])

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return allow("could not read hook payload: %v", err)
	}

	ev, err := hookio.Parse(agent, raw)
	if err != nil {
		return allow("could not parse hook payload: %v", err)
	}
	// The hook config names the event; the payload also carries it. Trust the
	// argument, since a matcher can fire a hook for an event the payload labels
	// differently and the config is the thing we control.
	if ev.Name == "" {
		ev.Name = event
	}

	cfg, err := config.Load(ev.CWD)
	if errors.Is(err, config.ErrNotFound) {
		// Not configured. Behave exactly as if this tool were not installed.
		return exitOK
	}
	if err != nil {
		return allow("could not load config: %v", err)
	}

	team := cfg.TeamFor(string(agent))

	switch event {
	case "SessionStart":
		emitContext(cfg.Summary(team))
		return exitOK
	case "PreToolUse":
		return checkWrite(cfg, team, ev)
	default:
		return exitOK
	}
}

// checkWrite blocks the call if any file it touches belongs to another team.
// Every file is checked, not just the first: one Codex patch can touch several,
// and stopping at the first allowed one would wave the rest through.
func checkWrite(cfg *config.Config, team string, ev *hookio.Event) int {
	if team == "" {
		// This session declared no team, so there is nothing to enforce against.
		return exitOK
	}

	for _, file := range ev.Files {
		decision, pattern, err := cfg.Map.Check(team, file)
		if err != nil {
			// Either the path is outside the repo, which is not ours to police,
			// or the config claims one path for two teams. Neither is grounds
			// for stopping the user; say it and move on.
			return allow("%v", err)
		}
		if decision != territory.Denied {
			continue
		}
		owner := cfg.Map.Owner(file)
		fmt.Fprintf(os.Stderr,
			"Blocked by agentcoord: %s belongs to the %q team (matched %q), "+
				"and you are on %q. Another agent may be working there right now. "+
				"Work inside your own territory, or ask for this file to be shared.\n",
			rel(cfg.Root, file), owner, pattern, team)
		return exitBlock
	}

	// A shell command could touch anything, and we deliberately do not guess
	// which paths are in it. Saying so is more honest than implying coverage
	// this version does not have.
	if ev.Command != "" && len(ev.Files) == 0 {
		fmt.Fprintf(os.Stderr,
			"agentcoord: shell command not checked against territory. "+
				"Files edited through the shell are not yet covered.\n")
	}
	return exitOK
}

// emitContext returns text both agents inject into the model's context. The
// shape is identical for Claude Code and Codex; verified against both.
func emitContext(text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": text,
		},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		// Nothing useful to say to the model, and no reason to fail the session.
		return
	}
	fmt.Println(string(enc))
}

// allow reports a problem without blocking. Always returns exitOK, so callers
// read as `return allow(...)` at every fail-open site.
func allow(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "agentcoord: "+format+" (allowing the call)\n", args...)
	return exitOK
}

func rel(root, path string) string {
	if trimmed, ok := strings.CutPrefix(path, root+"/"); ok {
		return trimmed
	}
	return path
}

func runStatus() int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	cfg, err := config.Load(cwd)
	if errors.Is(err, config.ErrNotFound) {
		fmt.Printf("No %s in this repo or any parent. Run: agentcoord init\n",
			config.FileName)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Printf("repo: %s\n\n", cfg.Root)
	for _, t := range cfg.Map.Teams {
		fmt.Printf("%s\n", t.Name)
		for _, p := range t.Owns {
			fmt.Printf("  owns    %s\n", p)
		}
		for _, p := range t.Shared {
			fmt.Printf("  shared  %s\n", p)
		}
	}
	for _, agent := range []string{"claude", "codex"} {
		if team := cfg.TeamFor(agent); team != "" {
			fmt.Printf("\n%s defaults to team %q\n", agent, team)
		}
	}
	return 0
}

const starterConfig = `{
  "teams": {
    "backend": {
      "owns": ["src/api/**", "src/services/**", "migrations/**"],
      "agents": ["claude"]
    },
    "frontend": {
      "owns": ["src/components/**", "src/pages/**", "styles/**"],
      "shared": ["src/types/**", "openapi.yaml"],
      "agents": ["codex"]
    }
  }
}
`

// runInit writes a starter declaration and never overwrites anything. Wiring the
// hooks into each agent's settings means merging into files the user owns, so
// this prints what to add rather than editing them behind their back.
func runInit() int {
	if _, err := os.Stat(config.FileName); err == nil {
		fmt.Printf("%s already exists, leaving it alone.\n", config.FileName)
	} else if err := os.WriteFile(config.FileName, []byte(starterConfig), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", config.FileName, err)
		return 1
	} else {
		fmt.Printf("wrote %s - edit it so the territories match this repo\n",
			config.FileName)
	}

	exe, err := os.Executable()
	if err != nil {
		exe = "agentcoord"
	}
	fmt.Printf(`
Next, wire the hooks. Add to .claude/settings.json:

  {"hooks":{
    "SessionStart":[{"hooks":[{"type":"command","command":"%s hook SessionStart claude"}]}],
    "PreToolUse":[{"matcher":"Write|Edit","hooks":[{"type":"command","command":"%s hook PreToolUse claude"}]}]
  }}

and to .codex/hooks.json:

  {"hooks":{
    "SessionStart":[{"hooks":[{"type":"command","command":"%s hook SessionStart codex"}]}],
    "PreToolUse":[{"hooks":[{"type":"command","command":"%s hook PreToolUse codex"}]}]
  }}

Codex only loads project hooks from a trusted project. Trust this repo in
~/.codex/config.toml, or the hooks are silently skipped.
`, exe, exe, exe, exe)
	return 0
}
