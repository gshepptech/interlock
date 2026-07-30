// Command interlock lets coding agents from different vendors work one repo
// without overwriting each other.
//
// It is invoked as a hook by each agent. On SessionStart it tells the agent what
// it owns and who else is here. On PreToolUse it blocks a write that lands in
// another team's territory, or on a shared file another agent currently holds,
// and explains why, so the agent redirects instead of failing.
package main

import (
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/gitstate"
	"github.com/gshepptech/interlock/internal/hookio"
	"github.com/gshepptech/interlock/internal/install"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/shellscan"
	"github.com/gshepptech/interlock/internal/territory"
)

const usage = `interlock - keep coding agents out of each other's way

  interlock hook <event> <agent>   run as an agent hook (reads payload on stdin)
  interlock status                 show territory and who holds what
  interlock doctor                 check for the ways this silently fails
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
	case "status":
		os.Exit(runStatus())
	case "doctor":
		os.Exit(runDoctor())
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
		fmt.Fprint(os.Stderr, "usage: interlock hook <event> <agent>\n")
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
		verifyAfter(cfg, team, agent, ev)
		return exitOK
	case "SessionEnd", "Stop":
		release(cfg, ev.SessionID)
		endSession(cfg, ev.SessionID)
		clearRemedy(cfg, ev.SessionID)
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
	files := ev.Files
	if len(files) == 0 && ev.Command != "" {
		found := shellscan.Scan(ev.Command)
		switch found.Confidence {
		case shellscan.None:
			return exitOK
		case shellscan.Opaque:
			// Something writes and nothing on the command line says where.
			// Blocking every such command would stop `python3 build.py`, which
			// is normal work. Instead the tree is snapshotted now and compared
			// after the command, so a write that lands outside the territory is
			// caught and the agent is told to undo it.
			if !armVerify(cfg, ev.SessionID) {
				fmt.Fprintf(os.Stderr,
					"interlock: this command writes somewhere that cannot be read "+
						"from the command line (%s), and this tree cannot be watched "+
						"(no git), so territory is not enforced for it. Stay inside "+
						"%q.\n", found.Reason, team)
			}
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
		fmt.Fprintf(os.Stderr, "interlock: leases unavailable (%v), "+
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
		// With no declaration every path is Unowned, which is the ordinary case
		// and the one the claim below covers.
		if team == "" && decision == territory.Denied {
			decision = territory.Unowned
		}

		switch decision {
		case territory.Denied:
			// A path this session was told to revert has to stay writable by it,
			// or the tool demands a fix and then blocks the fix. Narrow on
			// purpose: this exact path, this session, for a short window.
			if allowedToRemedy(cfg, ev.SessionID, rel(cfg.Root, file)) {
				fmt.Fprintf(os.Stderr,
					"interlock: allowing this write to %s so you can undo the "+
						"change you were asked to revert.\n", rel(cfg.Root, file))
				continue
			}
			owner := cfg.Map.Owner(file)
			record(cfg, journal.Entry{
				Kind: journal.BlockedTerritory, Agent: string(agent), Team: team,
				SessionID: ev.SessionID, Path: rel(cfg.Root, file),
				Detail: fmt.Sprintf("belongs to %q, matched %q", owner, pattern),
			})
			fmt.Fprintf(os.Stderr,
				"Blocked by interlock: %s belongs to the %q team (matched %q), "+
					"and you are on %q. Another agent may be working there right now. "+
					"Work inside your own territory, or ask for this file to be shared.\n",
				rel(cfg.Root, file), owner, pattern, team)
			return exitBlock

		default:
			// Every other outcome, whether the file was declared shared or was
			// never mentioned at all, is settled by claiming it. This is what
			// makes the tool work with no configuration: writing a file means
			// you are in it, and a second agent is told rather than colliding.
			if store == nil {
				continue
			}
			hold, implicit := lease.ImplicitTTL, true
			if decision == territory.Shared {
				// A file two teams were told to share is worth holding longer;
				// contention on it is expected rather than incidental.
				hold, implicit = lease.DefaultTTL, false
			}

			relPath := rel(cfg.Root, file)
			granted, current, err := store.AcquireFor(relPath, holder, hold, implicit)
			if err != nil {
				return allow("could not claim %s: %v", relPath, err)
			}
			if granted {
				continue
			}

			record(cfg, journal.Entry{
				Kind: journal.BlockedLease, Agent: string(agent), Team: team,
				SessionID: ev.SessionID, Path: relPath,
				Detail: fmt.Sprintf("%s had it, %s ago", current.Agent,
					time.Since(current.Acquired).Round(time.Second)),
			})
			fmt.Fprintf(os.Stderr,
				"Blocked by interlock: %s This is a timing conflict, not a "+
					"permission one: work on something else and come back to it. "+
					"Do not try to write it another way.\n",
				current.Describe(time.Now()))
			return exitBlock
		}
	}
	return exitOK
}

// renew extends the leases this session holds on the files it just touched, so a
// file under active work never expires beneath its holder.
func renew(cfg *config.Config, team string, agent hookio.Agent, ev *hookio.Event) {
	if len(ev.Files) == 0 {
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
		if err != nil {
			continue
		}
		hold, implicit := lease.ImplicitTTL, true
		if decision == territory.Shared {
			hold, implicit = lease.DefaultTTL, false
		}
		// Errors are ignored on purpose: a failed renewal costs the claim its
		// time to live, and there is nothing useful to tell the agent.
		store.AcquireFor(rel(cfg.Root, file), holder, hold, implicit)
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

// presence describes the other agents working this repo right now.
//
// This is the half of the tool that needs no rules and no configuration at all.
// An agent that knows a peer is live in src/components and last touched Card.tsx
// a minute ago will usually stay out of the way on its own, without ever being
// refused anything. Blocking is the backstop; this is the mechanism.
//
// Nothing is said when the repo is quiet, because a line of boilerplate on every
// session start trains an agent to skip the whole block.
func presence(cfg *config.Config, ownSession string) string {
	sessions, leases := otherSessions(cfg, ownSession), heldBy(cfg, ownSession)
	if len(sessions) == 0 && len(leases) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nOther agents are working in this repository right now.")

	now := time.Now()
	for _, sess := range sessions {
		fmt.Fprintf(&b, "\n  - %s", describeSession(sess, now))
		if areas := workingAreas(sess); areas != "" {
			fmt.Fprintf(&b, ", working in %s", areas)
		}
		if recent := sess.Recent; len(recent) > 0 {
			fmt.Fprintf(&b, ". Last touched %s %s ago",
				recent[0].Path, now.Sub(recent[0].At).Round(time.Second))
		}
	}
	for _, l := range leases {
		fmt.Fprintf(&b, "\n  - %s is claimed, so writing it will be refused until "+
			"they move on", l.Path)
	}
	b.WriteString("\nStay out of what they are in the middle of. If you need a " +
		"file they hold, do something else first and come back to it.")
	return b.String()
}

func describeSession(s *session.Session, now time.Time) string {
	who := s.Agent
	if who == "" {
		who = "an agent"
	}
	if s.Team != "" {
		who += " on " + s.Team
	}
	return fmt.Sprintf("%s, active %s ago", who,
		now.Sub(s.LastSeen).Round(time.Second))
}

// workingAreas names the directories a session has been writing in, which is a
// better hint than a file list: it tells a peer which part of the repo to avoid
// rather than which exact bytes are locked.
func workingAreas(s *session.Session) string {
	seen := map[string]bool{}
	var dirs []string
	for _, t := range s.Recent {
		dir := filepath.Dir(t.Path)
		if dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
		if len(dirs) == 3 {
			break
		}
	}
	return strings.Join(dirs, ", ")
}

func otherSessions(cfg *config.Config, ownSession string) []*session.Session {
	store, err := session.Open(cfg.Root)
	if err != nil {
		return nil
	}
	active, err := store.Active()
	if err != nil {
		return nil
	}
	var out []*session.Session
	for _, s := range active {
		if s.ID != ownSession {
			out = append(out, s)
		}
	}
	return out
}

func heldBy(cfg *config.Config, ownSession string) []*lease.Lease {
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return nil
	}
	active, err := store.Active()
	if err != nil {
		return nil
	}
	var out []*lease.Lease
	for _, l := range active {
		// Implicit claims churn as agents move between files, and listing all of
		// them would bury the session summary above. The directories a peer is
		// working in are the useful signal; only a deliberately shared file is
		// worth naming as a hard obstacle.
		if l.SessionID != ownSession && !l.Implicit {
			out = append(out, l)
		}
	}
	return out
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

func runStatus() int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Printf("repo: %s\n\n", cfg.Root)
	if !cfg.Declared {
		fmt.Printf("automatic: no %s, so agents claim files as they write them\n\n",
			config.FileName)
	}
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
	// A repo with no declaration is the normal case, so the only thing worth
	// failing on here is a declaration that exists and cannot be read.
	if _, err := config.Load(cwd); err != nil {
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
		return dashboard.Build(cfg, sessions, leases, readEvents(cfg), time.Now()), nil
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           dashboard.Handler(load),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("interlock dashboard on http://%s\n", addr)
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

// readEvents is best-effort: an unreadable journal costs the page its activity
// feed and must not cost it the rest.
func readEvents(cfg *config.Config) []journal.Entry {
	store, err := journal.Open(cfg.Root)
	if err != nil {
		return nil
	}
	events, err := store.Recent(25)
	if err != nil {
		return nil
	}
	return events
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
	var dryRun, uninstall, withTerritory bool
	for _, a := range args {
		switch a {
		case "--dry-run", "-dry-run", "-n":
			dryRun = true
		case "--uninstall", "-uninstall":
			uninstall = true
		case "--with-territory", "-with-territory":
			withTerritory = true
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

	// No configuration is written. The tool works on observation alone, and a
	// starter file would suggest it has to be filled in before anything happens.
	if withTerritory {
		switch _, err := os.Stat(config.FileName); {
		case err == nil:
			fmt.Printf("%s exists, leaving it alone\n", config.FileName)
		case dryRun:
			fmt.Printf("would write %s\n", config.FileName)
		default:
			if err := os.WriteFile(config.FileName, []byte(starterConfig), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write %s: %v\n", config.FileName, err)
				return 1
			}
			fmt.Printf("wrote %s, edit it so the territories match this repo\n",
				config.FileName)
		}
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
		if added, err := install.Gitignore(cwd, ".interlock/"); err != nil {
			fmt.Fprintf(os.Stderr, "gitignore: %v\n", err)
		} else if added {
			fmt.Println("added .interlock/ to .gitignore, it is local runtime state")
		}
	}

	fmt.Printf(`
