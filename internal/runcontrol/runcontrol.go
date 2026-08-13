// Package runcontrol is the typed application boundary for managed runs.
//
// It is the same shape as the control package next to it, and for the same
// reason: the CLI and the dashboard must not be two implementations of the same
// feature that drift apart. Both call these methods, both get the same typed
// errors, and both produce the same audit record. A dashboard button that did
// something subtly different from the command it claims to be is the failure
// this package exists to prevent.
package runcontrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/runner"
)

type (
	Actor   = journal.Actor
	Action  = journal.Action
	Outcome = journal.Outcome
)

const (
	ActorCLI       = journal.ActorCLI
	ActorDashboard = journal.ActorDashboard

	ActionStart     = journal.RunStart
	ActionPause     = journal.RunPause
	ActionResume    = journal.RunResume
	ActionStop      = journal.RunStop
	ActionForceStop = journal.RunForceStop
	ActionRetry     = journal.RunRetry
)

// Result is one terminal outcome, ready for a CLI to print or a handler to
// render.
type Result struct {
	Action  Action
	Outcome Outcome
	Target  string
	Message string
	// Warning is set when the action succeeded but its audit row did not. The
	// action still happened; saying otherwise would be the worse lie.
	Warning string
	Run     *managedrun.Record
}

// Successful reports whether the run ended up as the caller asked.
func (r Result) Successful() bool {
	return r.Outcome == journal.Completed || r.Outcome == journal.NoChange ||
		r.Outcome == journal.CompletedAuditWarning
}

// Service performs run actions and records them.
type Service struct {
	Root string
	// Executable is Interlock's own binary, which is what a supervisor is.
	Executable string
	Now        func() time.Time
	Readiness  time.Duration
	// AppendAudit is injectable only at the storage boundary. Nil uses the
	// repository's journal.
	AppendAudit func(journal.Entry) error
}

// Start is a request for a new run.
type Start struct {
	Options runner.Options
	Prompt  string
}

// Launch creates a run and starts its supervisor.
func (s Service) Launch(ctx context.Context, actor Actor, input Start) (Result, error) {
	result := Result{Action: journal.RunStart, Target: input.Options.Label()}
	if err := s.validate(ctx, actor); err != nil {
		result.Outcome, result.Message = journal.Failed, err.Error()
		return s.finish(actor, result, err)
	}

	store := s.store()
	record, err := store.Create(managedrun.Create{
		Options: input.Options, Prompt: input.Prompt, Creator: creator(actor),
	})
	if err != nil {
		return s.reject(actor, result, err)
	}
	result.Target = record.ID()

	started, err := s.launcher().Start(record.ID())
	result.Run = &started
	switch {
	case errors.Is(err, managedrun.ErrNotReady):
		// Not a failure. The record is durable and the supervisor may still be
		// on its way; the run itself is the thing to look at.
		result.Outcome = journal.Completed
		result.Message = "run created; the agent has not reported yet"
	case err != nil:
		result.Outcome, result.Message = journal.Failed, "the supervisor could not be started"
		return s.finish(actor, result, &control.Error{Kind: control.Internal, Err: err})
	default:
		result.Outcome = journal.Completed
		result.Message = fmt.Sprintf("%s is running", started.Label())
	}
	return s.finish(actor, result, nil)
}

