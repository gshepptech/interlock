// Package session records what each agent is currently doing.
//
// Leases answer "who holds this file". This answers "who is here and what are
// they working on", which is what a human watching the repo needs and what an
// agent joining a busy repo should be told.
//
// State is one file per session, not an append-only log. A log of every tool call
// grows without bound and needs rotating, and rotation under concurrent appends
// is a problem worth not having. Session state is naturally bounded by how many
// agents are running, and each file carries a short ring of recent touches, which
// is enough to see what someone is doing without keeping their history.
package session

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

// IdleAfter is how long without a hook firing before a session is treated as
// gone. Agents have no reliable exit event (Codex exposes none at all), so this
// is the only thing that stops a crashed session showing as live forever. It is
// generous because a model can think for a long time between tool calls.
const IdleAfter = 10 * time.Minute

// RecentAfter is the short window in which hook recency is shown as recent.
// It is not a claim that the model is actively producing work.
const RecentAfter = 30 * time.Second

// Observation is the presence state that can be inferred from hook timestamps.
type Observation string

const (
	Recent Observation = "recent"
	Quiet  Observation = "quiet"
	Ended  Observation = "ended"
)

// RecentTouches is how many files to remember per session. Enough to show what
// someone is working on, short enough that the file stays small.
const RecentTouches = 8

// Touch is one file a session wrote.
type Touch struct {
	Path string    `json:"path"`
	Tool string    `json:"tool"`
	At   time.Time `json:"at"`
}

// Session is what one agent is doing right now.
type Session struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Team     string    `json:"team"`
	Model    string    `json:"model"`
	CWD      string    `json:"cwd"`
	Started  time.Time `json:"started"`
	LastSeen time.Time `json:"last_seen"`
	Recent   []Touch   `json:"recent,omitempty"`
	// RunID links this session to a run Interlock launched. It is set from the
	// environment the supervisor gave the agent, so it is present exactly when
	// Interlock owns the process and absent for every session it merely
	// observes. Nothing else may be used to make that distinction.
	RunID string `json:"run_id,omitempty"`
}

// Managed reports whether Interlock launched the agent behind this session, as
// opposed to having noticed it through a hook.
func (s *Session) Managed() bool { return s.RunID != "" }

// Idle reports whether the session has gone quiet for long enough to be treated
// as gone.
func (s *Session) Idle(now time.Time) bool {
	return now.Sub(s.LastSeen) > IdleAfter
}

// ObservationAt classifies only what hook recency proves.
func (s *Session) ObservationAt(now time.Time) Observation {
	switch {
	case s.Idle(now):
		return Ended
	case now.Sub(s.LastSeen) > RecentAfter:
		return Quiet
	default:
		return Recent
	}
}

// Store is a directory of session state under a repo.
type Store struct {
	dir string
	now func() time.Time
}

// Open prepares the session directory under root.
func Open(root string) (*Store, error) { return open(root, time.Now) }

func open(root string, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".interlock", "sessions")
	return &Store{dir: dir, now: now}, nil
}

// Sighting is what one hook event knows about the session it came from.
// Fields are filled in as they become available; an empty one never overwrites
// something already known.
type Sighting struct {
	Agent string
	Team  string
	Model string
	CWD   string
	RunID string
}

// Seen records that a session is alive, creating it on first sight.
//
// There is no separate register step because SessionStart is not guaranteed to
// have fired: hooks can be installed mid-session, and a session that has been
// resumed may never send one. Any event is enough to know someone is there.
func (s *Store) Seen(id, agent, team, model, cwd string) error {
	return s.See(id, Sighting{Agent: agent, Team: team, Model: model, CWD: cwd})
}

// See is Seen with everything an event may carry, including the run this
// session belongs to when Interlock started it.
func (s *Store) See(id string, sighting Sighting) error {
	if id == "" {
		return errors.New("session id is required")
	}
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()

	cur, err := s.read(id)
	if errors.Is(err, os.ErrNotExist) {
		now := s.now()
		cur = &Session{ID: id, Started: now}
	} else if err != nil {
		return err
	}
	cur.LastSeen = s.now()
	// Later events carry better information than the first: a team may be
	// resolved from the environment, a model may only appear on some events.
	// Never overwrite something known with something empty.
	cur.Agent = preferNonEmpty(sighting.Agent, cur.Agent)
	cur.Team = preferNonEmpty(sighting.Team, cur.Team)
	cur.Model = preferNonEmpty(sighting.Model, cur.Model)
	cur.CWD = preferNonEmpty(sighting.CWD, cur.CWD)
	cur.RunID = preferNonEmpty(sighting.RunID, cur.RunID)
	return s.write(cur)
}