That is it. Nothing to configure: open Claude in one terminal and Codex in
another and they will see each other. Whoever writes a file first has it for a
minute or two; the other is told who is in there and works on something else.

One step cannot be done for you. Codex loads project hooks only from a trusted
project and skips them silently otherwise, so add this to ~/.codex/config.toml:

  [projects.%q]
  trust_level = "trusted"

Then run: interlock doctor, and interlock dashboard to watch them work.

If you ever need a hard rule the automatic behaviour cannot work out, such as
one team owning a directory outright, run: interlock init --with-territory
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
	fmt.Printf("\n%s and .interlock/ are left in place. Delete them if you are done.\n",
		config.FileName)
	if failed {
		return 1
	}
	return 0
}

// pendingDir holds the snapshot taken before a command whose targets could not
// be read off the command line.
func pendingDir(root string) string {
	return filepath.Join(root, ".interlock", "pending")
}

func pendingFile(root, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(pendingDir(root), hex.EncodeToString(sum[:16])+".json")
}

// armVerify snapshots the working tree so the next PostToolUse can tell what the
// command actually wrote. It reports whether the tree can be watched at all.
func armVerify(cfg *config.Config, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	snap, err := gitstate.Take(cfg.Root)
	if err != nil {
		return false
	}
	enc, err := json.Marshal(snap)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(pendingDir(cfg.Root), 0o755); err != nil {
		return false
	}
	// Written directly rather than atomically: a torn snapshot fails to parse
	// and the verification is skipped, which is the same outcome as not having
	// armed it. Nothing downstream trusts a partial read.
	return os.WriteFile(pendingFile(cfg.Root, sessionID), enc, 0o644) == nil
}

