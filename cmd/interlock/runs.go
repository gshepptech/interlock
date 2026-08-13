package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runcontrol"
	"github.com/gshepptech/interlock/internal/runner"
)

const runsUsage = `usage:
  interlock run <codex|claude> "prompt" [--name N] [--model M] [--team T]
      [--cwd DIR] [--env NAME ...] [--no-follow]
  interlock runs list
  interlock runs show ID
  interlock runs logs ID [--stderr] [--follow] [--bytes N]
  interlock runs pause ID --confirm ID
  interlock runs resume ID
  interlock runs stop ID [--force] --confirm ID
  interlock runs retry ID --confirm ID
`

// runRun starts an agent and, by default, shows what it does.
//
// Following is the default because the alternative teaches the wrong thing: an
// agent you started and cannot see is one you will forget you started. Ctrl-C
// detaches this terminal and deliberately does not signal the agent, so leaving
// the room is not the same as stopping the work.
func runRun(args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, runsUsage)
		return exitUsage
	}
	options := runner.Options{Runner: runner.Name(args[0])}
	prompt := args[1]
	follow := true

	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		value := func() (string, bool) {
			if i+1 >= len(rest) {
				return "", false
			}
			i++
			return rest[i], true
		}
		var ok bool
		switch rest[i] {
		case "--name":
			options.Name, ok = value()
		case "--model":
			options.Model, ok = value()
		case "--team":
			options.Team, ok = value()
		case "--cwd":
			options.WorkingDir, ok = value()
		case "--env":
			var name string
			if name, ok = value(); ok {
				options.Environment = append(options.Environment, name)
			}
		case "--interactive":
			// Accepted so the refusal explains itself, rather than the flag
			// looking like a typo.
			options.Interactive, ok = true, true
		case "--no-follow":
			follow, ok = false, true
		default:
			ok = false
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", rest[i], runsUsage)
			return exitUsage
		}
	}

	service, err := newRunService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, runErr := service.Launch(context.Background(), runcontrol.ActorCLI,
		runcontrol.Start{Options: options, Prompt: prompt})
	printRunResult(result)
	if runErr != nil || !result.Successful() {
		return 1
	}
	if !follow || result.Run == nil {
		return 0
	}
	return followRun(service, *result.Run)
}

func runRuns(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, runsUsage)
		return exitUsage
	}
	service, err := newRunService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()

	switch args[0] {
	case "list":
		if len(args) != 1 {
			return runsUsageError()
		}
		records, err := service.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs list: %v\n", err)
			return 1
		}
		printRunList(os.Stdout, records, time.Now())
		return 0

	case "show":
		if len(args) != 2 {
			return runsUsageError()
		}
		record, err := service.Show(ctx, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs show: %v\n", err)
			return 1
		}
		printRunDetail(os.Stdout, record, time.Now())
		return 0

	case "logs":
		return runLogs(service, args[1:])

	case "pause", "resume", "stop", "retry":
		return runLifecycle(service, args)
	}
	return runsUsageError()
}

func runLifecycle(service runcontrol.Service, args []string) int {
	if len(args) < 2 {
		return runsUsageError()
	}
	action, id := args[0], args[1]
	confirmation := ""
	force := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--confirm":
			if i+1 >= len(args) {
				return runsUsageError()
			}
			i++
			confirmation = args[i]
		case "--force":
			force = true
		default:
			return runsUsageError()
		}
	}

	ctx := context.Background()
	var result runcontrol.Result
	var err error
	switch {
	case action == "pause":
		result, err = service.Pause(ctx, runcontrol.ActorCLI, id, confirmation)
	case action == "resume":
		result, err = service.Resume(ctx, runcontrol.ActorCLI, id)
	case action == "stop" && force:
		result, err = service.ForceStop(ctx, runcontrol.ActorCLI, id, confirmation)
	case action == "stop":
		result, err = service.Stop(ctx, runcontrol.ActorCLI, id, confirmation)
	case action == "retry":
		result, err = service.Retry(ctx, runcontrol.ActorCLI, id, confirmation)
	}
	printRunResult(result)
	if err != nil || !result.Successful() {
		return 1
	}
	return 0
}

func runLogs(service runcontrol.Service, args []string) int {
	if len(args) == 0 {
		return runsUsageError()
	}
	id := args[0]
	stream := managedrun.Stdout
	follow := false
	var bytes int64 = 16 * 1024
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--stderr":
			stream = managedrun.Stderr
		case "--follow":
			follow = true
		case "--bytes":
			if i+1 >= len(args) {
				return runsUsageError()
			}
			i++
			parsed, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil || parsed <= 0 {
				fmt.Fprintf(os.Stderr, "--bytes needs a positive number\n")
				return exitUsage
			}
			bytes = parsed
		default:
			return runsUsageError()
		}
	}

	ctx := context.Background()
	cursor := managedrun.Cursor{}
	chunk, cursor, err := service.Logs(ctx, id, stream, cursor, bytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs logs: %v\n", err)
		return 1
	}
	if chunk.Truncated {
		fmt.Fprintln(os.Stderr, "(earlier output not shown)")
	}
	os.Stdout.WriteString(chunk.Text())
	if !follow {
		return 0
	}
	record, err := service.Show(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs logs: %v\n", err)
		return 1
	}
	return tail(service, record, stream, cursor)
}

