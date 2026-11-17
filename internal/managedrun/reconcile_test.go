package managedrun

import (
	"os/exec"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
)

// abandon puts a record into the state a crash leaves behind: it says the run
// is live, and nothing is listening for it.
func abandon(t *testing.T, store *Store, state State) Record {
	t.Helper()
	record := mustCreate(t, store)
	for _, step := range []State{Starting, Running} {
		if _, _, err := store.Transition(record.ID(), record.Spec.Generation, step, nil); err != nil {
			t.Fatal(err)
		}
		if step == state {
			break
		}
	}
	reloaded, err := store.Load(record.ID())
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

func TestReconcileMarksAnAbandonedRunLost(t *testing.T) {
	store, _ := testStore(t)
	record := abandon(t, store, Running)

	records, err := store.Reconcile()
	if err != nil || len(records) != 1 {
		t.Fatalf("Reconcile = %#v err=%v", records, err)
	}
	if records[0].Status.State != Lost || records[0].Status.Reason != ReasonSupervisorGone {
		t.Fatalf("status = %#v", records[0].Status)
	}
	if records[0].Status.EndedAt.IsZero() {
		t.Error("a lost run has finished, whatever happened to the process")
	}

	// And it stays lost: a terminal state is not revisited on the next listing.
	again, err := store.Reconcile()
	if err != nil || again[0].Status.Sequence != records[0].Status.Sequence {
		t.Fatalf("a second Reconcile changed the record: %#v err=%v", again[0].Status, err)
	}
	_ = record
}

// The claim this whole design rests on: a record's stored pid is never used to
// decide anything. Here it names a process that is very much alive and has
// nothing to do with Interlock.
func TestReconcileNeverTrustsOrTouchesAStoredPID(t *testing.T) {
	store, _ := testStore(t)
	record := abandon(t, store, Running)

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	})
	if _, _, err := store.Update(record.ID(), record.Spec.Generation, func(status *Status) {
		status.ChildPID = bystander.Process.Pid
		status.ProcessGroup = bystander.Process.Pid
	}); err != nil {
		t.Fatal(err)
	}

	records, err := store.Reconcile()
	if err != nil || records[0].Status.State != Lost {
		t.Fatalf("state = %s err=%v; a live pid is not a live run", records[0].Status.State, err)
	}
	if !alive(bystander.Process.Pid) {
		t.Fatal("reconciliation signalled a process it found through a stored pid")
	}
}

// A supervisor that holds the run's lock is alive by the only definition that
// cannot lie, even if its socket did not answer this instant.
func TestReconcileLeavesARunWhoseSupervisorHoldsItsLock(t *testing.T) {
	store, _ := testStore(t)
	record := abandon(t, store, Running)

	unlock, acquired, err := lockfile.TryAcquire(store.SupervisorLockPath(record.ID()))
	if err != nil || !acquired {
		t.Fatalf("TryAcquire = %v acquired=%v", err, acquired)
	}
	defer unlock()

	records, err := store.Reconcile()
	if err != nil || records[0].Status.State != Running {
		t.Fatalf("state = %s err=%v", records[0].Status.State, err)
	}
}

// A run created a moment ago has not had time to be claimed. Declaring it lost
// would turn a slow start into a failure.
func TestReconcileGivesANewRunTimeToStart(t *testing.T) {
	store, now := testStore(t)
	record := mustCreate(t, store)

	records, err := store.Reconcile()
	if err != nil || records[0].Status.State != Queued {
		t.Fatalf("state = %s err=%v", records[0].Status.State, err)
	}

	*now = now.Add(startupGrace + time.Second)
	records, err = store.Reconcile()
	if err != nil || records[0].Status.State != Lost {
		t.Fatalf("state after the grace = %s err=%v", records[0].Status.State, err)
	}
	_ = record
}

func TestReconcileLeavesALiveRunAlone(t *testing.T) {
	f := newFixture(t, "linger")
	f.awaitState(t, Running)

	records, err := f.store.Reconcile()
	if err != nil || len(records) != 1 {
		t.Fatalf("Reconcile = %#v err=%v", records, err)
	}
	if records[0].Status.State != Running {
		t.Fatalf("state = %s; a live supervisor answered for this run", records[0].Status.State)
	}

	single, err := f.store.ReconcileOne(f.record.ID())
	if err != nil || single.Status.State != Running {
		t.Fatalf("ReconcileOne = %s err=%v", single.Status.State, err)
	}

	f.call(t, Stop)
	f.awaitTerminal(t)
}
