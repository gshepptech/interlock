package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runner"
)

// runSupervise hosts one run's supervisor.
//
// It is deliberately not in the usage text. Nobody types it: the launcher
// spawns this program again with this verb, which is how the supervisor ends up
// in its own session, outliving the CLI call or the dashboard request that
// asked for the run. Keeping it a verb of the same binary means there is no
// second thing to install and no second version to keep in step.
func runSupervise(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, "usage: interlock supervise <run-id>\n")
		return exitUsage
	}
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

	// A signal to the supervisor is a request to stop the agent politely, not a
	// reason to abandon it. Without this the agent would be orphaned by the very
	// thing that was supposed to own it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	supervisor := &managedrun.Supervisor{
		Store: managedrun.Open(cfg.Root),
		RunID: args[0],
		Environment: runner.Environment{
			Root:   cfg.Root,
			Lookup: os.LookupEnv,
		},
	}
	err = supervisor.Serve(ctx)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, managedrun.ErrAlreadySupervised):
		// Two supervisors for one run is not a failure; it is one supervisor.
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "supervise %s: %v\n", args[0], err)
		return 1
	}
}
