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
)

// IdleAfter is how long without a hook firing before a session is treated as
// gone. Agents have no reliable exit event (Codex exposes none at all), so this
// is the only thing that stops a crashed session showing as live forever. It is
// generous because a model can think for a long time between tool calls.
const IdleAfter = 10 * time.Minute

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
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Team     string    `json:"team"`
	Model    string    `json:"model"`
	CWD      string    `json:"cwd"`
	Started  time.Time `json:"started"`
	LastSeen time.Time `json:"last_seen"`
	Recent   []Touch   `json:"recent,omitempty"`
}

// Idle reports whether the session has gone quiet for long enough to be treated
// as gone.
func (s *Session) Idle(now time.Time) bool {
	return now.Sub(s.LastSeen) > IdleAfter
}

// Store is a directory of session state under a repo.
type Store struct {
	dir string
	now func() time.Time
}

// Open prepares the session directory under root.
func Open(root string) (*Store, error) { return open(root, time.Now) }

func open(root string, now func() time.Time) (*Store, error) {
	dir := filepath.Join(root, ".agentcoord", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &Store{dir: dir, now: now}, nil
}

// Seen records that a session is alive, creating it on first sight.
//
// There is no separate register step because SessionStart is not guaranteed to
// have fired: hooks can be installed mid-session, and a session that has been
// resumed may never send one. Any event is enough to know someone is there.
func (s *Store) Seen(id, agent, team, model, cwd string) error {
	if id == "" {
		return errors.New("session id is required")
	}
	cur, err := s.read(id)
	if err != nil {
		now := s.now()
		cur = &Session{ID: id, Started: now}
	}
	cur.LastSeen = s.now()
	// Later events carry better information than the first: a team may be
	// resolved from the environment, a model may only appear on some events.
	// Never overwrite something known with something empty.
	cur.Agent = preferNonEmpty(agent, cur.Agent)
	cur.Team = preferNonEmpty(team, cur.Team)
	cur.Model = preferNonEmpty(model, cur.Model)
	cur.CWD = preferNonEmpty(cwd, cur.CWD)
	return s.write(cur)
}

// Touched records a file a session wrote, keeping the most recent few.
func (s *Store) Touched(id, tool string, paths []string) error {
	if id == "" || len(paths) == 0 {
		return nil
	}
	cur, err := s.read(id)
	if err != nil {
		cur = &Session{ID: id, Started: s.now()}
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
	err := os.Remove(s.pathFor(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}

// Active lists sessions that have been seen recently, most recently seen first.
// Idle sessions are skipped but not deleted, because reading must not mutate.
func (s *Store) Active() ([]*Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session dir: %w", err)
	}

	now := s.now()
	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := readFile(filepath.Join(s.dir, e.Name()))
		if err != nil || sess.Idle(now) {
			continue
		}
		out = append(out, sess)
	}
	slices.SortFunc(out, func(a, b *Session) int {
		return b.LastSeen.Compare(a.LastSeen)
	})
	return out, nil
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
	return &sess, nil
}

func (s *Store) pathFor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

// write installs the file atomically, so a dashboard reading mid-write sees the
// finished file or the previous one, never half of either.
//
// There is no lock. Only the owning session writes its own file, so there is no
// read-modify-write race between sessions to protect against. A session's own
// hooks can overlap, and the loss in that case is one touch entry, which is not
// worth a lock on the hot path.
func (s *Store) write(sess *Session) error {
	enc, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
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
