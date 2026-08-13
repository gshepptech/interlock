package runcontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runner"
)

type harness struct {
	service Service
	root    string
	audit   []journal.Entry
}

// newHarness builds a service whose supervisor is a program that starts and
// then says nothing. That is enough for everything this package owns: the
// decisions, the refusals and the audit record. Whether a real agent runs is
// the supervisor's business and is tested where the supervisor lives.
func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	patient := filepath.Join(t.TempDir(), "slow-interlock")
	if err := os.WriteFile(patient, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{root: root}
	h.service = Service{
		Root: root, Executable: patient, Readiness: 100 * time.Millisecond,
		AppendAudit: func(entry journal.Entry) error {
			h.audit = append(h.audit, entry)
			return nil
		},
	}
	return h
}

func (h *harness) start(t *testing.T) managedrun.Record {
	t.Helper()
	result, err := h.service.Launch(context.Background(), ActorCLI, Start{
		Options: runner.Options{Runner: runner.Codex, Name: "api"},
		Prompt:  "Implement the invitation API.",
	})
	if err != nil || result.Run == nil {
		t.Fatalf("Launch = %#v err=%v", result, err)
	}
	return *result.Run
}

func (h *harness) lastAudit(t *testing.T) journal.Entry {
	t.Helper()
	if len(h.audit) == 0 {
		t.Fatal("no audit record was written")
	}
	return h.audit[len(h.audit)-1]
}

func TestLaunchCreatesARunAndRecordsIt(t *testing.T) {
	h := newHarness(t)
	record := h.start(t)

	if !managedrun.ValidID(record.ID()) {
		t.Fatalf("run = %#v", record)
	}
	entry := h.lastAudit(t)
	if entry.Action != journal.RunStart || entry.Target != record.ID() ||
		entry.Actor != ActorCLI || entry.Result != journal.Completed {
		t.Fatalf("audit = %#v", entry)
	}
	// The prompt is the one field that routinely describes private work. It must
	// not travel into the audit trail.
	if entry.Detail == "Implement the invitation API." ||
		entry.Path == "Implement the invitation API." {
		t.Fatal("the prompt reached the journal")
	}
}

func TestLaunchRefusesWhatCannotRunAndLeavesNothingBehind(t *testing.T) {
	cases := []struct {
		name  string
		input Start
	}{
		{"unknown runner", Start{Options: runner.Options{Runner: "gemini"}, Prompt: "go"}},
		{"no prompt", Start{Options: runner.Options{Runner: runner.Codex}}},
		{"interactive", Start{
			Options: runner.Options{Runner: runner.Codex, Interactive: true}, Prompt: "go"}},
		{"working directory outside the repository", Start{
			Options: runner.Options{Runner: runner.Codex, WorkingDir: "../elsewhere"}, Prompt: "go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			result, err := h.service.Launch(context.Background(), ActorCLI, tc.input)
			if control.KindOf(err) != control.Invalid {
				t.Fatalf("Launch = %v (%s)", err, control.KindOf(err))
			}
			if result.Outcome != journal.Rejected {
				t.Fatalf("outcome = %q", result.Outcome)
			}
			// A refusal is still an action someone took, so it is still audited.
			if entry := h.lastAudit(t); entry.Action != journal.RunStart ||
				entry.Result != journal.Rejected {
				t.Fatalf("audit = %#v", entry)
			}
			runs, err := h.service.List(context.Background())
			if err != nil || len(runs) != 0 {
				t.Fatalf("List = %#v err=%v", runs, err)
			}
		})
	}
}

func TestEveryLifecycleActionNeedsTheRunIdBack(t *testing.T) {
	h := newHarness(t)
	record := h.start(t)
	ctx := context.Background()

	for name, call := range map[string]func(string) (Result, error){
		"pause": func(c string) (Result, error) { return h.service.Pause(ctx, ActorCLI, record.ID(), c) },
		"stop":  func(c string) (Result, error) { return h.service.Stop(ctx, ActorCLI, record.ID(), c) },
		"force": func(c string) (Result, error) {
			return h.service.ForceStop(ctx, ActorCLI, record.ID(), c)
		},
		"retry": func(c string) (Result, error) { return h.service.Retry(ctx, ActorCLI, record.ID(), c) },
	} {
		t.Run(name, func(t *testing.T) {
			result, err := call("something else")
			if control.KindOf(err) != control.Invalid ||
				result.Outcome != journal.RejectedConfirmation {
				t.Fatalf("%s without the run id = %#v err=%v", name, result, err)
			}
		})
	}
}

