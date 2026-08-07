// Command interlock lets coding agents from different vendors work one repo
// without overwriting each other.
//
// It is invoked as a hook by each agent. On SessionStart it tells the agent what
// it owns and who else is here. On PreToolUse it blocks a write that lands in
// another team's territory, or on a shared file another agent currently holds,
// and explains why, so the agent redirects instead of failing.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/cleanup"
	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/controlhttp"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/diagnostic"
	"github.com/gshepptech/interlock/internal/gitstate"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/hookio"
	"github.com/gshepptech/interlock/internal/install"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/shellscan"
	"github.com/gshepptech/interlock/internal/territory"
	"github.com/gshepptech/interlock/internal/verification"
)

const usage = `interlock - keep coding agents out of each other's way

  interlock hook <event> <agent>   run as an agent hook (reads payload on stdin)
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
	case "status":
		os.Exit(runStatus())
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

	now := time.Now()
	sessionStore, err := session.Open(cfg.Root)
	if err != nil {
		fmt.Printf("\nsessions unavailable: %v\n", err)
	} else if observed, err := sessionStore.All(); err != nil {
		fmt.Printf("\ncannot read sessions: %v\n", err)
	} else if len(observed) == 0 {
		fmt.Println("\nno observed sessions")
	} else {
		fmt.Println("\nobserved sessions:")
		for _, sess := range observed {
			expires := sess.LastSeen.Add(session.IdleAfter)
			expiry := "expires in " + formatDuration(expires.Sub(now))
			if !expires.After(now) {
				expiry = "expired " + formatDuration(now.Sub(expires)) + " ago"
			}
			fmt.Printf("  %s  %-7s %-6s last seen %s ago, %s\n",
				sess.ID, fallback(sess.Agent, "unknown"),
				sess.ObservationAt(now), formatDuration(now.Sub(sess.LastSeen)), expiry)
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
	fmt.Println("held right now:")
	for _, l := range active {
		fmt.Printf("  %s Expires in %s.\n", l.Describe(now),
			formatDuration(l.Remaining(now)))
	}
	return 0
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	duration = duration.Round(time.Second)
	if duration < time.Second {
		return "now"
	}
	return duration.String()
}

func runCleanup(args []string) int {
	dryRun := false
	var sessionID, confirmation string
	var dryRunSet, sessionSet, confirmationSet bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-dry-run", "-n":
			if dryRunSet {
				fmt.Fprintln(os.Stderr, "--dry-run may be specified only once")
				return exitUsage
			}
			dryRunSet = true
			dryRun = true
		case "--session":
			if sessionSet || i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--session needs an id")
				return exitUsage
			}
			sessionSet = true
			i++
			sessionID = args[i]
		case "--confirm":
			if confirmationSet || i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--confirm needs the exact session id")
				return exitUsage
			}
			confirmationSet = true
			i++
			confirmation = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", args[i])
			return exitUsage
		}
	}
	if dryRun && sessionID != "" {
		fmt.Fprintln(os.Stderr, "--dry-run cannot be combined with --session")
		return exitUsage
	}
	if sessionSet != confirmationSet {
		fmt.Fprintln(os.Stderr, "--session and --confirm must be provided together")
		return exitUsage
	}
	if sessionSet && (sessionID == "" || confirmation == "") {
		fmt.Fprintln(os.Stderr, "--session and --confirm values must not be empty")
		return exitUsage
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sessionSet {
		service, err := newControlService(cfg.Root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, err := service.ClearStaleSession(
			context.Background(), control.ActorCLI, sessionID, confirmation)
		printControlResult(os.Stdout, os.Stderr, result)
		if err != nil || !result.Successful() {
			return 1
		}
		return 0
	}
	if !dryRun {
		service, err := newControlService(cfg.Root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, runErr := service.ClearAllStale(context.Background(), control.ActorCLI)
		printControlResult(os.Stdout, os.Stderr, result)
		if runErr != nil || !result.Successful() {
			return 1
		}
		return 0
	}
	result, err := (cleanup.Service{Root: cfg.Root}).Run(true)
	for _, item := range result.Sessions {
		printCleanupResult(os.Stdout, item, dryRun)
	}
	if len(result.Sessions) == 0 {
		fmt.Println("no stale sessions")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
		return 1
	}
	return 0
}

func printCleanupResult(w io.Writer, item cleanup.SessionResult, dryRun bool) {
	agent := fallback(item.Agent, "unknown agent")
	if dryRun || item.Outcome == cleanup.Pending {
		fmt.Fprintf(w, "would remove stale session %s (%s), last seen %s\n",
			item.ID, agent, item.LastSeen.Format(time.RFC3339))
		return
	}
	switch item.Outcome {
	case cleanup.Removed:
		fmt.Fprintf(w, "removed stale session %s (%s): %d leases, remedy=%t, pending=%t\n",
			item.ID, agent, item.LeasesReleased, item.RemedyCleared, item.PendingRemoved)
	case cleanup.Failed:
		fmt.Fprintf(w, "failed to clean stale session %s (%s): %s\n",
			item.ID, agent, fallback(item.Error, "unknown error"))
	default:
		fmt.Fprintf(w, "kept session %s (%s): it became active during cleanup\n",
			item.ID, agent)
	}
}

func runLease(args []string) int {
	if len(args) < 2 || args[0] != "release" {
		fmt.Fprintln(os.Stderr, "usage: interlock lease release ID --confirm ID")
		return exitUsage
	}
	sessionID := args[1]
	confirmation := ""
	for i := 2; i < len(args); i++ {
		if args[i] != "--confirm" || i+1 >= len(args) {
			fmt.Fprintln(os.Stderr, "usage: interlock lease release ID --confirm ID")
			return exitUsage
		}
		i++
		confirmation = args[i]
	}
	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	service, err := newControlService(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, runErr := service.ReleaseSessionLeases(
		context.Background(), control.ActorCLI, sessionID, confirmation)
	printControlResult(os.Stdout, os.Stderr, result)
	if runErr != nil || !result.Successful() {
		return 1
	}
	return 0
}

func runHandoff(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: interlock handoff request|list|show|resolve")
		return exitUsage
	}
	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store := handoffrequest.Open(root)
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return handoffUsage()
		}
		items, err := store.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "handoff list: %v\n", err)
			return 1
		}
		if len(items) == 0 {
			fmt.Println("no handoff requests")
			return 0
		}
		for _, item := range items {
			status := "open"
			if !item.Open() {
				status = "resolved"
			}
			fmt.Printf("%s  %-8s %s:%s  from %s\n", item.Request.ID, status,
				item.Request.TargetKind, item.Request.Target, item.Request.SourceSession)
		}
		return 0
	case "show":
		if len(args) != 2 {
			return handoffUsage()
		}
		item, err := store.Show(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "handoff show: %v\n", err)
			return 1
		}
		status := "open"
		if !item.Open() {
			status = "resolved"
		}
		fmt.Printf("id: %s\nstatus: %s\nfrom: %s\nto: %s:%s\nnote: %s\n",
			item.Request.ID, status, item.Request.SourceSession,
			item.Request.TargetKind, item.Request.Target, item.Request.Note)
		return 0
	case "request":
		values, ok := parseNamedArgs(args[1:], map[string]bool{
			"--from": true, "--target-kind": true, "--to": true, "--note": true,
		})
		if !ok {
			return handoffUsage()
		}
		service, err := newControlService(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, runErr := service.RequestHandoff(context.Background(), control.ActorCLI,
			handoffrequest.Create{
				SourceSession: values["--from"],
				TargetKind:    handoffrequest.TargetKind(values["--target-kind"]),
				Target:        values["--to"], Note: values["--note"],
			})
		printControlResult(os.Stdout, os.Stderr, result)
		if result.Handoff != nil {
			fmt.Printf("request: %s\n", result.Handoff.Request.ID)
		}
		if runErr != nil || !result.Successful() {
			return 1
		}
		return 0
	case "resolve":
		if len(args) < 2 {
			return handoffUsage()
		}
		values, ok := parseNamedArgs(args[2:], map[string]bool{"--confirm": true})
		if !ok {
			return handoffUsage()
		}
		service, err := newControlService(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, runErr := service.ResolveHandoff(
			context.Background(), control.ActorCLI, args[1], values["--confirm"])
		printControlResult(os.Stdout, os.Stderr, result)
		if runErr != nil || !result.Successful() {
			return 1
		}
		return 0
	default:
		return handoffUsage()
	}
}

func parseNamedArgs(args []string, allowed map[string]bool) (map[string]string, bool) {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !allowed[args[i]] || i+1 >= len(args) || values[args[i]] != "" {
			return nil, false
		}
		name := args[i]
		i++
		values[name] = args[i]
	}
	for name := range allowed {
		if values[name] == "" && name != "--note" {
			return nil, false
		}
	}
	return values, true
}

func handoffUsage() int {
	fmt.Fprintln(os.Stderr, "usage: interlock handoff request --from ID --target-kind agent|team --to NAME --note TEXT")
	fmt.Fprintln(os.Stderr, "       interlock handoff list | show ID | resolve ID --confirm ID")
	return exitUsage
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
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fmt.Fprintf(os.Stderr,
			"refusing to listen on %q: use an IP-literal loopback address; this page shows your development activity "+
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
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// The repository boundary is fixed for the server lifetime. Runtime state is
	// re-read per request, while config changes take effect after a restart.
	load := dashboardLoad(cfg)
	controller, err := newControlService(cfg.Root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	controller.Config = cfg
	handler, err := controlhttp.New(controller, addr, load)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	fmt.Printf("interlock dashboard on http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

func dashboardLoad(cfg *config.Config) controlhttp.Load {
	return func() (dashboard.View, []handoffrequest.Item, error) {
		sessions, err := readSessions(cfg)
		if err != nil {
			return dashboard.View{}, nil, err
		}
		leases, err := readLeases(cfg)
		if err != nil {
			return dashboard.View{}, nil, err
		}
		requests, err := handoffrequest.Open(cfg.Root).List()
		if err != nil {
			return dashboard.View{}, nil, err
		}
		return dashboard.Build(cfg, sessions, leases, readEvents(cfg), time.Now()), requests, nil
	}
}

func readSessions(cfg *config.Config) ([]*session.Session, error) {
	store, err := session.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	return store.All()
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

	codexFile, err := codexConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot locate Codex config: %v\n", err)
		return 1
	}
	repairs, repairErr := (diagnostic.Service{}).Repair(diagnostic.Options{
		Root: cwd, Executable: exe, CodexFile: codexFile,
	}, dryRun)
	for _, repair := range repairs {
		switch {
		case repair.Changed:
			fmt.Printf("%s: %s\n", repair.ID, repair.Detail)
		default:
			fmt.Printf("%s: already current\n", repair.ID)
		}
	}
	if repairErr != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", repairErr)
	}

	fmt.Printf(`
