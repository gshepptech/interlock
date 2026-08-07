// Package control is the typed application boundary shared by CLI and HTTP.
package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/cleanup"
	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/diagnostic"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
)

type Actor = journal.Actor

const (
	ActorCLI       = journal.ActorCLI
	ActorDashboard = journal.ActorDashboard
)

type Action = journal.Action
type Outcome = journal.Outcome

const (
	CleanupAll            = journal.CleanupAll
	CleanupSession        = journal.CleanupSession
	ReleaseLeases         = journal.ReleaseLeases
	DoctorSmoke           = journal.DoctorSmoke
	DoctorRepair          = journal.DoctorRepair
	HandoffRequest        = journal.HandoffRequest
	HandoffResolve        = journal.HandoffResolve
	Completed             = journal.Completed
	NoChange              = journal.NoChange
	Rejected              = journal.Rejected
	RejectedConfirmation  = journal.RejectedConfirmation
	Failed                = journal.Failed
	Partial               = journal.Partial
	CompletedAuditWarning = journal.CompletedAuditWarning
	PartialAuditWarning   = journal.PartialAuditWarning
)

type ErrorKind string

const (
	Invalid  ErrorKind = "invalid"
	NotFound ErrorKind = "not_found"
	Conflict ErrorKind = "conflict"
	Internal ErrorKind = "internal"
)

type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func KindOf(err error) ErrorKind {
	var controlErr *Error
	if errors.As(err, &controlErr) {
		return controlErr.Kind
	}
	return Internal
}

type CleanupPayload struct {
	Attempted int
	Removed   int
	Skipped   int
	Failed    int
	Session   *cleanup.SessionResult
}

type Result struct {
	Action     Action
	Outcome    Outcome
	Target     string
	Message    string
	Warning    string
	Cleanup    *CleanupPayload
	Diagnostic *diagnostic.Report
	Repairs    []diagnostic.Repair
	Handoff    *handoffrequest.Item
}

func (r Result) Successful() bool {
	return r.Outcome == Completed || r.Outcome == NoChange ||
		r.Outcome == CompletedAuditWarning
}

type Service struct {
	Root       string
	Executable string
	CodexFile  string
	Now        func() time.Time
	Diagnostic diagnostic.Service
	Config     *config.Config
	// AppendAudit is injectable only at the storage boundary. Nil uses journal.Store.
	AppendAudit func(journal.Entry) error
}

func (s Service) ClearStaleSession(ctx context.Context, actor Actor, sessionID, confirmation string) (Result, error) {
	result := Result{Action: CleanupSession, Target: sessionID}
	if err := s.validateMutation(ctx, actor, sessionID); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != sessionID {
		err := invalid("confirmation must exactly match the session id")
		result.Outcome, result.Message = RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}
	item, err := (cleanup.Service{Root: s.Root, Now: s.Now}).RunSession(sessionID)
	result.Cleanup = &CleanupPayload{Attempted: 1, Session: &item}
	switch item.Outcome {
	case cleanup.Removed:
		result.Outcome, result.Message = Completed, "stale session and associated state removed"
		result.Cleanup.Removed = 1
	case cleanup.NotFound:
		err = &Error{Kind: NotFound, Err: errors.New("session not found")}
		result.Outcome, result.Message = Rejected, err.Error()
		result.Cleanup.Skipped = 1
	case cleanup.SkippedActive:
		err = &Error{Kind: Conflict, Err: errors.New("session is active and was not removed")}
		result.Outcome, result.Message = Rejected, err.Error()
		result.Cleanup.Skipped = 1
	default:
		if err == nil {
			err = errors.New(item.Error)
		}
		err = &Error{Kind: Internal, Err: err}
		result.Outcome, result.Message = Failed, err.Error()
		result.Cleanup.Failed = 1
	}
	return s.finish(actor, result, err)
}