// Touched records a file a session wrote, keeping the most recent few.
func (s *Store) Touched(id, tool string, paths []string) error {
	if id == "" || len(paths) == 0 {
		return nil
	}
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()

	cur, err := s.read(id)
	if errors.Is(err, os.ErrNotExist) {
		cur = &Session{ID: id, Started: s.now()}
	} else if err != nil {
		return err
	}
	cur.LastSeen = s.now()

	for _, p := range paths {
		// A file written twice should move to the front rather than appear
		// twice, or a loop editing one file fills the whole ring with it.
		cur.Recent = slices.DeleteFunc(cur.Recent, func(t Touch) bool {
			return t.Path == p
		})
		cur.Recent = append([]Touch{{Path: p, Tool: tool, At: s.now()}}, cur.Recent...)
	}
	if len(cur.Recent) > RecentTouches {
		cur.Recent = cur.Recent[:RecentTouches]
	}
	return s.write(cur)
}

// End removes a session's state on a tidy exit.
func (s *Store) End(id string) error {
	if id == "" {
		return nil
	}
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.endLocked(id)
}

func (s *Store) endLocked(id string) error {
	if _, err := s.read(id); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	err := os.Remove(s.pathFor(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}

// Stale lists sessions whose last event is at or before cutoff. It is read-only;
// CleanupStale revalidates each result under the session lock before removal.
func (s *Store) Stale(cutoff time.Time) ([]*Session, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, sess := range all {
		if !sess.LastSeen.After(cutoff) {
			out = append(out, sess)
		}
	}
	slices.SortFunc(out, func(a, b *Session) int {
		return a.LastSeen.Compare(b.LastSeen)
	})
	return out, nil
}

// CleanupStale holds the session lock while rechecking liveness and removing
// associated state through cleanup. The session record is removed last and is
// retained when cleanup fails, so a later run can retry safely.
func (s *Store) CleanupStale(id string, cutoff time.Time, cleanup func() error) (
	*Session, bool, error) {
	if id == "" {
		return nil, false, errors.New("session id is required")
	}
	unlock, err := s.lock(id)
	if err != nil {
		return nil, false, err
	}
	defer unlock()

	sess, err := s.read(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if sess.LastSeen.After(cutoff) {
		return sess, false, nil
	}
	if cleanup != nil {
		if err := cleanup(); err != nil {
			return sess, false, err
		}
	}
	if err := s.endLocked(id); err != nil {
		return sess, false, err
	}
	return sess, true, nil
}

// Active lists sessions that have been seen recently, most recently seen first.
// Idle sessions are skipped but not deleted, because reading must not mutate.
func (s *Store) Active() ([]*Session, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := all[:0]
	for _, sess := range all {
		if !sess.Idle(now) {
			out = append(out, sess)
		}
	}
	return out, nil
}

// All lists every readable session record, including expired observations.
func (s *Store) All() ([]*Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session dir: %w", err)
	}

	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := readFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	slices.SortFunc(out, func(a, b *Session) int {
		return b.LastSeen.Compare(a.LastSeen)
	})
	return out, nil
}

// Get returns one session by its stable agent-provided identifier.
func (s *Store) Get(id string) (*Session, error) {
	if id == "" {
		return nil, errors.New("session id is required")
	}
	return s.read(id)
}

// Inspect scans persisted session records without mutating them.
func (s *Store) Inspect() ([]state.Issue, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}
	var issues []state.Issue
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		if _, err := readFile(path); err != nil {
			issues = append(issues, state.Issue{Store: "session", Path: path, Err: err})
		}
	}
	return issues, nil
}

func (s *Store) read(id string) (*Session, error) { return readFile(s.pathFor(id)) }

func readFile(file string) (*Session, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", file, err)
	}
	if sess.ID == "" {
		return nil, fmt.Errorf("session %s has no id", file)
	}
	if err := state.Validate("session", file, sess.Version); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) pathFor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

func (s *Store) lock(id string) (func(), error) {
	dir := filepath.Join(s.dir, ".locks")
	unlock, err := lockfile.AcquireKeyed(dir, id)
	if err != nil {
		return nil, fmt.Errorf("lock session: %w", err)
	}
	return unlock, nil
}

// write installs the file atomically, so a dashboard reading mid-write sees the
// finished file or the previous one, never half of either.
//
// Callers hold a keyed advisory lock around every read-modify-write operation.
func (s *Store) write(sess *Session) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	sess.Version = state.CurrentVersion
	enc, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp session: %w", err)
	}
	if err := os.Rename(name, s.pathFor(sess.ID)); err != nil {
		return fmt.Errorf("install session: %w", err)
	}
	return nil
}

func preferNonEmpty(next, current string) string {
	if next != "" {
		return next
	}
	return current
}
