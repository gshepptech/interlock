package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/gitstate"
	"github.com/gshepptech/interlock/internal/hookio"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/shellscan"
	"github.com/gshepptech/interlock/internal/territory"
	"github.com/gshepptech/interlock/internal/verification"
)

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
			noteRunBlocked(cfg.Root, fmt.Sprintf("blocked: %s belongs to the %s team",
				rel(cfg.Root, file), owner))
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
			noteRunBlocked(cfg.Root, fmt.Sprintf("blocked: %s is held by %s",
				relPath, current.Agent))
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