func (s Service) ClearAllStale(ctx context.Context, actor Actor) (Result, error) {
	result := Result{Action: CleanupAll, Target: "all stale sessions", Cleanup: &CleanupPayload{}}
	if err := s.validateMutation(ctx, actor, "all"); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	cleaned, err := (cleanup.Service{Root: s.Root, Now: s.Now}).Run(false)
	result.Cleanup.Attempted = len(cleaned.Sessions)
	for _, item := range cleaned.Sessions {
		switch item.Outcome {
		case cleanup.Removed:
			result.Cleanup.Removed++
		case cleanup.Failed:
			result.Cleanup.Failed++
		default:
			result.Cleanup.Skipped++
		}
	}
	// Sweeping orphaned leases is real work even when no session was stale, so
	// it must not be reported as "nothing happened".
	if cleaned.LeasesPruned > 0 && result.Cleanup.Attempted == 0 {
		result.Outcome = Completed
		result.Message = fmt.Sprintf("no stale sessions; swept %d expired lease(s)",
			cleaned.LeasesPruned)
		return s.finish(actor, result, err)
	}

	switch {
	case result.Cleanup.Attempted == 0:
		result.Outcome, result.Message = NoChange, "no stale sessions"
	case err != nil || result.Cleanup.Failed > 0:
		result.Outcome = Partial
		result.Message = fmt.Sprintf("cleanup partially completed: removed %d, skipped %d, failed %d",
			result.Cleanup.Removed, result.Cleanup.Skipped, result.Cleanup.Failed)
		if err == nil {
			err = errors.New(result.Message)
		}
		err = &Error{Kind: Internal, Err: err}
	default:
		result.Outcome = Completed
		result.Message = fmt.Sprintf("removed stale session state: %d removed, %d skipped",
			result.Cleanup.Removed, result.Cleanup.Skipped)
	}
	return s.finish(actor, result, err)
}

