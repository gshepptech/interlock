// Package journal records the moments this tool actually did something.
//
// A coordination tool is invisible when it works. Two agents do not collide, and
// nobody can tell whether that is because the tool prevented it or because it
// never would have happened. Every block, every redirect, and every write that
// slipped past the guard goes in here, so the dashboard can show what the tool
// has been doing and a person can judge whether it is worth running.
//
// This is a log, which the rest of the design avoids. It is affordable here only
// because entries are rare: a handful per session, not one per tool call. It is
// capped, and the cap is enforced on write.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/state"
)

// Keep is how many entries survive a trim. Enough to cover a working day of
// coordination without the file ever being worth rotating.
const Keep = 200

// trimAt is when a trim happens. Trimming at twice the kept size means the
// rewrite is rare even in a busy repo.
const trimAt = 2 * Keep

// ControlVersion is journal-specific. Other stores remain on state version 1.
// Phase 0 treats these records as future state and therefore refuses to trim a
// mixed journal rather than deleting audit history it cannot interpret.
//
// Version 3 adds the managed-run actions. An older binary reading a v3 row sees
// future state and declines to trim the file, which is the behaviour that keeps
// a mixed journal intact rather than silently shortened.
const ControlVersion = 3

// MinControlVersion is the oldest control record this binary still reads. A
// journal written before managed runs existed is history, and history is not
// improved by discarding it.
const MinControlVersion = 2

var errKindVersion = errors.New("journal kind/version mismatch")

// Kind is what happened. A closed set, because the dashboard renders each one
// differently and an unknown kind would render as nothing.
type Kind string

const (
	// BlockedTerritory is a write into another team's paths, refused.
	BlockedTerritory Kind = "blocked_territory"
	// BlockedLease is a write to a shared file someone else was holding, refused.
	BlockedLease Kind = "blocked_lease"
	// EscapedGuard is a write that reached disk without passing the guard,
	// found afterwards. The tool failed to prevent this one, and saying so is
	// the point: a guard that hides its own misses cannot be trusted.
	EscapedGuard Kind = "escaped_guard"
	// ControlAction is a terminal result from the local control plane.
	ControlAction Kind = "control_action"
)

// Known reports whether a kind is one this version renders.
func (k Kind) Known() bool {
	switch k {
	case BlockedTerritory, BlockedLease, EscapedGuard, ControlAction:
		return true
	}
	return false
}

// Actor is the trusted surface that requested a control action.
type Actor string

const (
	ActorCLI       Actor = "cli"
	ActorDashboard Actor = "dashboard"
)

func (a Actor) known() bool { return a == ActorCLI || a == ActorDashboard }

// Action is one closed control-plane operation.
type Action string

const (
	CleanupAll     Action = "cleanup_all"
	CleanupSession Action = "cleanup_session"
	ReleaseLeases  Action = "release_leases"
	DoctorSmoke    Action = "doctor_smoke"
	DoctorRepair   Action = "doctor_repair"
	HandoffRequest Action = "handoff_request"
	HandoffResolve Action = "handoff_resolve"
	RunStart       Action = "run_start"
	RunPause       Action = "run_pause"
	RunResume      Action = "run_resume"
	RunStop        Action = "run_stop"
	RunForceStop   Action = "run_force_stop"
	RunRetry       Action = "run_retry"
)

func (a Action) known() bool {
	switch a {
	case CleanupAll, CleanupSession, ReleaseLeases, DoctorSmoke, DoctorRepair,
		HandoffRequest, HandoffResolve,
		RunStart, RunPause, RunResume, RunStop, RunForceStop, RunRetry:
		return true
	}
	return false
}

// Outcome is the terminal result of a control action.
type Outcome string

