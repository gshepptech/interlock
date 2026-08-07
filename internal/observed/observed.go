// Package observed records which agents have actually invoked Interlock's hooks.
//
// It exists because of a failure this tool could not see. Codex changed how it
// records hook trust, stopped accepting the entries Interlock writes, and began
// skipping its hooks silently. Every check still passed: the hooks were in the
// settings file, the trust entries were in the config, and the smoke test ran
// the hook command directly and got a session. Nothing was enforcing, and
// nothing said so.
//
// The mistake was checking our own bookkeeping. Installing a hook is not
// evidence that an agent runs it, and only the agent can supply that evidence.
// So every hook invocation leaves a mark here, and a mark that never appears is
// the one honest signal that an installation is not working.
//
// Records are per agent, not per session, and are never expired. "Codex has
// never once called us in this repository" is exactly the claim worth keeping.
package observed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Record is what one agent has done in one repository.
type Record struct {
	Agent string    `json:"agent"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Count int       `json:"count"`
}

// Store holds one record per agent under a repository.
type Store struct {
	dir string
	now func() time.Time
}

// Open prepares the directory under root.
func Open(root string) (*Store, error) { return open(root, time.Now) }

func open(root string, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".interlock", "observed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create observed dir: %w", err)
	}
	return &Store{dir: dir, now: now}, nil
}

// Mark records that an agent invoked a hook.
//
// Callers should ignore the error. Failing to note that an agent called us is
// not a reason to stop serving it, and the hook must never fail a user's work
// over bookkeeping.
func (s *Store) Mark(agent string) error {
	if agent == "" {
		return errors.New("agent is required")
	}
	record, err := s.Get(agent)
	if err != nil {
		record = &Record{Agent: agent, First: s.now()}
	}
	record.Last = s.now()
	record.Count++
	return s.write(record)
}

// Get returns what is known about one agent.
func (s *Store) Get(agent string) (*Record, error) {
	raw, err := os.ReadFile(s.pathFor(agent))
	if err != nil {
		return nil, err
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("parse observed record: %w", err)
	}
	if record.Agent == "" {
		return nil, fmt.Errorf("observed record for %q has no agent", agent)
	}
	return &record, nil
}

// Seen reports whether an agent has ever invoked a hook here.
func (s *Store) Seen(agent string) bool {
	record, err := s.Get(agent)
	return err == nil && record.Count > 0
}

// All returns every record, sorted by agent for stable display.
func (s *Store) All() ([]*Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read observed dir: %w", err)
	}

	var out []*Record
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		var record Record
		if err := json.Unmarshal(raw, &record); err != nil || record.Agent == "" {
			continue
		}
		out = append(out, &record)
	}
	slices.SortFunc(out, func(a, b *Record) int { return strings.Compare(a.Agent, b.Agent) })
	return out, nil
}

// pathFor keeps one file per agent. Agent names are a closed set of short
// slugs, so they are used directly rather than hashed, which keeps the
// directory readable when someone goes looking.
func (s *Store) pathFor(agent string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, agent)
	return filepath.Join(s.dir, safe+".json")
}

// write installs the record atomically, so a reader never sees half of one.
func (s *Store) write(record *Record) error {
	enc, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode observed record: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp record: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp record: %w", err)
	}
	if err := os.Rename(name, s.pathFor(record.Agent)); err != nil {
		return fmt.Errorf("install record: %w", err)
	}
	return nil
}
