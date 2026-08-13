// Package managedrun is the durable record of an agent Interlock started.
//
// Everything before this package observes: a hook fires, a session appears, and
// the most that can be said is that something was here recently. A managed run
// is the opposite claim. Interlock launched this process, owns its output, and
// can prove whether it is still alive, so its state is authoritative rather than
// inferred. Keeping the two apart is the whole reason this package exists: a
// word like "running" must never be reachable from hook recency.
//
// A run is three files and a directory. The specification is written once and
// never changed, so what was asked for cannot drift. The status is replaced
// atomically under a per-run lock, so the many processes that read a run list
// never see half of a transition. The prompt and the control token are secrets
// on disk, mode 0600, and never enter a status record, a journal entry, or a
// rendered page.
//
// Two identifiers matter and they do different jobs. The run ID names the
// record and appears in every command. The generation is random, minted with
// the record, and proves that a supervisor answering on a socket is the
// supervisor for *this* run rather than a stale process that inherited its
// path. Nothing in this package or above it may signal a process on the strength
// of a stored PID alone.
package managedrun

import (
	"fmt"
	"slices"
	"time"

	"github.com/gshepptech/interlock/internal/runner"
)

// State is where a run has got to. The set is closed: an unknown state has no
// legal transition out of it and no honest way to render.
type State string

const (
	// Queued means the record exists and no supervisor has claimed it yet.
	Queued State = "queued"
	// Starting means a supervisor holds the run lock and is launching the agent.
	Starting State = "starting"
	// Running means the child process group is alive and producing work.
	Running State = "running"
	// Quiet means the supervisor and process group are live but nothing has been
	// heard for a while. It is a statement about output, not about health, and it
	// is never inferred for a session Interlock did not launch.
	Quiet State = "quiet"
	// Blocked means an Interlock hook refused a write from this run. It is set
	// only by a refusal that carried the run's own identifier, never by matching
	// words in the agent's output.
	Blocked State = "blocked"
	// Paused means the process group is stopped and will resume where it left off.
	Paused State = "paused"
	// Stopping means a graceful interrupt was delivered and the agent has been
	// given its grace window to exit on its own.
	Stopping State = "stopping"
	// Completed means the agent exited zero.
	Completed State = "completed"
	// Failed means the agent exited non-zero, was killed, or never started.
	Failed State = "failed"
	// Lost means Interlock can no longer prove anything about the process. The
	// supervisor is gone and no live socket answers for this generation, so the
	// child may or may not still be running. Saying so is the point: the
	// alternative is signalling a PID that may since have been reused.
	Lost State = "lost"
)

// Known reports whether this release understands the state.
func (s State) Known() bool { return len(transitions[s]) > 0 || s.Terminal() }

// Terminal reports whether the run has finished changing.
func (s State) Terminal() bool {
	return s == Completed || s == Failed || s == Lost
}

// Live reports whether the run is still expected to have a process behind it.
func (s State) Live() bool { return s.Known() && !s.Terminal() }

// transitions is the closed table of legal moves. Everything not listed is a
// conflict, including any move out of a terminal state: a finished run is
// history, and a retry creates a new record rather than rewinding this one.
var transitions = map[State][]State{
	Queued:   {Starting, Failed, Lost},
	Starting: {Running, Failed, Lost},
	Running:  {Quiet, Blocked, Paused, Stopping, Completed, Failed, Lost},
	Quiet:    {Running, Blocked, Paused, Stopping, Completed, Failed, Lost},
	Blocked:  {Running, Quiet, Paused, Stopping, Completed, Failed, Lost},
	Paused:   {Running, Stopping, Completed, Failed, Lost},
	Stopping: {Completed, Failed, Lost},
}

// CanTransition reports whether from -> to is a legal move. A move to the state
// already held is not a transition and is reported as false; callers treat that
// as the idempotent no-change case rather than as a conflict.
func CanTransition(from, to State) bool {
	if from == to || !to.Known() {
		return false
	}
	return slices.Contains(transitions[from], to)
}

// States lists every state in lifecycle order, for documentation and tests.
func States() []State {
	return []State{Queued, Starting, Running, Quiet, Blocked, Paused, Stopping,
		Completed, Failed, Lost}
}

// Creator is the surface that asked for the run. It matches the control-plane
// actors so an audit row and a run record name the same thing.
type Creator string

const (
	CreatorCLI       Creator = "cli"
	CreatorDashboard Creator = "dashboard"
)

func (c Creator) known() bool { return c == CreatorCLI || c == CreatorDashboard }

// Reason is a closed, safe explanation for a terminal or degraded state.
//
// It is closed because it is rendered in HTML and printed in terminals, and
// because the honest alternative, an arbitrary error string from a child
// process, is exactly the sort of text that carries paths and credentials.
type Reason string

const (
	ReasonNone           Reason = ""
	ReasonLaunchFailed   Reason = "launch_failed"
	ReasonExitNonZero    Reason = "exit_nonzero"
	ReasonSignaled       Reason = "signaled"
	ReasonForceStopped   Reason = "force_stopped"
	ReasonStoppedByUser  Reason = "stopped_by_user"
	ReasonSupervisorGone Reason = "supervisor_gone"
)