// A run whose supervisor never appeared cannot be paused, and the honest answer
// says so rather than reporting success.
func TestLifecycleOnARunWithNoSupervisor(t *testing.T) {
	h := newHarness(t)
	record := h.start(t)

	store := managedrun.Open(h.root)
	if _, _, err := store.Transition(record.ID(), record.Spec.Generation, Starting, nil); err != nil {
		t.Fatal(err)
	}

	result, err := h.service.Pause(context.Background(), ActorCLI, record.ID(), record.ID())
	if control.KindOf(err) != control.Conflict || result.Outcome != journal.Rejected {
		t.Fatalf("Pause = %#v err=%v", result, err)
	}
	if result.Run == nil || result.Run.Status.State != managedrun.Lost {
		t.Fatalf("run = %#v; reconciliation should have found nothing listening", result.Run)
	}
	if entry := h.lastAudit(t); entry.Action != journal.RunPause || entry.Result != journal.Rejected {
		t.Fatalf("audit = %#v", entry)
	}
}

// Starting is spelled out here because the test drives the record directly,
// standing in for a supervisor that claimed the run and then died.
const Starting = managedrun.Starting

func TestRetryCreatesANewRunAndKeepsTheOldOne(t *testing.T) {
	h := newHarness(t)
	record := h.start(t)
	store := managedrun.Open(h.root)

	// A live run is not retried; it is stopped first.
	result, err := h.service.Retry(context.Background(), ActorCLI, record.ID(), record.ID())
	if control.KindOf(err) != control.Conflict || result.Outcome != journal.Rejected {
		t.Fatalf("retry of a live run = %#v err=%v", result, err)
	}

	if _, _, err := store.Transition(record.ID(), record.Spec.Generation, managedrun.Failed,
		func(status *managedrun.Status) { status.Reason = managedrun.ReasonExitNonZero }); err != nil {
		t.Fatal(err)
	}

	result, err = h.service.Retry(context.Background(), ActorCLI, record.ID(), record.ID())
	if err != nil || !result.Successful() {
		t.Fatalf("Retry = %#v err=%v", result, err)
	}
	fresh := *result.Run
	if fresh.ID() == record.ID() {
		t.Fatal("retry reused the record; the failure is part of the history")
	}
	if fresh.Spec.RetryOf != record.ID() {
		t.Fatalf("retry_of = %q, want %q", fresh.Spec.RetryOf, record.ID())
	}
	if !reflect.DeepEqual(fresh.Spec.Options, record.Spec.Options) {
		t.Fatalf("options = %#v, want %#v", fresh.Spec.Options, record.Spec.Options)
	}
	original, err := store.Prompt(record.ID())
	if err != nil {
		t.Fatal(err)
	}
	copied, err := store.Prompt(fresh.ID())
	if err != nil || string(copied) != string(original) {
		t.Fatalf("prompt = %q err=%v", copied, err)
	}
	if _, err := store.Load(record.ID()); err != nil {
		t.Fatalf("the original run was lost: %v", err)
	}
}

func TestShowAndLogsRefuseAnUnknownRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.service.Show(ctx, "run-"+"0123456789abcdef01234567"); control.KindOf(err) != control.NotFound {
		t.Fatalf("Show = %v (%s)", err, control.KindOf(err))
	}
	if _, _, err := h.service.Logs(ctx, "nonsense", managedrun.Stdout,
		managedrun.Cursor{}, 1024); control.KindOf(err) != control.NotFound {
		t.Fatalf("Logs = %v (%s)", err, control.KindOf(err))
	}
	if len(h.audit) != 0 {
		t.Fatal("reading is not an action and must not be audited")
	}
}

// The action happened. An audit store that cannot record it is worth a warning,
// not a lie about what was done.
func TestAnUnavailableAuditIsAWarningNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.service.AppendAudit = func(journal.Entry) error { return errors.New("disk is full") }

	result, err := h.service.Launch(context.Background(), ActorCLI, Start{
		Options: runner.Options{Runner: runner.Codex}, Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if result.Outcome != journal.CompletedAuditWarning || result.Warning == "" {
		t.Fatalf("result = %#v", result)
	}
	if !result.Successful() {
		t.Fatal("a missing audit row does not undo the run")
	}
	if result.Run == nil {
		t.Fatal("the run still exists and must be reported")
	}
}