// Retry starts a new run from a finished one.
//
// It deliberately creates a new record rather than reviving the old one. The
// failure is part of the history, and a retry that erased it would make the
// second attempt impossible to explain.
func (s Service) Retry(ctx context.Context, actor Actor, id, confirmation string) (Result, error) {
	result := Result{Action: journal.RunRetry, Target: id}
	if err := s.validate(ctx, actor); err != nil {
		result.Outcome, result.Message = journal.Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != id {
		err := invalid("confirmation must exactly match the run id")
		result.Outcome, result.Message = journal.RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}

	store := s.store()
	source, err := store.Load(id)
	if err != nil {
		return s.reject(actor, result, err)
	}
	if !source.Status.State.Terminal() {
		err := &control.Error{Kind: control.Conflict,
			Err: fmt.Errorf("run %s is %s; stop it before retrying", id, source.Status.State)}
		result.Outcome, result.Message = journal.Rejected, err.Error()
		return s.finish(actor, result, err)
	}
	prompt, err := store.Prompt(id)
	if err != nil {
		return s.reject(actor, result, err)
	}

	record, err := store.Create(managedrun.Create{
		Options: source.Spec.Options, Prompt: string(prompt),
		Creator: creator(actor), RetryOf: id,
	})
	if err != nil {
		return s.reject(actor, result, err)
	}
	result.Target = record.ID()

	started, err := s.launcher().Start(record.ID())
	result.Run = &started
	switch {
	case errors.Is(err, managedrun.ErrNotReady):
		result.Outcome, result.Message = journal.Completed, "retry created; the agent has not reported yet"
	case err != nil:
		result.Outcome, result.Message = journal.Failed, "the supervisor could not be started"
		return s.finish(actor, result, &control.Error{Kind: control.Internal, Err: err})
	default:
		result.Outcome = journal.Completed
		result.Message = fmt.Sprintf("retrying %s as %s", id, record.ID())
	}
	return s.finish(actor, result, nil)
}

// Pause stops the agent and everything it started, keeping its work in place.
func (s Service) Pause(ctx context.Context, actor Actor, id, confirmation string) (Result, error) {
	return s.lifecycle(ctx, actor, journal.RunPause, managedrun.Pause, id, confirmation,
		"the agent and everything it started are stopped")
}

// Resume continues a paused agent.
func (s Service) Resume(ctx context.Context, actor Actor, id string) (Result, error) {
	return s.lifecycle(ctx, actor, journal.RunResume, managedrun.Resume, id, id,
		"the agent has been resumed")
}

// Stop asks the agent to finish. It never escalates: if the agent ignores the
// request, the answer is that force is required, and that remains a decision
// for a person.
func (s Service) Stop(ctx context.Context, actor Actor, id, confirmation string) (Result, error) {
	return s.lifecycle(ctx, actor, journal.RunStop, managedrun.Stop, id, confirmation,
		"the agent was asked to stop")
}

// ForceStop kills the agent and its whole process group.
func (s Service) ForceStop(ctx context.Context, actor Actor, id, confirmation string) (Result, error) {
	return s.lifecycle(ctx, actor, journal.RunForceStop, managedrun.ForceStop, id, confirmation,
		"the agent and everything it started were killed")
}

func (s Service) lifecycle(ctx context.Context, actor Actor, action Action,
	request managedrun.ControlAction, id, confirmation, success string) (Result, error) {
	result := Result{Action: action, Target: id}
	if err := s.validate(ctx, actor); err != nil {
		result.Outcome, result.Message = journal.Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != id {
		err := invalid("confirmation must exactly match the run id")
		result.Outcome, result.Message = journal.RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}

	// Reconcile first, so a user is never told a run was stopped when in truth
	// nothing was listening for it.
	store := s.store()
	record, err := store.ReconcileOne(id)
	if err != nil {
		return s.reject(actor, result, err)
	}
	result.Run = &record
	if record.Status.State.Terminal() {
		err := &control.Error{Kind: control.Conflict,
			Err: fmt.Errorf("run %s has finished as %s", id, record.Describe())}
		result.Outcome, result.Message = journal.Rejected, err.Error()
		return s.finish(actor, result, err)
	}

	response, err := store.Call(id, request)
	if errors.Is(err, managedrun.ErrNoSupervisor) {
		err := &control.Error{Kind: control.Conflict,
			Err: errors.New("no supervisor is listening for this run; it cannot be controlled from here")}
		result.Outcome, result.Message = journal.Rejected, err.Error()
		return s.finish(actor, result, err)
	}
	if err != nil {
		return s.reject(actor, result, err)
	}
	if updated, loadErr := store.Load(id); loadErr == nil {
		result.Run = &updated
	}

	switch response.Outcome {
	case managedrun.Applied:
		result.Outcome, result.Message = journal.Completed, success
	case managedrun.Unchanged:
		result.Outcome, result.Message = journal.NoChange, response.Detail
	case managedrun.ForceRequired:
		err := &control.Error{Kind: control.Conflict, Err: errors.New(
			"the agent has not exited; stop it with force once you are satisfied it is safe")}
		result.Outcome, result.Message = journal.Rejected, err.Error()
		return s.finish(actor, result, err)
	case managedrun.NotRunning:
		err := &control.Error{Kind: control.Conflict, Err: errors.New("the agent is no longer running")}
		result.Outcome, result.Message = journal.Rejected, err.Error()
		return s.finish(actor, result, err)
	default:
		// Everything left is the supervisor refusing to believe the caller, which
		// is a fault rather than a decision the user can act on.
		err := &control.Error{Kind: control.Internal,
			Err: fmt.Errorf("the supervisor refused the request (%s)", response.Outcome)}
		result.Outcome, result.Message = journal.Failed, err.Error()
		return s.finish(actor, result, err)
	}
	return s.finish(actor, result, nil)
}

// List returns every run, reconciled, newest first. Reads are not audited: a
// listing is not an action.
func (s Service) List(ctx context.Context) ([]managedrun.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, &control.Error{Kind: control.Internal, Err: err}
	}
	records, err := s.store().Reconcile()
	if err != nil {
		return nil, &control.Error{Kind: control.Internal, Err: err}
	}
	return records, nil
}

// Show returns one reconciled run.
func (s Service) Show(ctx context.Context, id string) (managedrun.Record, error) {
	if err := ctx.Err(); err != nil {
		return managedrun.Record{}, &control.Error{Kind: control.Internal, Err: err}
	}
	record, err := s.store().ReconcileOne(id)
	if err != nil {
		return managedrun.Record{}, translate(err)
	}
	return record, nil
}

// Logs reads a bounded piece of one stream.
func (s Service) Logs(ctx context.Context, id string, stream managedrun.Stream,
	cursor managedrun.Cursor, limit int64) (managedrun.LogChunk, managedrun.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return managedrun.LogChunk{}, cursor, &control.Error{Kind: control.Internal, Err: err}
	}
	chunk, next, err := s.store().Follow(id, stream, cursor, limit)
	if err != nil {
		return managedrun.LogChunk{}, cursor, translate(err)
	}
	return chunk, next, nil
}

