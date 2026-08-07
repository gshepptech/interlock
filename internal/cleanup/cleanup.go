// Package cleanup explicitly removes runtime state for expired sessions.
package cleanup

import (
	"errors"
	"fmt"
	"time"

	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/verification"
)

// Outcome reports what happened after a stale candidate was revalidated.
type Outcome string

const (
	Pending       Outcome = "pending"
	Removed       Outcome = "removed"
	SkippedActive Outcome = "skipped_active"
	NotFound      Outcome = "not_found"
	Failed        Outcome = "failed"
)

// SessionResult reports the artifacts removed for one stale session.
type SessionResult struct {
	ID             string
	Agent          string
	LastSeen       time.Time
	LeasesReleased int
	RemedyCleared  bool
	PendingRemoved bool
	SessionRemoved bool
	Outcome        Outcome
	Error          string
}

// Result is the complete cleanup result.
type Result struct {
	DryRun   bool
	Sessions []SessionResult
	// LeasesPruned counts expired leases swept that belonged to no session
	// still on record. Releasing a session takes its own leases with it, but a
	// session that expired long ago leaves its leases behind with nothing left
	// to attribute them to.
	LeasesPruned int
}

// Service coordinates cleanup across the individual runtime stores.
type Service struct {
	Root string
	Now  func() time.Time
}

type stores struct {
	sessions     *session.Store
	leases       *lease.Store
	remedies     *remedy.Store
	verification *verification.Store
}

// Stale returns the sessions cleanup would consider at this moment.
func (s Service) Stale() ([]*session.Session, error) {
	if s.Root == "" {
		return nil, errors.New("cleanup root is required")
	}
	store, err := session.Open(s.Root)
	if err != nil {
		return nil, err
	}
	return store.Stale(s.now().Add(-session.IdleAfter))
}

// Run removes stale state. Each session is revalidated while holding its
// per-session lock, so a session that resumed after enumeration is preserved.
func (s Service) Run(dryRun bool) (Result, error) {
	result := Result{DryRun: dryRun}
	stale, err := s.Stale()
	if err != nil {
		return result, err
	}
	if dryRun {
		for _, candidate := range stale {
			result.Sessions = append(result.Sessions, SessionResult{
				ID: candidate.ID, Agent: candidate.Agent, LastSeen: candidate.LastSeen,
				Outcome: Pending,
			})
		}
		return result, nil
	}

	stores, err := s.openStores()
	if err != nil {
		return result, err
	}
	var allErrs []error
	cutoff := s.now().Add(-session.IdleAfter)

	for _, candidate := range stale {
		item, err := s.runSession(stores, candidate.ID, cutoff)
		if err != nil {
			allErrs = append(allErrs, err)
		}
		result.Sessions = append(result.Sessions, item)
	}

	// Sweep expired leases with no session left to own them. Without this a
	// repository in daily use keeps one file per file ever edited: seven days
	// of real work left 109 expired leases behind 2 live ones, and every read
	// walked all 111.
	pruned, pruneErr := stores.leases.Prune()
	if pruneErr != nil {
		allErrs = append(allErrs, fmt.Errorf("prune expired leases: %w", pruneErr))
	}
	result.LeasesPruned = pruned

	return result, errors.Join(allErrs...)
}

// RunSession removes one session only if it is stale at the final locked
// recheck. Missing and active sessions are typed non-error outcomes.
func (s Service) RunSession(id string) (SessionResult, error) {
	if s.Root == "" {
		return SessionResult{ID: id, Outcome: Failed}, errors.New("cleanup root is required")
	}
	if id == "" {
		return SessionResult{Outcome: Failed}, errors.New("session id is required")
	}
	stores, err := s.openStores()
	if err != nil {
		return SessionResult{ID: id, Outcome: Failed, Error: err.Error()}, err
	}
	return s.runSession(stores, id, s.now().Add(-session.IdleAfter))
}

func (s Service) openStores() (stores, error) {
	sessions, err := session.Open(s.Root)
	if err != nil {
		return stores{}, err
	}
	leases, err := lease.Open(s.Root)
	if err != nil {
		return stores{}, err
	}
	remedies, err := remedy.Open(s.Root)
	if err != nil {
		return stores{}, err
	}
	return stores{
		sessions: sessions, leases: leases, remedies: remedies,
		verification: verification.Open(s.Root),
	}, nil
}

func (s Service) runSession(stores stores, id string, cutoff time.Time) (SessionResult, error) {
	item := SessionResult{ID: id}
	seen, removed, err := stores.sessions.CleanupStale(id, cutoff, func() error {
		var errs []error
		var releaseErr error
		item.LeasesReleased, releaseErr = stores.leases.Release(id)
		if releaseErr != nil {
			errs = append(errs, fmt.Errorf("release leases: %w", releaseErr))
		}
		var remedyErr error
		item.RemedyCleared, remedyErr = stores.remedies.ClearResult(id)
		if remedyErr != nil {
			errs = append(errs, fmt.Errorf("clear remedy: %w", remedyErr))
		}
		var pendingErr error
		item.PendingRemoved, pendingErr = stores.verification.Remove(id)
		if pendingErr != nil {
			errs = append(errs, fmt.Errorf("remove pending verification: %w", pendingErr))
		}
		return errors.Join(errs...)
	})
	if seen != nil {
		item.Agent = seen.Agent
		item.LastSeen = seen.LastSeen
	}
	item.SessionRemoved = removed
	switch {
	case err != nil:
		item.Outcome = Failed
		item.Error = err.Error()
		return item, fmt.Errorf("cleanup session %s: %w", id, err)
	case removed:
		item.Outcome = Removed
	case seen == nil:
		item.Outcome = NotFound
	default:
		item.Outcome = SkippedActive
	}
	return item, nil
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
