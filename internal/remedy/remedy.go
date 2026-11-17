// Package remedy grants an agent permission to undo its own mistake.
//
// It exists because of a deadlock found by running the thing. A shell command
// whose targets cannot be read in advance is allowed through and checked
// afterwards; when it turns out to have written outside the agent's territory,
// the agent is told to revert it. The agent then tries to delete the file and
// territory blocks that too, because the file is still not its to touch.
//
// The tool demanded a fix and then prevented it. So a path an agent has been
// told to revert becomes writable by that agent, narrowly and briefly: only the
// exact paths it was told about, only for that session, and only for a short
// window. Anything wider would be a hole in territory rather than a way out of a
// deadlock.
package remedy

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

	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/state"
)

// TTL is how long a grant lasts. Long enough for an agent to finish its turn and
// act on what it was told, short enough that it is never a standing permission.
const TTL = 15 * time.Minute

// grant is one session's outstanding permissions.
type grant struct {
	Version int                  `json:"version"`
	Paths   map[string]time.Time `json:"paths"`
	Session string               `json:"session"`
}

// Store holds grants under a repo.
type Store struct {
	dir string
	ttl time.Duration
	now func() time.Time
}

// Open prepares the grant directory under root.
func Open(root string) (*Store, error) { return open(root, TTL, time.Now) }

func open(root string, ttl time.Duration, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".interlock", "remedy")
	return &Store{dir: dir, ttl: ttl, now: now}, nil
}

// Grant lets a session write the given paths, to undo what it just did there.
//
// Re-granting an existing path refreshes it rather than adding a second entry,
// so an agent that is told twice does not end up with a longer window than one
// told once.
func (s *Store) Grant(sessionID string, paths []string) error {
	if sessionID == "" || len(paths) == 0 {
		return nil
	}
	unlock, err := s.lock(sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	g, err := s.read(sessionID)
	if errors.Is(err, os.ErrNotExist) {
		g = &grant{Session: sessionID, Paths: map[string]time.Time{}}
	} else if err != nil {
		return err
	}
	for _, p := range paths {
		g.Paths[p] = s.now()
	}
	return s.write(g)
}

// Allowed reports whether a session may write path to undo its own change.
//
// Reading does not consume the grant. An agent may need more than one tool call
// to revert something, and a permission that evaporated after the first attempt
// would recreate the deadlock it exists to solve. The time to live is what ends
// it.
func (s *Store) Allowed(sessionID, path string) bool {
	if sessionID == "" {
		return false
	}
	g, err := s.read(sessionID)
	if err != nil {
		return false
	}
	granted, ok := g.Paths[path]
	return ok && s.now().Sub(granted) <= s.ttl
}

// Clear drops a session's grants, on a tidy exit.
func (s *Store) Clear(sessionID string) error {
	_, err := s.ClearResult(sessionID)
	return err
}

// ClearResult drops a session's grants and reports whether a file was removed.
func (s *Store) ClearResult(sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	unlock, err := s.lock(sessionID)
	if err != nil {
		return false, err
	}
	defer unlock()

	if _, err := s.read(sessionID); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	err = os.Remove(s.pathFor(sessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("clear remedy: %w", err)
	}
	return err == nil, nil
}

// Outstanding lists the paths a session still owes a fix on, sorted. This is
// what tells an agent at session start that it left something undone.
func (s *Store) Outstanding(sessionID string) []string {
	g, err := s.read(sessionID)
	if err != nil {
		return nil
	}
	var out []string
	for p, at := range g.Paths {
		if s.now().Sub(at) <= s.ttl {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// Inspect scans persisted remedy grants without mutating them.
func (s *Store) Inspect() ([]state.Issue, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read remedy dir: %w", err)
	}
	var issues []state.Issue
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		if _, err := readGrant(path); err != nil {
			issues = append(issues, state.Issue{Store: "remedy", Path: path, Err: err})
		}
	}
	return issues, nil
}

func (s *Store) read(sessionID string) (*grant, error) {
	return readGrant(s.pathFor(sessionID))
}

func readGrant(file string) (*grant, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var g grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("parse remedy grant: %w", err)
	}
	if g.Paths == nil {
		g.Paths = map[string]time.Time{}
	}
	if err := state.Validate("remedy", file, g.Version); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) write(g *grant) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create remedy dir: %w", err)
	}
	g.Version = state.CurrentVersion
	enc, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode remedy grant: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp grant: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp grant: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp grant: %w", err)
	}
	if err := os.Rename(name, s.pathFor(g.Session)); err != nil {
		return fmt.Errorf("install grant: %w", err)
	}
	return nil
}

func (s *Store) pathFor(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

func (s *Store) lock(sessionID string) (func(), error) {
	unlock, err := lockfile.AcquireKeyed(filepath.Join(s.dir, ".locks"), sessionID)
	if err != nil {
		return nil, fmt.Errorf("lock remedy grant: %w", err)
	}
	return unlock, nil
}
