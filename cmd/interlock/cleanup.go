package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gshepptech/interlock/internal/cleanup"
	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/control"
)

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
	if result.LeasesPruned > 0 {
		fmt.Printf("swept %d expired lease(s)\n", result.LeasesPruned)
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