const (
	Completed             Outcome = "completed"
	NoChange              Outcome = "no_change"
	Rejected              Outcome = "rejected"
	RejectedConfirmation  Outcome = "rejected_confirmation"
	Failed                Outcome = "failed"
	Partial               Outcome = "partial"
	CompletedAuditWarning Outcome = "completed_audit_warning"
	PartialAuditWarning   Outcome = "partial_audit_warning"
)

func (o Outcome) known() bool {
	switch o {
	case Completed, NoChange, Rejected, RejectedConfirmation, Failed, Partial,
		CompletedAuditWarning, PartialAuditWarning:
		return true
	}
	return false
}

// Entry is one recorded moment.
type Entry struct {
	Version   int       `json:"version"`
	At        time.Time `json:"at"`
	Kind      Kind      `json:"kind"`
	Agent     string    `json:"agent"`
	Team      string    `json:"team"`
	SessionID string    `json:"session_id"`
	Path      string    `json:"path"`
	// Detail is the human-readable why, already phrased for display.
	Detail string  `json:"detail"`
	Actor  Actor   `json:"actor,omitempty"`
	Action Action  `json:"action,omitempty"`
	Target string  `json:"target,omitempty"`
	Result Outcome `json:"result,omitempty"`
	// Aggregate counts are populated only for bounded bulk cleanup audit rows.
	Attempted int `json:"attempted,omitempty"`
	Removed   int `json:"removed,omitempty"`
	Skipped   int `json:"skipped,omitempty"`
	Failed    int `json:"failed,omitempty"`
}

// Store is the journal file for one repo.
type Store struct {
	file string
	dir  string
	now  func() time.Time
}

// Open prepares the journal under root.
func Open(root string) (*Store, error) { return open(root, time.Now) }

func open(root string, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".interlock")
	return &Store{file: filepath.Join(dir, "journal.jsonl"), dir: dir, now: now}, nil
}

// Append records an entry.
//
// Callers should ignore the error. Failing to record that a block happened is
// not a reason to stop having blocked it, and the hook must never fail a user's
// work over bookkeeping.
func (s *Store) Append(e Entry) error {
	if !e.Kind.Known() {
		return fmt.Errorf("unknown journal kind %q", e.Kind)
	}
	if e.At.IsZero() {
		e.At = s.now()
	}
	if e.Kind == ControlAction {
		e.Version = ControlVersion
		if err := validateControl(e); err != nil {
			return err
		}
	} else {
		e.Version = state.CurrentVersion
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create journal dir: %w", err)
	}

	unlock, err := lockfile.Acquire(filepath.Join(s.dir, "journal.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode journal entry: %w", err)
	}

	f, err := os.OpenFile(s.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close journal: %w", err)
	}

	return s.trimIfLong()
}

// Recent returns up to n entries, newest first.
//
// A line that does not parse, or carries a kind this version does not know, is
// skipped rather than failing the read. The journal is a display, and one bad
// line must not blank the page.
func (s *Store) Recent(n int) ([]Entry, error) {
	entries, err := s.all()
	if err != nil {
		return nil, err
	}
	// Reverse into newest-first without a second allocation pass.
	out := make([]Entry, 0, min(n, len(entries)))
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, entries[i])
	}
	return out, nil
}

// CountSince reports how many entries of each kind happened after t. This is the
// number that tells someone whether the tool is earning its place.
func (s *Store) CountSince(t time.Time) (map[Kind]int, error) {
	entries, err := s.all()
	if err != nil {
		return nil, err
	}
	counts := map[Kind]int{}
	for _, e := range entries {
		if e.At.After(t) {
			counts[e.Kind]++
		}
	}
	return counts, nil
}

// Inspect scans journal lines without mutating or compacting the file.
func (s *Store) Inspect() ([]state.Issue, error) {
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	var issues []state.Issue
	lineNumber := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		lineNumber++
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := s.file + ":" + strconv.Itoa(lineNumber)
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			issues = append(issues, state.Issue{Store: "journal", Path: path, Err: err})
			continue
		}
		if err := validateEntry(path, entry); err != nil {
			issues = append(issues, state.Issue{Store: "journal", Path: path, Err: err})
			continue
		}
		if entry.Kind == ControlAction {
			if err := validateControl(entry); err != nil {
				issues = append(issues, state.Issue{Store: "journal", Path: path, Err: err})
			}
		}
	}
	return issues, nil
}