// followRun shows a run's output until it finishes or the user leaves.
func followRun(service runcontrol.Service, record managedrun.Record) int {
	return tail(service, record, managedrun.Stdout, managedrun.Cursor{})
}

// tail polls for output. Polling rather than watching is deliberate: the log is
// a file that another process rotates underneath us, and a cursor that notices
// that is simpler to reason about than a descriptor that has to be told.
func tail(service runcontrol.Service, record managedrun.Record,
	stream managedrun.Stream, cursor managedrun.Cursor) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	id := record.ID()
	for {
		chunk, next, err := service.Logs(ctx, id, stream, cursor, 64*1024)
		if err == nil {
			cursor = next
			os.Stdout.WriteString(chunk.Text())
		}

		current, err := service.Show(ctx, id)
		if err == nil && current.Status.State.Terminal() {
			fmt.Fprintf(os.Stderr, "\n%s %s\n", id, current.Describe())
			if current.Status.State == managedrun.Completed {
				return 0
			}
			return 1
		}

		select {
		case <-ctx.Done():
			// Leaving the terminal is not stopping the agent, and saying so is
			// the difference between a tool people trust and one they do not.
			fmt.Fprintf(os.Stderr,
				"\ndetached from %s; it is still running. `interlock runs stop %s --confirm %s` to stop it\n",
				id, id, id)
			return 0
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func printRunResult(result runcontrol.Result) {
	if result.Message != "" {
		fmt.Println(result.Message)
	}
	if result.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", result.Warning)
	}
	// A command that created a run prints its identifier on a line of its own,
	// because everything else a person does with that run is addressed by it.
	if result.Run != nil &&
		(result.Action == runcontrol.ActionStart || result.Action == runcontrol.ActionRetry) {
		fmt.Printf("run: %s\n", result.Run.ID())
	}
}

func printRunList(out io.Writer, records []managedrun.Record, now time.Time) {
	if len(records) == 0 {
		fmt.Fprintln(out, "no managed runs")
		return
	}
	for _, record := range records {
		fmt.Fprintf(out, "%s  %-9s %-7s %-16s %s\n", record.ID(), record.Status.State,
			record.Runner(), truncate(record.Label(), 16),
			record.Duration(now).Round(time.Second))
	}
}

func printRunDetail(out io.Writer, record managedrun.Record, now time.Time) {
	fmt.Fprintf(out, "id: %s\nstate: %s\nrunner: %s\nname: %s\n",
		record.ID(), record.Describe(), record.Runner(), record.Label())
	if record.Spec.Options.Model != "" {
		fmt.Fprintf(out, "model: %s\n", record.Spec.Options.Model)
	}
	if record.Spec.Options.Team != "" {
		fmt.Fprintf(out, "team: %s\n", record.Spec.Options.Team)
	}
	fmt.Fprintf(out, "directory: %s\n", record.Spec.Options.Dir())
	fmt.Fprintf(out, "started by: %s\n", record.Spec.Creator)
	fmt.Fprintf(out, "running for: %s\n", record.Duration(now).Round(time.Second))
	if record.Spec.RetryOf != "" {
		fmt.Fprintf(out, "retry of: %s\n", record.Spec.RetryOf)
	}
	if record.Status.Summary != "" {
		fmt.Fprintf(out, "last: %s\n", record.Status.Summary)
	}
	if record.Status.ForceRequired {
		fmt.Fprintf(out, "force required: interlock runs stop %s --force --confirm %s\n",
			record.ID(), record.ID())
	}
	// The pids are printed as what they are: something to look at, never
	// something to act on. Interlock signals through the supervisor.
	if record.Status.ChildPID != 0 {
		fmt.Fprintf(out, "process (for reference only): %d\n", record.Status.ChildPID)
	}
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max-1] + "…"
}

func runsUsageError() int {
	fmt.Fprint(os.Stderr, runsUsage)
	return exitUsage
}

func newRunService() (runcontrol.Service, error) {
	root, err := currentRoot()
	if err != nil {
		return runcontrol.Service{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return runcontrol.Service{}, fmt.Errorf("cannot determine my own path: %w", err)
	}
	return runcontrol.Service{Root: root, Executable: executable}, nil
}

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