// verifyAfter compares the tree against the snapshot armed before an opaque
// command, and reports anything that landed outside the agent's territory.
//
// This runs after the write. It cannot prevent it, so the value is entirely in
// telling the agent precisely what to undo, which agents are good at.
func verifyAfter(cfg *config.Config, team string, agent hookio.Agent, ev *hookio.Event) {
	if team == "" || ev.SessionID == "" {
		return
	}
	file := pendingFile(cfg.Root, ev.SessionID)
	raw, err := os.ReadFile(file)
	if err != nil {
		return // nothing was armed, which is the common case
	}
	// One snapshot answers one command. Removing it first means a failure below
	// cannot leave it to be compared against some unrelated later command.
	os.Remove(file)

	var before gitstate.Snapshot
	if err := json.Unmarshal(raw, &before); err != nil {
		return
	}
	after, err := gitstate.Take(cfg.Root)
	if err != nil {
		return
	}

	var escaped []string
	for _, path := range gitstate.Changed(&before, after) {
		// Our own runtime state changes constantly and is nobody's territory.
		if strings.HasPrefix(path, ".interlock/") {
			continue
		}
		decision, pattern, err := cfg.Map.Check(team, filepath.Join(cfg.Root, path))
		if err != nil || decision != territory.Denied {
			continue
		}
		escaped = append(escaped, path)
		record(cfg, journal.Entry{
			Kind: journal.EscapedGuard, Agent: string(agent), Team: team,
			SessionID: ev.SessionID, Path: path,
			Detail: fmt.Sprintf("written by a command whose target could not be "+
				"read in advance; belongs to %q, matched %q",
				cfg.Map.Owner(filepath.Join(cfg.Root, path)), pattern),
		})
	}
	if len(escaped) == 0 {
		return
	}

	// Granted before the agent is told, so the revert it is about to attempt is
	// not blocked by the very rule that flagged it.
	grantRemedy(cfg, ev.SessionID, escaped)

	// PostToolUse can put text in front of the model on both agents, which is
	// the only thing that turns this into a correction rather than a log line.
	emitPostContext(fmt.Sprintf(
		"interlock: that command wrote %s, which the %s team does not own. "+
			"You have been granted permission to revert exactly those paths for "+
			"the next %s, so delete or restore them now and then stay inside your "+
			"own territory.",
		strings.Join(escaped, ", "), team, remedy.TTL))
	fmt.Fprintf(os.Stderr,
		"interlock: %s wrote outside %q: %s\n",
		agent, team, strings.Join(escaped, ", "))
}