func (s *Store) all() ([]Entry, error) {
	result, err := s.readAll()
	return result.entries, err
}

type readResult struct {
	entries   []Entry
	hasFuture bool
}

func (s *Store) readAll() (readResult, error) {
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return readResult{}, nil
	}
	if err != nil {
		return readResult{}, fmt.Errorf("read journal: %w", err)
	}

	var result readResult
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if err := validateEntry(s.file, e); err != nil {
			var unsupported *state.UnsupportedVersionError
			if errors.As(err, &unsupported) || errors.Is(err, errKindVersion) {
				result.hasFuture = true
			}
			continue
		}
		if e.Kind == ControlAction && validateControl(e) != nil {
			continue
		}
		result.entries = append(result.entries, e)
	}
	return result, nil
}

func validateEntry(path string, entry Entry) error {
	if err := validateVersion(path, entry.Version); err != nil {
		return err
	}
	if !entry.Kind.Known() {
		return fmt.Errorf("unknown kind %q", entry.Kind)
	}
	if entry.Kind == ControlAction {
		if entry.Version < MinControlVersion {
			return fmt.Errorf("%w: control action version %d, want %d to %d",
				errKindVersion, entry.Version, MinControlVersion, ControlVersion)
		}
		return nil
	}
	if entry.Version > state.CurrentVersion {
		return fmt.Errorf("%w: coordination entry version %d, want 0 or %d",
			errKindVersion, entry.Version, state.CurrentVersion)
	}
	return nil
}

func validateVersion(path string, version int) error {
	switch {
	case version < 0:
		return fmt.Errorf("journal state %s has invalid version %d", path, version)
	case version > ControlVersion:
		return &state.UnsupportedVersionError{
			Store: "journal", Path: path, Found: version, Supported: ControlVersion,
		}
	default:
		return nil
	}
}

// Downgrade is the outcome to record when the action succeeded but the audit
// row could not be written.
//
// The action still happened, and saying otherwise would be the worse lie. What
// changes is that the result carries a warning, so nobody reads the journal
// later and concludes that nothing was done.
func Downgrade(outcome Outcome) Outcome {
	switch outcome {
	case Completed, NoChange:
		return CompletedAuditWarning
	case Partial:
		return PartialAuditWarning
	}
	return outcome
}

func validateControl(e Entry) error {
	if !e.Actor.known() {
		return fmt.Errorf("unknown control actor %q", e.Actor)
	}
	if !e.Action.known() {
		return fmt.Errorf("unknown control action %q", e.Action)
	}
	if !e.Result.known() {
		return fmt.Errorf("unknown control result %q", e.Result)
	}
	return nil
}

// trimIfLong rewrites the file down to the most recent Keep entries. It runs
// under the caller's lock, so no append can interleave with the rewrite.
func (s *Store) trimIfLong() error {
	info, err := os.Stat(s.file)
	if err != nil {
		return nil
	}
	// A cheap size gate first, so the common append does not read the file back.
	// 400 entries of a few hundred bytes is well under this.
	if info.Size() < 64*1024 {
		return nil
	}

	result, err := s.readAll()
	if err != nil || result.hasFuture || len(result.entries) <= trimAt {
		return err
	}
	entries := result.entries
	entries = entries[len(entries)-Keep:]

	var buf strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(s.dir, ".journal-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp journal: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.WriteString(buf.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp journal: %w", err)
	}
	if err := os.Rename(name, s.file); err != nil {
		return fmt.Errorf("install trimmed journal: %w", err)
	}
	return nil
}
