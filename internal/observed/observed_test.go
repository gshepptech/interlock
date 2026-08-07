package observed

import (
	"testing"
	"time"
)

func store(t *testing.T, clock *time.Time) *Store {
	t.Helper()
	s, err := open(t.TempDir(), func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// The whole point: an agent that has never called us must be distinguishable
// from one that has.
func TestUnseenAgentIsReportedUnseen(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if s.Seen("codex") {
		t.Error("an agent that never called us reads as seen")
	}
	if err := s.Mark("codex"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if !s.Seen("codex") {
		t.Error("an agent that called us reads as unseen")
	}
	if s.Seen("claude") {
		t.Error("marking one agent marked another")
	}
}

func TestMarkKeepsFirstAndAdvancesLast(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Mark("claude"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	first, err := s.Get("claude")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	now = now.Add(time.Hour)
	if err := s.Mark("claude"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	second, err := s.Get("claude")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !second.First.Equal(first.First) {
		t.Error("First moved, losing when the agent was first seen")
	}
	if !second.Last.After(first.Last) {
		t.Error("Last did not advance")
	}
	if second.Count != 2 {
		t.Errorf("Count = %d, want 2", second.Count)
	}
}

// Records outlive sessions on purpose. "Codex has never once called us here" is
// the claim worth keeping, and it cannot be made from state that expires.
func TestRecordsDoNotExpire(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Mark("codex"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	now = now.Add(90 * 24 * time.Hour)

	if !s.Seen("codex") {
		t.Error("a record aged out; it must survive so an absence stays meaningful")
	}
}

func TestAllIsSortedAndSkipsRubbish(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	for _, agent := range []string{"codex", "claude"} {
		if err := s.Mark(agent); err != nil {
			t.Fatalf("Mark: %v", err)
		}
	}
	records, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(records) != 2 || records[0].Agent != "claude" || records[1].Agent != "codex" {
		t.Fatalf("All = %+v, want claude then codex", records)
	}
}

func TestMarkRequiresAnAgent(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Mark(""); err == nil {
		t.Error("Mark accepted an empty agent")
	}
}

func TestAllOnAnEmptyStore(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	records, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("All = %+v, want none", records)
	}
}