func (s Service) ReleaseSessionLeases(ctx context.Context, actor Actor, sessionID, confirmation string) (Result, error) {
	result := Result{Action: ReleaseLeases, Target: sessionID}
	if err := s.validateMutation(ctx, actor, sessionID); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != sessionID {
		err := invalid("confirmation must exactly match the session id")
		result.Outcome, result.Message = RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}
	store, err := lease.Open(s.Root)
	if err != nil {
		err = &Error{Kind: Internal, Err: err}
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	released, err := store.Release(sessionID)
	if err != nil {
		err = &Error{Kind: Internal, Err: err}
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if released == 0 {
		result.Outcome, result.Message = NoChange, "session held no leases"
	} else {
		result.Outcome = Completed
		result.Message = fmt.Sprintf("released %d leases", released)
	}
	return s.finish(actor, result, nil)
}

// CheckInstallation is a read-only query and deliberately does not append an audit row.
func (s Service) CheckInstallation(ctx context.Context) (Result, error) {
	result := Result{Message: "installation checked"}
	if err := ctx.Err(); err != nil {
		return result, &Error{Kind: Internal, Err: err}
	}
	report, err := s.Diagnostic.Check(s.diagnosticOptions(false))
	if err != nil {
		return result, &Error{Kind: Internal, Err: err}
	}
	result.Diagnostic = &report
	if report.HasFailures() {
		result.Outcome, result.Message = Completed, "installation has failures"
	} else {
		result.Outcome, result.Message = Completed, "installation is healthy"
	}
	return result, nil
}

func (s Service) VerifyInstallation(ctx context.Context, actor Actor) (Result, error) {
	result := Result{Action: DoctorSmoke, Target: "installation"}
	if err := s.validateMutation(ctx, actor, "installation"); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	report, err := s.Diagnostic.Check(s.diagnosticOptions(true))
	if err != nil {
		err = &Error{Kind: Internal, Err: err}
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	result.Diagnostic = &report
	if report.HasFailures() {
		result.Outcome, result.Message = Failed, "installation smoke verification failed"
	} else {
		result.Outcome, result.Message = Completed, "installation smoke verification passed"
	}
	return s.finish(actor, result, nil)
}

func (s Service) RepairInstallation(ctx context.Context, actor Actor, confirmation string, smoke bool) (Result, error) {
	result := Result{Action: DoctorRepair, Target: "installation"}
	if err := s.validateMutation(ctx, actor, "installation"); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != "repair" {
		err := invalid("confirmation must be repair")
		result.Outcome, result.Message = RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}
	repairs, repairErr := s.Diagnostic.Repair(s.diagnosticOptions(false), false)
	result.Repairs = repairs
	report, checkErr := s.Diagnostic.Check(s.diagnosticOptions(smoke))
	result.Diagnostic = &report
	err := errors.Join(repairErr, checkErr)
	changed := false
	for _, repair := range repairs {
		changed = changed || repair.Changed
	}
	switch {
	case err != nil:
		err = &Error{Kind: Internal, Err: err}
		result.Outcome, result.Message = Failed, err.Error()
	case report.HasFailures():
		result.Outcome, result.Message = Failed, "repairs completed but installation still has failures"
	case changed:
		result.Outcome, result.Message = Completed, "safe installation repairs applied"
	default:
		result.Outcome, result.Message = NoChange, "installation already current"
	}
	return s.finish(actor, result, err)
}

func (s Service) RequestHandoff(ctx context.Context, actor Actor, input handoffrequest.Create) (Result, error) {
	result := Result{Action: HandoffRequest, Target: input.SourceSession}
	if err := s.validateMutation(ctx, actor, input.SourceSession); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	input.Requester = requester(actor)
	request, created, err := handoffrequest.Open(s.Root).Create(input)
	if err != nil {
		kind, outcome, message := Internal, Failed, "handoff request could not be stored"
		switch {
		case errors.Is(err, handoffrequest.ErrInvalid):
			kind, outcome, message = Invalid, Rejected, err.Error()
		case errors.Is(err, handoffrequest.ErrCapacity):
			kind = Conflict
			outcome, message = Rejected, err.Error()
		}
		err = &Error{Kind: kind, Err: err}
		result.Outcome, result.Message = outcome, message
		return s.finish(actor, result, err)
	}
	item := handoffrequest.Item{Request: request}
	result.Handoff = &item
	result.Target = request.ID
	if created {
		result.Outcome, result.Message = Completed, "handoff request created"
	} else {
		result.Outcome, result.Message = NoChange, "identical open handoff request already exists"
	}
	return s.finish(actor, result, nil)
}

func (s Service) ResolveHandoff(ctx context.Context, actor Actor, id, confirmation string) (Result, error) {
	result := Result{Action: HandoffResolve, Target: id}
	if err := s.validateMutation(ctx, actor, id); err != nil {
		result.Outcome, result.Message = Failed, err.Error()
		return s.finish(actor, result, err)
	}
	if confirmation != id {
		err := invalid("confirmation must exactly match the request id")
		result.Outcome, result.Message = RejectedConfirmation, err.Error()
		return s.finish(actor, result, err)
	}
	_, resolved, err := handoffrequest.Open(s.Root).Resolve(id, requester(actor))
	if err != nil {
		kind, outcome, message := Internal, Failed, "handoff request could not be resolved"
		switch {
		case errors.Is(err, handoffrequest.ErrInvalid):
			kind, outcome, message = Invalid, Rejected, err.Error()
		case errors.Is(err, handoffrequest.ErrNotFound):
			kind, outcome, message = NotFound, Rejected, err.Error()
		}
		err = &Error{Kind: kind, Err: err}
		result.Outcome, result.Message = outcome, message
		return s.finish(actor, result, err)
	}
	item, err := handoffrequest.Open(s.Root).Show(id)
	if err == nil {
		result.Handoff = &item
	}
	if resolved {
		result.Outcome, result.Message = Completed, "handoff request resolved"
	} else {
		result.Outcome, result.Message = NoChange, "handoff request already resolved"
	}
	return s.finish(actor, result, nil)
}

func (s Service) finish(actor Actor, result Result, operationErr error) (Result, error) {
	entry := journal.Entry{
		Kind: journal.ControlAction, Actor: actor, Action: result.Action,
		Target: result.Target, Result: result.Outcome, Detail: result.Message,
	}
	if result.Cleanup != nil {
		entry.Attempted = result.Cleanup.Attempted
		entry.Removed = result.Cleanup.Removed
		entry.Skipped = result.Cleanup.Skipped
		entry.Failed = result.Cleanup.Failed
	}
	if err := s.appendAudit(entry); err != nil {
		result.Warning = "action audit unavailable: " + err.Error()
		switch result.Outcome {
		case Completed, NoChange:
			result.Outcome = CompletedAuditWarning
		case Partial:
			result.Outcome = PartialAuditWarning
		}
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

func (s Service) validateMutation(ctx context.Context, actor Actor, target string) error {
	if err := ctx.Err(); err != nil {
		return &Error{Kind: Internal, Err: err}
	}
	if s.Root == "" {
		return &Error{Kind: Invalid, Err: errors.New("control root is required")}
	}
	if actor != ActorCLI && actor != ActorDashboard {
		return &Error{Kind: Invalid, Err: errors.New("invalid control actor")}
	}
	if target == "" || len(target) > 128 || strings.ContainsAny(target, "\r\n\x00") {
		return invalid("invalid control target")
	}
	return nil
}

func (s Service) diagnosticOptions(smoke bool) diagnostic.Options {
	return diagnostic.Options{
		Root: s.Root, Executable: s.Executable, CodexFile: s.CodexFile, Smoke: smoke,
		Config: s.Config,
	}
}

func invalid(message string) error {
	return &Error{Kind: Invalid, Err: errors.New(message)}
}

func requester(actor Actor) handoffrequest.Requester {
	if actor == ActorDashboard {
		return handoffrequest.Dashboard
	}
	return handoffrequest.CLI
}
