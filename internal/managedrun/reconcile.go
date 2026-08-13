package managedrun

import (
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
)

// Reconciliation answers one question: of the runs that claim to be live, which
// ones actually are?
//
// It has to exist because the records outlive the processes. A machine is put
// to sleep, a supervisor is killed, a laptop reboots mid-run, and what is left
// on disk still says "running". The tempting fix is to check whether the stored
// pid is alive, and that is precisely the mistake this package refuses to make:
// pids are reused, so the process answering to that number may be something the
// user is in the middle of using.
//
// So liveness is established two ways, both of which are about Interlock's own
// processes. A supervisor answers on its socket and agrees about the run's
// generation, or it holds the run's lock, which the kernel releases when the
// process dies for any reason. If neither is true, nothing can be said about
// the agent any more, and the run is marked lost rather than guessed at.

const (
	// probeTimeout is short because a probe talks to a local process that is
	// either there or not. A supervisor that cannot answer in this long is not
	// one a user should be waiting on.
	probeTimeout = 300 * time.Millisecond
	// startupGrace is how long a queued run is given before it is treated as
	// abandoned. It covers the gap between creating a record and a supervisor
	// claiming it.
	startupGrace = 15 * time.Second
	// settleDelay is the second chance a supervisor gets. A socket is replaced
	// atomically at startup, so a probe can arrive in the one moment there is
	// nothing to connect to.
	settleDelay = 50 * time.Millisecond
	// reconcileBudget bounds the work one call does. Whatever is left is
	// reconciled by the next call rather than making a listing slow.
	reconcileBudget = time.Second
)

// Reconcile refreshes every live-looking run and returns the current list,
// newest first. It never signals a process and never adopts one.
func (c *Store) Reconcile() ([]Record, error) {
	records, err := c.List()
	if err != nil {
		return nil, err
	}
	deadline := c.now().Add(reconcileBudget)
	for i, record := range records {
		if record.Status.State.Terminal() {
			continue
		}
		if c.now().After(deadline) {
			break
		}
		records[i] = c.reconcile(record)
	}
	return records, nil
}

// ReconcileOne refreshes a single run. Lifecycle commands call it first, so a
// user is never told a run was stopped when in truth nothing was listening.
func (c *Store) ReconcileOne(id string) (Record, error) {
	record, err := c.Load(id)
	if err != nil {
		return Record{}, err
	}
	if record.Status.State.Terminal() {
		return record, nil
	}
	return c.reconcile(record), nil
}

func (c *Store) reconcile(record Record) Record {
	if live, updated := c.probe(record); live {
		return updated
	}
	// No answer. Before concluding anything, ask the question that does not
	// depend on timing: is a supervisor holding this run at all?
	if c.supervised(record.Spec.ID) {
		return record
	}
	// A run that was only just created has not had time to be claimed, and
	// declaring it lost would turn a slow start into a failure.
	if record.Status.State == Queued && c.now().Sub(record.Spec.CreatedAt) < startupGrace {
		return record
	}

	// One more chance, because a supervisor replaces its socket atomically as it
	// starts and a probe can land in that gap.
	time.Sleep(settleDelay)
	if live, updated := c.probe(record); live {
		return updated
	}
	if c.supervised(record.Spec.ID) {
		return record
	}

	status, changed, err := c.Transition(record.Spec.ID, record.Spec.Generation, Lost,
		func(status *Status) {
			status.Reason = ReasonSupervisorGone
			status.Summary = "the supervisor is gone; the agent cannot be controlled from here"
			status.ForceRequired = false
		})
	if err != nil || !changed {
		return record
	}
	return Record{Spec: record.Spec, Status: status}
}

// probe asks the supervisor for the truth. A supervisor that answers owns the
// run's state, so what it says is used directly rather than re-read from disk.
func (c *Store) probe(record Record) (bool, Record) {
	response, err := c.call(record.Spec.ID, Probe, probeTimeout)
	if err != nil || !response.Outcome.Successful() {
		return false, record
	}
	if response.Status.ID != record.Spec.ID || response.Status.Generation != record.Spec.Generation {
		return false, record
	}
	return true, Record{Spec: record.Spec, Status: response.Status}
}

// supervised reports whether a process still holds the run's lock. The kernel
// drops that lock when the holder dies, for any reason, which makes this the
// one liveness question with an honest answer.
func (c *Store) supervised(id string) bool {
	held, err := lockfile.Held(c.SupervisorLockPath(id))
	// An error here means the question could not be asked. Assuming a
	// supervisor is present is the safe direction: the cost is a stale record
	// for one more cycle, where the other way round is declaring a live run
	// lost.
	return err != nil || held
}
