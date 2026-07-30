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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentcoord/internal/config"
	"agentcoord/internal/dashboard"
	"agentcoord/internal/hookio"
	"agentcoord/internal/install"
	"agentcoord/internal/lease"
	"agentcoord/internal/session"
	"agentcoord/internal/shellscan"
	"agentcoord/internal/territory"
)

const usage = `agentcoord - keep coding agents out of each other's way

  agentcoord hook <event> <agent>   run as an agent hook (reads payload on stdin)
  agentcoord status                 show territory and who holds what
  agentcoord dashboard [--addr A]   serve a live view on 127.0.0.1 (default :7717)
  agentcoord init [--dry-run]       wire the hooks into each agent's settings
  agentcoord init --uninstall       take them back out

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
	case "dashboard":
		os.Exit(runDashboard(args[1:]))
	case "init":
		os.Exit(runInit(args[1:]))
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
		note(cfg, ev, team, agent)
		emitContext(cfg.Summary(team) + presence(cfg, ev.SessionID))
		return exitOK
	case "PreToolUse":
		note(cfg, ev, team, agent)
		return checkWrite(cfg, team, agent, ev)
	case "PostToolUse":
		note(cfg, ev, team, agent)
		recordTouches(cfg, ev)
		renew(cfg, team, agent, ev)
		return exitOK
	case "SessionEnd", "Stop":
		release(cfg, ev.SessionID)
		endSession(cfg, ev.SessionID)
		return exitOK
	default:
		note(cfg, ev, team, agent)
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

// note records that this session is alive and what it is.
//
// Every event does this, not just SessionStart, because SessionStart is not
// guaranteed to have fired: hooks can be installed mid-session, and a resumed
// session may never send one. Errors are dropped, because failing to record
// presence is not worth telling anyone about and certainly not worth blocking.
func note(cfg *config.Config, ev *hookio.Event, team string, agent hookio.Agent) {
	if ev.SessionID == "" {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	store.Seen(ev.SessionID, string(agent), team, ev.Model, ev.CWD)
}

// recordTouches remembers the files a session just wrote, so the dashboard can
// show what it is working on rather than only that it exists.
func recordTouches(cfg *config.Config, ev *hookio.Event) {
	if ev.SessionID == "" || len(ev.Files) == 0 {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	paths := make([]string, 0, len(ev.Files))
	for _, f := range ev.Files {
		paths = append(paths, rel(cfg.Root, f))
	}
	store.Touched(ev.SessionID, ev.ToolName, paths)
}

func endSession(cfg *config.Config, sessionID string) {
	if sessionID == "" {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	store.End(sessionID)
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

// runDashboard serves the live view.
//
// It binds to loopback only and has no flag to change that. This page shows a
// person's development activity, and there is no version of exposing it on a
// network interface that is worth the convenience.
func runDashboard(args []string) int {
	addr := "127.0.0.1:7717"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr", "-addr":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--addr needs a value like 127.0.0.1:7717")
				return exitUsage
			}
			i++
			addr = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", args[i])
			return exitUsage
		}
	}
	if host, _, err := net.SplitHostPort(addr); err != nil {
		fmt.Fprintf(os.Stderr, "cannot parse --addr %q: %v\n", addr, err)
		return exitUsage
	} else if !isLoopback(host) {
		fmt.Fprintf(os.Stderr,
			"refusing to listen on %q: this page shows your development activity "+
				"and is served on loopback only\n", host)
		return exitUsage
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	// Fail here rather than serving a page that explains it has no config.
	if _, err := config.Load(cwd); err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Printf("No %s in this repo or any parent. Run: agentcoord init\n",
				config.FileName)
			return 1
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// State is re-read per request, so the page shows the repo as it is now and
	// the server does not have to be restarted when the config changes.
	load := func() (dashboard.View, error) {
		cfg, err := config.Load(cwd)
		if err != nil {
			return dashboard.View{}, err
		}
		sessions, err := readSessions(cfg)
		if err != nil {
			return dashboard.View{}, err
		}
		leases, err := readLeases(cfg)
		if err != nil {
			return dashboard.View{}, err
		}
		return dashboard.Build(cfg, sessions, leases, time.Now()), nil
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           dashboard.Handler(load),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("agentcoord dashboard on http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

func readSessions(cfg *config.Config) ([]*session.Session, error) {
	store, err := session.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	return store.Active()
}

func readLeases(cfg *config.Config) ([]*lease.Lease, error) {
	store, err := lease.Open(cfg.Root)
	if err != nil {
		// Leases are unavailable on platforms without an advisory lock. The rest
		// of the page is still worth showing.
		return nil, nil
	}
	return store.Active()
}

func isLoopback(host string) bool {
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const starterConfig = `{
  "teams": {
    "backend": {
      "owns": ["src/api/**", "src/services/**", "migrations/**"],
      "shared": ["src/types/**", "openapi.yaml"],
      "agents": ["claude"]
    },
    "frontend": {
      "owns": ["src/components/**", "src/pages/**", "styles/**"],
      "agents": ["codex"]
    }
  }
}
`

// runInit wires the hooks into each agent's settings, merging rather than
// replacing, and reports exactly which files it changed. Nothing is silent:
// these are files the user owns.
func runInit(args []string) int {
	var dryRun, uninstall bool
	for _, a := range args {
		switch a {
		case "--dry-run", "-dry-run", "-n":
			dryRun = true
		case "--uninstall", "-uninstall":
			uninstall = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return exitUsage
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}

	if uninstall {
		return uninstallHooks(cwd, dryRun)
	}

	if _, err := os.Stat(config.FileName); err == nil {
		fmt.Printf("%s exists, leaving it alone\n", config.FileName)
	} else if dryRun {
		fmt.Printf("would write %s\n", config.FileName)
	} else if err := os.WriteFile(config.FileName, []byte(starterConfig), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", config.FileName, err)
		return 1
	} else {
		fmt.Printf("wrote %s, edit it so the territories match this repo\n", config.FileName)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine my own path: %v\n", err)
		return 1
	}

	failed := false
	for _, target := range install.Plan(cwd) {
		change, err := install.Apply(target, exe, dryRun)
		if err != nil {
			// One agent's settings being unreadable must not stop the other from
			// being wired up.
			fmt.Fprintf(os.Stderr, "%s: %v\n", target.Agent, err)
			failed = true
			continue
		}
		switch {
		case !change.Changed():
			fmt.Printf("%s: already wired up\n", target.Agent)
		case dryRun:
			fmt.Printf("%s: would add %s to %s\n",
				target.Agent, strings.Join(change.Added, ", "), rel(cwd, change.File))
		default:
			fmt.Printf("%s: added %s to %s\n",
				target.Agent, strings.Join(change.Added, ", "), rel(cwd, change.File))
			if change.Backup != "" {
				fmt.Printf("%s: kept a copy of your settings at %s\n",
					target.Agent, rel(cwd, change.Backup))
			}
		}
	}

	if !dryRun {
		if added, err := install.Gitignore(cwd, ".agentcoord/"); err != nil {
			fmt.Fprintf(os.Stderr, "gitignore: %v\n", err)
		} else if added {
			fmt.Println("added .agentcoord/ to .gitignore, it is local runtime state")
		}
	}

	fmt.Printf(`
One step is left and it cannot be done for you. Codex loads project hooks only
from a trusted project, and skips them silently otherwise. Add this to
~/.codex/config.toml:

  [projects.%q]
  trust_level = "trusted"

Then: agentcoord status, and agentcoord dashboard to watch.
`, cwd)
	if failed {
		return 1
	}
	return 0
}

func uninstallHooks(root string, dryRun bool) int {
	failed := false
	for _, target := range install.Plan(root) {
		change, err := install.Remove(target, dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", target.Agent, err)
			failed = true
			continue
		}
		switch {
		case !change.Changed():
			fmt.Printf("%s: nothing of ours to remove\n", target.Agent)
		case dryRun:
			fmt.Printf("%s: would remove %s from %s\n",
				target.Agent, strings.Join(change.Removed, ", "), rel(root, change.File))
		default:
			fmt.Printf("%s: removed %s from %s\n",
				target.Agent, strings.Join(change.Removed, ", "), rel(root, change.File))
		}
	}
	fmt.Printf("\n%s and .agentcoord/ are left in place. Delete them if you are done.\n",
		config.FileName)
	if failed {
		return 1
	}
	return 0
}