func (s Service) store() *managedrun.Store { return managedrun.Open(s.Root) }

func (s Service) launcher() managedrun.Launcher {
	return managedrun.Launcher{
		Store: s.store(), Executable: s.Executable, Readiness: s.Readiness,
	}
}

func (s Service) validate(ctx context.Context, actor Actor) error {
	if err := ctx.Err(); err != nil {
		return &control.Error{Kind: control.Internal, Err: err}
	}
	if s.Root == "" {
		return &control.Error{Kind: control.Invalid, Err: errors.New("control root is required")}
	}
	if actor != ActorCLI && actor != ActorDashboard {
		return &control.Error{Kind: control.Invalid, Err: errors.New("invalid control actor")}
	}
	return nil
}

// reject turns a store or runner error into the right kind of refusal and
// records it. Every attempted mutation produces exactly one audit row, whether
// or not it succeeded.
func (s Service) reject(actor Actor, result Result, err error) (Result, error) {
	err = translate(err)
	outcome := journal.Rejected
	if control.KindOf(err) == control.Internal {
		outcome = journal.Failed
	}
	result.Outcome, result.Message = outcome, err.Error()
	return s.finish(actor, result, err)
}

// translate maps the storage and runner vocabularies onto the four kinds the
// CLI and HTTP layers know how to present.
func translate(err error) error {
	var already *control.Error
	if errors.As(err, &already) {
		return err
	}
	switch {
	case errors.Is(err, managedrun.ErrNotFound):
		return &control.Error{Kind: control.NotFound, Err: errors.New("run not found")}
	case errors.Is(err, runner.ErrInvalid), errors.Is(err, runner.ErrUnsupported),
		errors.Is(err, runner.ErrNotInstalled), errors.Is(err, managedrun.ErrInvalid):
		return &control.Error{Kind: control.Invalid, Err: err}
	case errors.Is(err, managedrun.ErrCapacity), errors.Is(err, managedrun.ErrConflict),
		errors.Is(err, managedrun.ErrGeneration):
		return &control.Error{Kind: control.Conflict, Err: err}
	}
	return &control.Error{Kind: control.Internal, Err: err}
}

func (s Service) finish(actor Actor, result Result, operationErr error) (Result, error) {
	entry := journal.Entry{
		Kind: journal.ControlAction, Actor: actor, Action: result.Action,
		Target: result.Target, Result: result.Outcome, Detail: result.Message,
	}
	if err := s.appendAudit(entry); err != nil {
		result.Warning = "action audit unavailable: " + err.Error()
		result.Outcome = journal.Downgrade(result.Outcome)
		if operationErr != nil {
			operationErr = errors.Join(operationErr, errors.New(result.Warning))
		}
	}
	return result, operationErr
}

func (s Service) appendAudit(entry journal.Entry) error {
	if s.AppendAudit != nil {
		return s.AppendAudit(entry)
	}
	store, err := journal.Open(s.Root)
	if err != nil {
		return err
	}
	return store.Append(entry)
}

func creator(actor Actor) managedrun.Creator {
	if actor == ActorDashboard {
		return managedrun.CreatorDashboard
	}
	return managedrun.CreatorCLI
}

func invalid(message string) error {
	return &control.Error{Kind: control.Invalid, Err: errors.New(message)}
}
