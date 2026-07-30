// Package lease serialises access to files that more than one team may enter.
//
// Territory removes the collisions that come from two teams being somewhere they
// were never meant to share. What is left is the handful of files both teams
// legitimately need: the type definitions, the API contract, the migration. This
// is the layer for those.
//
// One property shapes the whole design: an agent cannot wait. There is no way to
// park a tool call and resume it when a lock frees. The only primitives are
// allow and block, so a held file must produce a redirect ("someone else has
// this, go and do something else") rather than a queue. Nothing here ever blocks
// waiting for another agent's lease.
package lease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"interlock/internal/lockfile"
)

// DefaultTTL is how long a declared shared file is held without being renewed.
// It has to outlast the gap between one tool call and the next within a single
// turn, and be short enough that a crashed agent does not hold a contract file
// hostage. Renewal on every touch means an actively worked file stays held.
const DefaultTTL = 3 * time.Minute

// ImplicitTTL is how long an ordinary file is held just because someone wrote to
// it. It is much shorter than DefaultTTL on purpose.
//
// With no configuration every write takes a claim, so an agent that touches
// thirty files in a turn would lock its peer out of all thirty. A claim only
// means "I am in here right now", so it should lapse about as fast as attention
// does. Ninety seconds is long enough to cover the gap between two tool calls in
// one turn and short enough that a file someone has moved on from frees quickly.
const ImplicitTTL = 90 * time.Second

