package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/session"
)

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