func (r Reason) known() bool {
	switch r {
	case ReasonNone, ReasonLaunchFailed, ReasonExitNonZero, ReasonSignaled,
		ReasonForceStopped, ReasonStoppedByUser, ReasonSupervisorGone:
		return true
	}
	return false
}

// Describe renders a reason for a person.
func (r Reason) Describe() string {
	switch r {
	case ReasonLaunchFailed:
		return "the agent could not be started"
	case ReasonExitNonZero:
		return "the agent exited with an error"
	case ReasonSignaled:
		return "the agent was terminated by a signal"
	case ReasonForceStopped:
		return "the run was force stopped"
	case ReasonStoppedByUser:
		return "the run was stopped"
	case ReasonSupervisorGone:
		return "the supervisor is gone and the process can no longer be controlled"
	}
	return ""
}

// MaxSummary bounds the free text attached to a status. Long enough for one
// sentence of explanation, short enough that a status file stays small.
const MaxSummary = 256

// Spec is the immutable half of a run. It is written once, with exclusive
// creation, and never rewritten: if what was asked for could change after the
// fact, no later record of what happened would be worth reading.
type Spec struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	// Generation is minted with the record and proves the identity of the
	// supervisor that answers for it. See the package comment.
	Generation string         `json:"generation"`
	Options    runner.Options `json:"options"`
	CreatedAt  time.Time      `json:"created_at"`
	Creator    Creator        `json:"creator"`
	// RetryOf names the run this one was created from. Retry never reuses a
	// record, so the chain stays readable after the fact.
	RetryOf string `json:"retry_of,omitempty"`
}

// Status is the mutable half. Every field here is either authoritative or
// explicitly diagnostic; nothing in it may be used to decide that a process is
// alive.
type Status struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Generation string `json:"generation"`
	State      State  `json:"state"`
	// Sequence increments on every state change and never on a field update, so
	// a caller can tell a genuine transition from a refreshed timestamp.
	Sequence     uint64    `json:"sequence"`
	UpdatedAt    time.Time `json:"updated_at"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	EndedAt      time.Time `json:"ended_at,omitzero"`
	LastOutputAt time.Time `json:"last_output_at,omitzero"`
	// SupervisorPID, ChildPID and ProcessGroup are diagnostic only. Signals are
	// authorised by a live supervisor answering for the run generation, never by
	// these numbers, because a PID can be reused between one read and the next.
	SupervisorPID int    `json:"supervisor_pid,omitempty"`
	ChildPID      int    `json:"child_pid,omitempty"`
	ProcessGroup  int    `json:"process_group,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	Signal        string `json:"signal,omitempty"`
	Reason        Reason `json:"reason,omitempty"`
	Summary       string `json:"summary,omitempty"`
	// ForceRequired marks a graceful stop whose grace window expired. Interlock
	// will not escalate on its own; a person has to ask for the kill.
	ForceRequired bool `json:"force_required,omitempty"`
}

// Record is a run as a reader sees it: what was asked for, and where it got to.
type Record struct {
	Spec   Spec
	Status Status
}

// ID is the run identifier.
func (r Record) ID() string { return r.Spec.ID }

// Label is what a list should call the run.
func (r Record) Label() string { return r.Spec.Options.Label() }

// Runner is the agent this run launched.
func (r Record) Runner() runner.Name { return r.Spec.Options.Runner }

// Age is how long ago the run was created.
func (r Record) Age(now time.Time) time.Duration { return now.Sub(r.Spec.CreatedAt) }

// Duration is how long the run has been going, or how long it ran for.
func (r Record) Duration(now time.Time) time.Duration {
	start := r.Status.StartedAt
	if start.IsZero() {
		start = r.Spec.CreatedAt
	}
	if !r.Status.EndedAt.IsZero() {
		return r.Status.EndedAt.Sub(start)
	}
	return now.Sub(start)
}

// Describe renders the state for a person, including the exit detail that makes
// a bare "failed" actionable.
func (r Record) Describe() string {
	// A run someone stopped is described as stopped whichever way the agent
	// took it. Reporting "completed" or "failed" for a deliberate stop makes an
	// ordinary action read like luck or like a fault.
	switch r.Status.State {
	case Completed:
		if r.Status.Reason == ReasonStoppedByUser {
			return "stopped on request"
		}
		return "completed"
	case Failed:
		switch {
		case r.Status.Reason == ReasonForceStopped:
			return "force stopped"
		case r.Status.Reason == ReasonStoppedByUser:
			return "stopped on request"
		case r.Status.Reason == ReasonLaunchFailed:
			return "failed to start"
		case r.Status.Signal != "":
			return "failed after " + r.Status.Signal
		case r.Status.ExitCode != nil:
			return fmt.Sprintf("failed with exit code %d", *r.Status.ExitCode)
		}
		return "failed"
	case Stopping:
		if r.Status.ForceRequired {
			return "stopping; the agent has not exited and force is required"
		}
		return "stopping"
	case Lost:
		return "lost; the supervisor is gone and the process cannot be controlled"
	}
	return string(r.Status.State)
}

// summarise trims free text down to something safe to store and render. The
// text arrives from agent output, so escape sequences and control characters
// are the hazard, and the runner's sanitiser is the one place that removes them.
func summarise(text string) string { return runner.SafeText(text, MaxSummary) }