// Lease records who holds a path.
type Lease struct {
	Path      string `json:"path"` // repo-relative
	Team      string `json:"team"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	// PID is the hook process that took the lease. It is informational only,
	// for the dashboard and for debugging, and must never be used to decide
	// liveness. See the comment on Store.live.
	PID      int       `json:"pid"`
	Acquired time.Time `json:"acquired"`
	Renewed  time.Time `json:"renewed"`
	// Hold is how long this particular claim survives without renewal. Stored
	// on the lease rather than taken from the store, because an implicit claim
	// and a declared one have different lifetimes and a reader has to honour
	// whichever the writer meant.
	Hold time.Duration `json:"hold,omitempty"`
	// Implicit marks a claim taken simply because the file was written, rather
	// than one covering a file declared shared. Only the display cares.
	Implicit bool `json:"implicit,omitempty"`
}

// Holder identifies the session asking for a lease. SessionID is the identity
// that matters: it is stable across the many short-lived hook processes that one
// agent session spawns.
type Holder struct {
	Team      string
	Agent     string
	SessionID string
	PID       int
}

// Store is a directory of leases under a repo.
type Store struct {
	dir string
	ttl time.Duration
	now func() time.Time
}

// ErrUnsupported is re-exported so callers can check it without importing the
// lock package directly.
var ErrUnsupported = lockfile.ErrUnsupported

// Open prepares the lease directory under root. The directory is created on
// demand, so a repo that never contends never grows one.
func Open(root string) (*Store, error) {
	return open(root, DefaultTTL, time.Now)
}

func open(root string, ttl time.Duration, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".interlock", "leases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lease dir: %w", err)
	}
	return &Store{dir: dir, ttl: ttl, now: now}, nil
}

// Acquire claims path for holder.
//
// It reports granted, plus the current holder when it is refused. Re-entry by
// the same session renews rather than refusing, because an agent editing a file
// across several tool calls in one turn is the normal case, not contention.
func (s *Store) Acquire(path string, h Holder) (granted bool, current *Lease, err error) {
	return s.AcquireFor(path, h, s.ttl, false)
}

// AcquireFor claims path with an explicit hold time. An implicit claim is one
// taken because a file was written rather than because it was declared shared.
func (s *Store) AcquireFor(path string, h Holder, hold time.Duration, implicit bool) (
	granted bool, current *Lease, err error) {
	unlock, err := lockfile.Acquire(filepath.Join(s.dir, ".lock"))
	if err != nil {
		return false, nil, err
	}
	defer unlock()

	file := s.pathFor(path)

	existing, err := readLease(file)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Unheld.
	case err != nil:
		// A lease we cannot read is a lease we cannot honour. Treat it as
		// absent and overwrite it: leaving an unreadable file to block a
		// contract file forever is the worse failure.
		existing = nil
	case existing.SessionID == h.SessionID:
		existing.Renewed = s.now()
		// A file that starts as an implicit claim and turns out to be declared
		// shared should take the longer hold, not keep the shorter one.
		if hold > existing.Hold {
			existing.Hold, existing.Implicit = hold, implicit
		}
		if err := s.write(file, existing); err != nil {
			return false, nil, err
		}
		return true, existing, nil
	case s.live(existing):
		return false, existing, nil
	}

	// Either unheld, unreadable, or expired. The flock above means no other
	// acquirer sits between the read and this write, so replacing it is safe.
	fresh := &Lease{
		Path:      path,
		Team:      h.Team,
		Agent:     h.Agent,
		SessionID: h.SessionID,
		PID:       h.PID,
		Acquired:  s.now(),
		Renewed:   s.now(),
		Hold:      hold,
		Implicit:  implicit,
	}
	if err := s.write(file, fresh); err != nil {
		return false, nil, err
	}
	return true, fresh, nil
}

// Release drops every lease held by a session. Called when a session ends, so a
// tidy exit frees contract files immediately instead of after the time to live.
func (s *Store) Release(sessionID string) (int, error) {
	unlock, err := lockfile.Acquire(filepath.Join(s.dir, ".lock"))
	if err != nil {
		return 0, err
	}
	defer unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("read lease dir: %w", err)
	}

	released := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		file := filepath.Join(s.dir, e.Name())
		l, err := readLease(file)
		if err != nil || l.SessionID != sessionID {
			continue
		}
		if err := os.Remove(file); err == nil {
			released++
		}
	}
	return released, nil
}

// Active lists every live lease, sorted by path so a display is stable between
// calls. Expired leases are skipped but not deleted: reading must not mutate, so
// a reader and a writer never contend and Active needs no lock.
func (s *Store) Active() ([]*Lease, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lease dir: %w", err)
	}

	var out []*Lease
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		l, err := readLease(filepath.Join(s.dir, e.Name()))
		if err != nil || !s.live(l) {
			continue
		}
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b *Lease) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// live reports whether a lease still counts, on time to live alone.
//
// An earlier version also stole a lease whose recorded process was gone, to
// reclaim faster after a crash. That was wrong, and a cross-process test caught
// it: the hook is a short-lived process that exits immediately after acquiring,
// so its pid is dead within milliseconds and every lease looked abandoned. Three
// of twelve racing processes were each granted the same file.
//
// The pid of the hook is not the pid of the agent session, and there is no
// reliable way to reach the latter from inside a hook: the hook may be spawned
// through a shell, so the parent is not dependably the agent either. So liveness
// is the time to live, renewed on every touch, and a tidy exit calls Release.
func (s *Store) live(l *Lease) bool {
	if l == nil {
		return false
	}
	hold := l.Hold
	if hold <= 0 {
		// Written before claims carried their own hold time.
		hold = s.ttl
	}
	return s.now().Sub(l.Renewed) <= hold
}

// pathFor hashes the repo-relative path into a filename. Paths contain
// separators and can be longer than a filename may be, so the path itself lives
// inside the file and the name is derived from it.
func (s *Store) pathFor(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

// write puts the lease in place atomically. A reader that globs mid-write sees
// the finished file or no file, never half of one.
func (s *Store) write(file string, l *Lease) error {
	enc, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode lease: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp lease: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp lease: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp lease: %w", err)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install lease: %w", err)
	}
	return nil
}

func readLease(file string) (*Lease, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("parse lease %s: %w", file, err)
	}
	if l.SessionID == "" {
		return nil, fmt.Errorf("lease %s has no session id", file)
	}
	return &l, nil
}

// Describe renders a held lease for an agent to read. The consumer is a language
// model deciding what to do next, so it names the alternative rather than only
// the refusal.
func (l *Lease) Describe(now time.Time) string {
	held := now.Sub(l.Acquired).Round(time.Second)
	who := l.Agent
	if who == "" {
		who = "another agent"
	}
	// Teams only exist when someone declared them, which is the exception. The
	// common case has no team, and saying `the "" team` reads like a bug.
	if l.Team != "" {
		who += " on the " + l.Team + " team"
	}
	return fmt.Sprintf("%s is being worked on by %s right now (session %s, "+
		"for the last %s).", l.Path, who, shortID(l.SessionID), held)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