// emitPostContext injects text at PostToolUse. The event name has to match the
// event that is firing or the agent ignores the payload.
func emitPostContext(text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": text,
		},
	}
	if enc, err := json.Marshal(out); err == nil {
		fmt.Println(string(enc))
	}
}

// record writes a journal entry. Failing to record that something happened is
// never a reason to change what happened, so the error is dropped.
func record(cfg *config.Config, e journal.Entry) {
	store, err := journal.Open(cfg.Root)
	if err != nil {
		return
	}
	store.Append(e)
}

// runDoctor diagnoses the ways this tool can be installed and silently not work.
//
// That is the failure mode worth building a command for: Codex skips project
// hooks in an untrusted repo without saying anything, so a half-installed setup
// looks exactly like a working one until two agents collide.
func runDoctor() int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}

	problems := 0
	report := func(ok bool, subject, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Printf("%s  %-28s %s\n", mark, subject, detail)
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		report(false, "config", err.Error())
		return 1
	}
	if cfg.Declared {
		report(true, "territory", fmt.Sprintf("%d teams declared in %s",
			len(cfg.Map.Teams), config.FileName))
		// A team nobody joins enforces nothing.
		claimed := 0
		for _, t := range cfg.Map.Teams {
			if len(cfg.AgentsFor(t.Name)) > 0 {
				claimed++
			}
		}
		report(claimed > 0, "team assignment",
			fmt.Sprintf("%d of %d teams have a default agent", claimed, len(cfg.Map.Teams)))
	} else {
		// Not a problem. This is how the tool is meant to be used.
		report(true, "territory", "automatic, agents claim files as they write them")
	}
	report(true, "repo", cfg.Root)

	for _, target := range install.Plan(cwd) {
		wired, err := install.Installed(target)
		switch {
		case err != nil:
			report(false, target.Agent+" hooks", err.Error())
		case len(wired) == 0:
			report(false, target.Agent+" hooks",
				"not wired up. Run: interlock init")
		case len(wired) < len(target.Events):
			report(false, target.Agent+" hooks",
				fmt.Sprintf("only %s wired, expected %d events",
					strings.Join(wired, ", "), len(target.Events)))
		default:
			report(true, target.Agent+" hooks",
				fmt.Sprintf("%d events in %s", len(wired), rel(cwd, target.File)))
		}
	}

	// The silent one.
	trusted, detail := codexTrusts(cwd)
	report(trusted, "codex repo trust", detail)

	if gitstate.Available(cwd) {
		report(true, "after-the-fact checks", "git available, opaque writes are verified")
	} else {
		report(false, "after-the-fact checks",
			"not a git repo, so writes from opaque shell commands cannot be checked")
	}

	if _, err := lease.Open(cfg.Root); err != nil {
		report(false, "leases", err.Error())
	} else {
		report(true, "leases", "available")
	}

	fmt.Println()
	if problems == 0 {
		fmt.Println("Everything is wired up.")
		return 0
	}
	fmt.Printf("%d problem(s). Anything marked FAIL means part of this is not enforcing.\n",
		problems)
	return 1
}

