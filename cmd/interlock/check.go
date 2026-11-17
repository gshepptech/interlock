package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/territory"
)

// runCheck answers one question for an agent that cannot be intercepted: may I
// edit this file right now.
//
// An agent that is told before it writes does better than one refused after,
// and on Codex releases that do not honour hook trust nothing else can stop
// its writes. Agents follow the repository's instructions, and those
// instructions point here. The contract is
// the exit code, because that is what survives being read by a model: 0 free,
// 2 held, and never anything else for a question we could not answer.
func runCheck(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: interlock check <path>...")
		return exitUsage
	}

	cwd, err := os.Getwd()
	if err != nil {
		return allow("cannot determine working directory: %v", err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return allow("could not load config: %v", err)
	}
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return allow("could not read claims: %v", err)
	}
	held, err := store.Active()
	if err != nil {
		return allow("could not read claims: %v", err)
	}

	// An agent asking on its own behalf must not be told it is blocked by
	// itself. Two things can identify the caller.
	//
	// A session id is exact, when the caller knows it. An agent name is the
	// fallback and matters when hooks are also running: the hook claims the
	// file a moment before the model runs this check, so without it the agent
	// reads its own claim and refuses its own work. That happened in a live
	// Codex session, which reported the file "held by another codex session"
	// and declined. Where hooks are active they enforce contention between
	// sessions anyway, so ignoring the caller's own agent here costs nothing.
	own := os.Getenv("INTERLOCK_SESSION")
	ownAgent := os.Getenv("INTERLOCK_AGENT")
	team := os.Getenv(config.EnvTeam)

	blocked := false
	for _, arg := range args {
		target := arg
		if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
		relPath := rel(cfg.Root, filepath.Clean(target))

		var holder *lease.Lease
		for _, l := range held {
			if l.Path != relPath {
				continue
			}
			if l.SessionID == own || (ownAgent != "" && l.Agent == ownAgent) {
				continue
			}
			holder = l
			break
		}
		if holder != nil {
			blocked = true
			fmt.Printf("HELD  %s\n", holder.Describe(time.Now()))
			continue
		}

		// A declared territory is a standing rule rather than a live claim, and
		// it answers the same question, so it belongs in the same answer.
		if cfg.Declared && team != "" {
			decision, pattern, checkErr := cfg.Map.Check(team, target)
			if checkErr == nil && decision == territory.Denied {
				blocked = true
				fmt.Printf("HELD  %s belongs to the %q team (matched %q), and you are on %q\n",
					relPath, cfg.Map.Owner(target), pattern, team)
				continue
			}
		}
		fmt.Printf("FREE  %s\n", relPath)
	}

	if blocked {
		fmt.Fprintln(os.Stderr,
			"This is a scheduling conflict, not a permission one. Work on "+
				"something else and come back to it. Do not use another tool or "+
				"command to write it anyway.")
		return exitBlock
	}
	return exitOK
}