That is it. Nothing to configure: open Claude in one terminal and Codex in
another and they will see each other. Whoever writes a file first has it for a
minute or two; the other is told who is in there and works on something else.

Run interlock doctor to confirm it is enforcing, and interlock dashboard to
watch them work.

If you ever need a hard rule the automatic behaviour cannot work out, such as
one team owning a directory outright, run: interlock init --with-territory
`)
	if repairErr != nil {
		return 1
	}
	return 0
}

func uninstallHooks(root string, dryRun bool) int {
	failed := false
	// Read the trust entries before the hooks file is removed, because they are
	// derived from it and cannot be identified afterwards.
	var codexTrust []install.CodexTrustEntry
	for _, target := range install.Plan(root) {
		if target.Agent != "codex" {
			continue
		}
		entries, err := install.CodexTrustEntries(target)
		if err == nil {
			codexTrust = entries
		}
	}

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
		for _, old := range change.Superseded {
			fmt.Printf("%s: also removed %s from %s, left by an older version\n",
				target.Agent, strings.Join(old.Removed, ", "), rel(root, old.File))
		}
	}
	// Withdraw the Codex hook trust this repository was granted. Left behind, it
	// accumulates in the user's global config for every repository Interlock was
	// ever installed into, including ones that no longer exist.
	if len(codexTrust) > 0 {
		if codexFile, err := codexConfigPath(); err != nil {
			fmt.Fprintf(os.Stderr, "codex: cannot locate config to withdraw trust: %v\n", err)
			failed = true
		} else if change, err := install.UntrustCodexConfig(codexFile, codexTrust, dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "codex: %v\n", err)
			failed = true
		} else if len(change.HooksRemoved) > 0 {
			verb := "withdrew"
			if dryRun {
				verb = "would withdraw"
			}
			fmt.Printf("codex: %s hook trust for %d hook(s) from %s\n",
				verb, len(change.HooksRemoved), change.File)
		}
	}

	fmt.Printf("\n%s and .interlock/ are left in place. Delete them if you are done.\n",
		config.FileName)
	if failed {
		return 1
	}
	return 0
}

// armVerify snapshots the working tree so the next PostToolUse can tell what the
// command actually wrote. It reports whether the tree can be watched at all.
func armVerify(cfg *config.Config, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	return verification.Open(cfg.Root).Arm(sessionID) == nil
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
	before, err := verification.Open(cfg.Root).Consume(ev.SessionID)
	if err != nil {
		return // nothing was armed, which is the common case
	}
	after, err := gitstate.Take(cfg.Root)
	if err != nil {
		return
	}

	var escaped []string
	for _, path := range gitstate.Changed(before, after) {
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
func runDoctor(args []string) int {
	var fix, smoke bool
	for _, arg := range args {
		switch arg {
		case "--fix", "-fix":
			fix = true
		case "--smoke", "-smoke":
			smoke = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", arg)
			return exitUsage
		}
	}

	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	service, err := newControlService(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var result control.Result
	switch {
	case fix:
		result, err = service.RepairInstallation(
			context.Background(), control.ActorCLI, "repair", smoke)
		for _, repair := range result.Repairs {
			mark := "ok  "
			if repair.Changed {
				mark = "FIX "
			}
			fmt.Printf("%s  %-28s %s\n", mark, repair.ID, repair.Detail)
		}
		fmt.Println()
	case smoke:
		result, err = service.VerifyInstallation(context.Background(), control.ActorCLI)
	default:
		result, err = service.CheckInstallation(context.Background())
	}
	if result.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", result.Warning)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
	}
	if result.Diagnostic == nil {
		return 1
	}
	report := *result.Diagnostic
	problems := 0
	for _, check := range report.Checks {
		mark := "ok  "
		switch check.Severity {
		case diagnostic.Warn:
			mark = "WARN"
		case diagnostic.Fail:
			mark = "FAIL"
			problems++
		}
		fmt.Printf("%s  %-28s %s\n", mark, check.Subject, check.Detail)
	}

	fmt.Println()
	if problems == 0 && err == nil && result.Successful() {
		fmt.Println("Everything is wired up.")
		return 0
	}
	fmt.Printf("%d problem(s). Anything marked FAIL means part of this is not enforcing.\n",
		problems)
	return 1
}

func codexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
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