// codexTrusts reports whether Codex will load this repo's project hooks. Codex
// skips them silently for an untrusted project, so this is checked by reading
// its config rather than by waiting to find out.
func codexTrusts(root string) (bool, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "cannot find your home directory to check ~/.codex/config.toml"
	}
	file := filepath.Join(home, ".codex", "config.toml")
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, "no ~/.codex/config.toml, so Codex is either not installed or not configured"
	}
	if err != nil {
		return false, err.Error()
	}
	// Deliberately a substring check and not a TOML parse. Being wrong here
	// costs a misleading line in a diagnostic; pulling in a TOML parser to be
	// certain costs a dependency in a binary that has none.
	if strings.Contains(string(raw), `"`+root+`"`) {
		return true, "this repo is trusted, Codex will load its hooks"
	}
	return false, "this repo is NOT trusted, so Codex silently skips its hooks. " +
		"Add [projects.\"" + root + "\"] trust_level = \"trusted\" to ~/.codex/config.toml"
}

// allowedToRemedy reports whether this session was told to undo a change at this
// path and is still inside the window to do it.
func allowedToRemedy(cfg *config.Config, sessionID, path string) bool {
	if sessionID == "" {
		return false
	}
	store, err := remedy.Open(cfg.Root)
	if err != nil {
		return false
	}
	return store.Allowed(sessionID, path)
}

// grantRemedy permits a session to write back over paths it wrote by mistake.
func grantRemedy(cfg *config.Config, sessionID string, paths []string) {
	if sessionID == "" || len(paths) == 0 {
		return
	}
	store, err := remedy.Open(cfg.Root)
	if err != nil {
		return
	}
	store.Grant(sessionID, paths)
}

func clearRemedy(cfg *config.Config, sessionID string) {
	if sessionID == "" {
		return
	}
	store, err := remedy.Open(cfg.Root)
	if err != nil {
		return
	}
	store.Clear(sessionID)
}
