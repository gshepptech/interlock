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
	"time"
)

// TTL is how long a grant lasts. Long enough for an agent to finish its turn and
// act on what it was told, short enough that it is never a standing permission.
const TTL = 15 * time.Minute

// grant is one session's outstanding permissions.
type grant struct {
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
	dir := filepath.Join(root, ".agentcoord", "remedy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create remedy dir: %w", err)
	}
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
	g, err := s.read(sessionID)
	if err != nil {
		g = &grant{Session: sessionID, Paths: map[string]time.Time{}}
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
	if sessionID == "" {
		return nil
	}
	err := os.Remove(s.pathFor(sessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear remedy: %w", err)
	}
	return nil
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

func (s *Store) read(sessionID string) (*grant, error) {
	raw, err := os.ReadFile(s.pathFor(sessionID))
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
	return &g, nil
}

func (s *Store) write(g *grant) error {
	enc, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode remedy grant: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp grant: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
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
