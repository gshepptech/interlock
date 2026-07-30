package session

import (
	"testing"
	"time"
)

func store(t *testing.T, clock *time.Time) *Store {
	t.Helper()
	s, err := open(t.TempDir(), func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestSeenCreatesThenUpdates(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("s1", "claude", "backend", "opus", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	first, err := s.Active()
	if err != nil || len(first) != 1 {
		t.Fatalf("Active = %v err=%v, want one session", first, err)
	}
	if first[0].Agent != "claude" || first[0].Team != "backend" {
		t.Errorf("session = %+v, want claude/backend", first[0])
	}

	now = now.Add(time.Minute)
	if err := s.Seen("s1", "claude", "backend", "opus", "/repo"); err != nil {
		t.Fatalf("second Seen: %v", err)
	}
	again, _ := s.Active()
	if len(again) != 1 {
		t.Fatalf("got %d sessions, want the same one updated", len(again))
	}
	if !again[0].LastSeen.After(first[0].LastSeen) {
		t.Error("LastSeen did not advance")
	}
	if !again[0].Started.Equal(first[0].Started) {
		t.Error("Started changed, losing how long the session has been running")
	}
}

// Hooks can be installed mid-session, and a resumed session may never send a
// SessionStart. Any event has to be enough to know someone is there.
func TestTouchedOnItsOwnRegistersTheSession(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Touched("s1", "Write", []string{"src/api/users.go"}); err != nil {
		t.Fatalf("Touched: %v", err)
	}
	active, err := s.Active()
	if err != nil || len(active) != 1 {
		t.Fatalf("Active = %v err=%v, want the session to exist", active, err)
	}
	if len(active[0].Recent) != 1 || active[0].Recent[0].Path != "src/api/users.go" {
		t.Errorf("Recent = %+v, want the touched file", active[0].Recent)
	}
}

// A later event knows more than an earlier one. Overwriting a known team with an
// empty string would make the dashboard lose information as the session went on.
func TestKnownFieldsAreNotOverwrittenWithEmpty(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("s1", "claude", "backend", "opus", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := s.Seen("s1", "", "", "", ""); err != nil {
		t.Fatalf("Seen with blanks: %v", err)
	}

	active, _ := s.Active()
	got := active[0]
	if got.Agent != "claude" || got.Team != "backend" || got.Model != "opus" || got.CWD != "/repo" {
		t.Errorf("session = %+v, want the earlier values preserved", got)
	}
}

func TestRecentIsNewestFirstAndBounded(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	for i := range RecentTouches + 5 {
		now = now.Add(time.Second)
		path := "f" + string(rune('a'+i)) + ".go"
		if err := s.Touched("s1", "Write", []string{path}); err != nil {
			t.Fatalf("Touched: %v", err)
		}
	}

	active, _ := s.Active()
	recent := active[0].Recent
	if len(recent) != RecentTouches {
		t.Fatalf("got %d touches, want the ring capped at %d", len(recent), RecentTouches)
	}
	for i := 1; i < len(recent); i++ {
		if recent[i].At.After(recent[i-1].At) {
			t.Errorf("touch %d is newer than %d, want newest first", i, i-1)
		}
	}
}

// A loop editing one file must not fill the ring with that one file and hide
// everything else the session did.
func TestRepeatedTouchMovesToFrontWithoutDuplicating(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	for _, p := range []string{"a.go", "b.go", "a.go"} {
		now = now.Add(time.Second)
		if err := s.Touched("s1", "Write", []string{p}); err != nil {
			t.Fatalf("Touched: %v", err)
		}
	}

	active, _ := s.Active()
	recent := active[0].Recent
	if len(recent) != 2 {
		t.Fatalf("Recent = %+v, want 2 distinct files", recent)
	}
	if recent[0].Path != "a.go" {
		t.Errorf("front = %q, want the re-touched a.go", recent[0].Path)
	}
}

// Agents have no dependable exit event, so idling out is the only thing that
// stops a crashed session showing as live forever.
func TestIdleSessionsAreHiddenButNotDeleted(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("s1", "claude", "backend", "", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	now = now.Add(IdleAfter + time.Minute)

	active, err := s.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("Active = %+v, want none once idle", active)
	}

	// Reading must not mutate, so a later Seen resurrects the same session
	// rather than starting a new one.
	if err := s.Seen("s1", "claude", "backend", "", "/repo"); err != nil {
		t.Fatalf("Seen after idle: %v", err)
	}
	back, _ := s.Active()
	if len(back) != 1 {
		t.Fatalf("got %d, want the session back", len(back))
	}
	if back[0].Started.Equal(back[0].LastSeen) {
		t.Error("Started was reset, so the state file was discarded rather than reused")
	}
}

func TestEndRemovesTheSession(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("s1", "claude", "backend", "", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := s.Seen("s2", "codex", "frontend", "", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := s.End("s1"); err != nil {
		t.Fatalf("End: %v", err)
	}

	active, _ := s.Active()
	if len(active) != 1 || active[0].ID != "s2" {
		t.Errorf("Active = %+v, want only s2", active)
	}
}

// Ending a session that was never recorded is normal, not an error: the hook may
// fire in a repo where nothing was ever written.
func TestEndOnUnknownSessionIsNotAnError(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.End("never-existed"); err != nil {
		t.Errorf("End: %v, want nil", err)
	}
}

func TestActiveIsMostRecentlySeenFirst(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("older", "claude", "backend", "", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	now = now.Add(time.Minute)
	if err := s.Seen("newer", "codex", "frontend", "", "/repo"); err != nil {
		t.Fatalf("Seen: %v", err)
	}

	active, _ := s.Active()
	if len(active) != 2 {
		t.Fatalf("got %d sessions, want 2", len(active))
	}
	if active[0].ID != "newer" {
		t.Errorf("first = %q, want newer", active[0].ID)
	}
}

func TestSeenRequiresASessionID(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Seen("", "claude", "backend", "", "/repo"); err == nil {
		t.Error("Seen accepted an empty session id")
	}
}
