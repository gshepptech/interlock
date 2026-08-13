package managedrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultReadiness is how long a caller waits for a supervisor to report that
// the agent is running. It is a bound on waiting, not on starting: a run that
// takes longer is still starting, and the record says so.
const DefaultReadiness = 5 * time.Second

// ErrNotReady reports that the supervisor had not published a result in time.
// The run is not failed and nothing has been lost; the caller simply stopped
// waiting, and the record is the place to look.
var ErrNotReady = errors.New("the supervisor has not reported yet")

// Launcher spawns the detached supervisor for a queued run.
//
// The two steps are deliberately separate. Creating the record is durable and
// happens first, so a caller that dies between the two leaves a queued run that
// reconciliation can account for, rather than an agent nobody knows about. The
// spawn itself is fire and forget: the supervisor is put in its own session and
// this process does not wait for it, which is what lets the CLI exit while the
// agent keeps working.
type Launcher struct {
	Store *Store
	// Executable is Interlock's own binary. The supervisor is this same program
	// run again with a different verb, so there is no second thing to install
	// and no second version to keep in step.
	Executable string
	Readiness  time.Duration
}

// Start spawns a supervisor and waits for it to publish a result.
func (l Launcher) Start(id string) (Record, error) {
	if l.Store == nil || !ValidID(id) {
		return Record{}, ErrNotFound
	}
	if l.Executable == "" {
		return Record{}, fmt.Errorf("%w: the Interlock executable is not known", ErrInvalid)
	}
	record, err := l.Store.Load(id)
	if err != nil {
		return Record{}, err
	}
	if record.Status.State != Queued {
		return record, fmt.Errorf("%w: run %s is %s, not queued", ErrConflict, id, record.Status.State)
	}

	if err := l.spawn(id); err != nil {
		l.Store.Transition(id, record.Spec.Generation, Failed, func(status *Status) {
			status.Reason, status.Summary = ReasonLaunchFailed, err.Error()
		})
		reloaded, loadErr := l.Store.Load(id)
		if loadErr != nil {
			return record, err
		}
		return reloaded, err
	}
	return l.await(id)
}

func (l Launcher) spawn(id string) error {
	// The supervisor's own diagnostics have nowhere else to go: it has no
	// terminal, and its stderr would otherwise be discarded exactly when
	// something has gone wrong enough to write to it.
	bootstrap, err := os.OpenFile(l.Store.BootstrapPath(id),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open bootstrap log: %w", err)
	}
	defer bootstrap.Close()

	cmd := exec.Command(l.Executable, "supervise", id)
	cmd.Dir = l.Store.root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, bootstrap, bootstrap
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	// Releasing rather than waiting is the point: this process must be free to
	// exit while the supervisor carries on.
	return cmd.Process.Release()
}

// await polls the record until the supervisor publishes a result. Polling the
// durable state rather than talking to the supervisor means the answer is the
// same one every other reader will get, and a supervisor that dies during
// startup cannot leave a caller waiting on a promise.
func (l Launcher) await(id string) (Record, error) {
	deadline := time.Now().Add(l.readiness())
	for {
		record, err := l.Store.Load(id)
		if err != nil {
			return Record{}, err
		}
		switch {
		case record.Status.State == Running, record.Status.State.Terminal():
			return record, nil
		case time.Now().After(deadline):
			return record, ErrNotReady
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l Launcher) readiness() time.Duration {
	if l.Readiness <= 0 {
		return DefaultReadiness
	}
	return l.Readiness
}

// BootstrapPath is where a supervisor's own output goes before it has a run to
// write to.
func (c *Store) BootstrapPath(id string) string {
	return filepath.Join(c.Dir(id), "bootstrap.log")
}
