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
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
)

// Keep is how many entries survive a trim. Enough to cover a working day of
// coordination without the file ever being worth rotating.
const Keep = 200

// trimAt is when a trim happens. Trimming at twice the kept size means the
// rewrite is rare even in a busy repo.
const trimAt = 2 * Keep

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
)

// Known reports whether a kind is one this version renders.
func (k Kind) Known() bool {
	switch k {
	case BlockedTerritory, BlockedLease, EscapedGuard:
		return true
	}
	return false
}

// Entry is one recorded moment.
type Entry struct {
	At        time.Time `json:"at"`
	Kind      Kind      `json:"kind"`
	Agent     string    `json:"agent"`
	Team      string    `json:"team"`
	SessionID string    `json:"session_id"`
	Path      string    `json:"path"`
	// Detail is the human-readable why, already phrased for display.
	Detail string `json:"detail"`
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
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

func (s *Store) all() ([]Entry, error) {
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}

	var entries []Entry
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil || !e.Kind.Known() {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
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

	entries, err := s.all()
	if err != nil || len(entries) <= trimAt {
		return err
	}
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
