package managedrun

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartRefusesARunThatIsNotWaiting(t *testing.T) {
	store, _ := testStore(t)
	record := mustCreate(t, store)
	if _, _, err := store.Transition(record.ID(), record.Spec.Generation, Starting, nil); err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{Store: store, Executable: os.Args[0]}
	if _, err := launcher.Start(record.ID()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Start of a claimed run = %v, want conflict", err)
	}
	if _, err := launcher.Start("run-" + "0123456789abcdef01234567"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Start of an unknown run = %v, want not found", err)
	}
}

// A supervisor that cannot be spawned is a durable failure, not a run left
// waiting for something that will never arrive.
func TestStartRecordsASupervisorThatCannotBeSpawned(t *testing.T) {
	store, _ := testStore(t)
	record := mustCreate(t, store)
	launcher := Launcher{
		Store:      store,
		Executable: filepath.Join(t.TempDir(), "no-such-interlock"),
		Readiness:  200 * time.Millisecond,
	}
	got, err := launcher.Start(record.ID())
	if err == nil {
		t.Fatal("Start with a missing executable succeeded")
	}
	if got.Status.State != Failed || got.Status.Reason != ReasonLaunchFailed {
		t.Fatalf("status = %#v", got.Status)
	}
	if got.Status.Summary == "" {
		t.Error("a launch failure has to say what went wrong")
	}
}

// Waiting is bounded, and running out of patience is not the same as failing.
// The run is still queued and the record is still the place to look.
func TestStartReportsWhenTheSupervisorIsSlowWithoutFailingTheRun(t *testing.T) {
	store, _ := testStore(t)
	record := mustCreate(t, store)
	patient := filepath.Join(t.TempDir(), "slow-interlock")
	if err := os.WriteFile(patient, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{Store: store, Executable: patient, Readiness: 150 * time.Millisecond}
	got, err := launcher.Start(record.ID())
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Start = %v, want not ready", err)
	}
	if got.Status.State != Queued {
		t.Fatalf("state = %s; a slow start is not a failed one", got.Status.State)
	}
}
