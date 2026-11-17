// Package verification persists working-tree snapshots for opaque shell writes.
package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gshepptech/interlock/internal/gitstate"
	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/state"
)

// Store owns pending verification snapshots for one repository.
type Store struct {
	root string
	dir  string
}

// Open creates a read-only store handle. Mutating methods create directories.
func Open(root string) *Store {
	return &Store{root: root, dir: filepath.Join(root, ".interlock", "pending")}
}

// Arm records the working tree before an opaque command.
func (s *Store) Arm(sessionID string) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	unlock, err := s.lock(sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	file := s.pathFor(sessionID)
	if raw, err := os.ReadFile(file); err == nil {
		var existing gitstate.Snapshot
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("parse pending verification %s: %w", file, err)
		}
		if err := state.Validate("verification", file, existing.Version); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read pending verification: %w", err)
	}

	snapshot, err := gitstate.Take(s.root)
	if err != nil {
		return err
	}
	return s.write(file, snapshot)
}

// Consume returns and removes one valid pending snapshot. Future-version state
// is returned as an error and remains byte-for-byte untouched.
func (s *Store) Consume(sessionID string) (*gitstate.Snapshot, error) {
	unlock, err := s.lock(sessionID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	snapshot, err := s.read(sessionID)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(s.pathFor(sessionID)); err != nil {
		return nil, fmt.Errorf("remove pending verification: %w", err)
	}
	return snapshot, nil
}

// Remove deletes one valid snapshot and reports whether it existed.
func (s *Store) Remove(sessionID string) (bool, error) {
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
	if err := os.Remove(s.pathFor(sessionID)); err != nil {
		return false, fmt.Errorf("remove pending verification: %w", err)
	}
	return true, nil
}

// Inspect scans pending snapshots without consuming them.
func (s *Store) Inspect() ([]state.Issue, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending verification dir: %w", err)
	}
	var issues []state.Issue
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err == nil {
			var snapshot gitstate.Snapshot
			err = json.Unmarshal(raw, &snapshot)
			if err == nil {
				err = state.Validate("verification", path, snapshot.Version)
			}
		}
		if err != nil {
			issues = append(issues, state.Issue{
				Store: "verification", Path: path, Err: err,
			})
		}
	}
	return issues, nil
}

func (s *Store) read(sessionID string) (*gitstate.Snapshot, error) {
	if sessionID == "" {
		return nil, errors.New("session id is required")
	}
	file := s.pathFor(sessionID)
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var snapshot gitstate.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("parse pending verification %s: %w", file, err)
	}
	if err := state.Validate("verification", file, snapshot.Version); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *Store) write(file string, snapshot *gitstate.Snapshot) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create pending verification dir: %w", err)
	}
	snapshot.Version = state.CurrentVersion
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode pending verification: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp verification: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp verification: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp verification: %w", err)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install pending verification: %w", err)
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
		return nil, fmt.Errorf("lock pending verification: %w", err)
	}
	return unlock, nil
}
