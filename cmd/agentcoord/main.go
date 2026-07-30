// Command agentcoord lets coding agents from different vendors work one repo
// without overwriting each other.
//
// It is invoked as a hook by each agent. On SessionStart it tells the agent what
// it owns and who else is here. On PreToolUse it blocks a write that lands in
// another team's territory, or on a shared file another agent currently holds,
// and explains why, so the agent redirects instead of failing.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentcoord/internal/config"
	"agentcoord/internal/hookio"
	"agentcoord/internal/lease"
	"agentcoord/internal/shellscan"
	"agentcoord/internal/territory"
)

const usage = `agentcoord - keep coding agents out of each other's way

  agentcoord hook <event> <agent>   run as an agent hook (reads payload on stdin)
  agentcoord status                 show territory and who holds what
  agentcoord init                   write starter config and print hook wiring

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
// someone's work stopped. A territory violation and a held lease are the only
// things that block. Anything else that goes wrong, a payload we cannot read, a
// config we cannot parse, a repo with no config at all, allows the call and says
// so on stderr. The cost of allowing is a collision the user could already have
// had. The cost of blocking on our own bug is a session that cannot write to its
// own repo.
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
	if ev.Name == "" {
		ev.Name = event
	}

	cwd := ev.CWD
	if cwd == "" {
		// Some events carry no cwd. Falling back to ours is right, because the
		// agent spawned us in its own working directory.
		if cwd, err = os.Getwd(); err != nil {
			return allow("no cwd in payload and cannot determine ours: %v", err)
		}
	}

	cfg, err := config.Load(cwd)
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
		emitContext(cfg.Summary(team) + presence(cfg, ev.SessionID))
		return exitOK
	case "PreToolUse":
		return checkWrite(cfg, team, agent, ev)
	case "PostToolUse":
		renew(cfg, team, agent, ev)
		return exitOK
	case "SessionEnd", "Stop":
		release(cfg, ev.SessionID)
		return exitOK
	default:
		return exitOK
	}
}

// checkWrite decides whether a tool call may proceed.
//
// Every file is checked, not just the first: one Codex patch can touch several,
// and stopping at the first allowed one would wave the rest through.
func checkWrite(cfg *config.Config, team string, agent hookio.Agent, ev *hookio.Event) int {
	if team == "" {
		// This session declared no team, so there is nothing to enforce against.
		return exitOK
	}

	files := ev.Files
	if len(files) == 0 && ev.Command != "" {
		found := shellscan.Scan(ev.Command)
		switch found.Confidence {
		case shellscan.None:
			return exitOK
		case shellscan.Opaque:
			// Something writes and nothing on the command line says where.
			// Blocking every such command would stop `python3 build.py`, which
			// is normal work, so this reports and allows. It is the one hole
			// left, and saying so beats implying coverage.
			fmt.Fprintf(os.Stderr,
				"agentcoord: this command writes somewhere it cannot be read from "+
					"the command line (%s). Territory is not enforced for it. "+
					"Stay inside %q.\n", found.Reason, team)
			return exitOK
		case shellscan.Likely:
			files = absAll(ev.CWD, found.Paths)
		}
	}

	if len(files) == 0 {
		return exitOK
	}

	store, err := lease.Open(cfg.Root)
	if err != nil {
		// Without leases, territory still works. Enforce what we can.
		fmt.Fprintf(os.Stderr, "agentcoord: leases unavailable (%v), "+
			"territory still enforced\n", err)
		store = nil
	}

	holder := lease.Holder{
		Team:      team,
		Agent:     string(agent),
		SessionID: ev.SessionID,
		PID:       os.Getpid(),
	}

	for _, file := range files {
		decision, pattern, err := cfg.Map.Check(team, file)
		if err != nil {
			// Either the path is outside the repo, which is not ours to police,
			// or the config claims one path for two teams. Neither is grounds
			// for stopping the user; say it and move on.
			return allow("%v", err)
		}

		switch decision {
		case territory.Denied:
			fmt.Fprintf(os.Stderr,
				"Blocked by agentcoord: %s belongs to the %q team (matched %q), "+
					"and you are on %q. Another agent may be working there right now. "+
					"Work inside your own territory, or ask for this file to be shared.\n",
				rel(cfg.Root, file), cfg.Map.Owner(file), pattern, team)
			return exitBlock

		case territory.Shared:
			if store == nil {
				continue
			}
			relPath := rel(cfg.Root, file)
			granted, current, err := store.Acquire(relPath, holder)
			if err != nil {
				return allow("could not take a lease on %s: %v", relPath, err)
			}
			if !granted {
				fmt.Fprintf(os.Stderr,
					"Blocked by agentcoord: %s. It is a shared file, so only one "+
						"agent may hold it at a time. Work on something else and come "+
						"back to it, or ask the holder to finish.\n",
					current.Describe(time.Now()))
				return exitBlock
			}
		}
	}
	return exitOK
}

// renew extends the leases this session holds on the files it just touched, so a
// file under active work never expires beneath its holder.
func renew(cfg *config.Config, team string, agent hookio.Agent, ev *hookio.Event) {
	if team == "" || len(ev.Files) == 0 {
		return
	}
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return
	}
	holder := lease.Holder{
		Team: team, Agent: string(agent),
		SessionID: ev.SessionID, PID: os.Getpid(),
	}
	for _, file := range ev.Files {
		decision, _, err := cfg.Map.Check(team, file)
		if err != nil || decision != territory.Shared {
			continue
		}
		// Errors are ignored on purpose: a failed renewal costs the lease its
		// time to live, and there is nothing useful to tell the agent.
		store.Acquire(rel(cfg.Root, file), holder)
	}
}

// release frees this session's shared files on a tidy exit, rather than making
// the next agent wait out the time to live.
func release(cfg *config.Config, sessionID string) {
	if sessionID == "" {
		return
	}
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return
	}
	store.Release(sessionID)
}

// presence describes the other agents currently holding shared files, for
// injection at session start. Nothing is said when the repo is quiet, because a
// line of boilerplate on every session start trains an agent to ignore it.
func presence(cfg *config.Config, ownSession string) string {
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return ""
	}
	active, err := store.Active()
	if err != nil || len(active) == 0 {
		return ""
	}

	var b strings.Builder
	now := time.Now()
	for _, l := range active {
		if l.SessionID == ownSession {
			continue
		}
		fmt.Fprintf(&b, "\n  - %s", l.Describe(now))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nShared files currently held by other agents:" + b.String()
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

	store, err := lease.Open(cfg.Root)
	if err != nil {
		fmt.Printf("\nleases unavailable: %v\n", err)
		return 0
	}
	active, err := store.Active()
	if err != nil {
		fmt.Printf("\ncannot read leases: %v\n", err)
		return 0
	}
	fmt.Println()
	if len(active) == 0 {
		fmt.Println("no shared files held right now")
		return 0
	}
	now := time.Now()
	fmt.Println("held right now:")
	for _, l := range active {
		fmt.Printf("  %s\n", l.Describe(now))
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
Add .agentcoord/ to .gitignore: leases are local runtime state.

Then wire the hooks. In .claude/settings.json:

  {"hooks":{
    "SessionStart":[{"hooks":[{"type":"command","command":"%[1]s hook SessionStart claude"}]}],
    "PreToolUse":[{"matcher":"Write|Edit|Bash","hooks":[{"type":"command","command":"%[1]s hook PreToolUse claude"}]}],
    "PostToolUse":[{"matcher":"Write|Edit","hooks":[{"type":"command","command":"%[1]s hook PostToolUse claude"}]}],
    "SessionEnd":[{"hooks":[{"type":"command","command":"%[1]s hook SessionEnd claude"}]}]
  }}

In .codex/hooks.json:

  {"hooks":{
    "SessionStart":[{"hooks":[{"type":"command","command":"%[1]s hook SessionStart codex"}]}],
    "PreToolUse":[{"hooks":[{"type":"command","command":"%[1]s hook PreToolUse codex"}]}],
    "PostToolUse":[{"hooks":[{"type":"command","command":"%[1]s hook PostToolUse codex"}]}]
  }}

Codex only loads project hooks from a trusted project. Trust this repo in
~/.codex/config.toml, or the hooks are silently skipped. Codex exposes no
session-end event, so its leases are freed by the time to live rather than
on exit.
`, exe)
	return 0
}
