package journal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func store(t *testing.T, clock *time.Time) *Store {
	t.Helper()
	s, err := open(t.TempDir(), func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	return s
}

func entry(kind Kind, path string) Entry {
	return Entry{Kind: kind, Agent: "claude", Team: "backend",
		SessionID: "s1", Path: path, Detail: "because"}
}

func TestRecentIsNewestFirst(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	for _, p := range []string{"a.go", "b.go", "c.go"} {
		now = now.Add(time.Second)
		if err := s.Append(entry(BlockedTerritory, p)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := s.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Path != "c.go" || got[2].Path != "a.go" {
		t.Errorf("order = %s,%s,%s, want c,b,a", got[0].Path, got[1].Path, got[2].Path)
	}
}

func TestRecentRespectsTheLimit(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	for i := range 10 {
		now = now.Add(time.Second)
		if err := s.Append(entry(BlockedLease, string(rune('a'+i))+".go")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := s.Recent(3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d, want 3", len(got))
	}
}

func TestEmptyJournalIsNotAnError(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	got, err := s.Recent(10)
	if err != nil {
		t.Fatalf("Recent on an empty journal: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want none", len(got))
	}
}

// The journal is a display. One bad line must not blank the page.
func TestUnreadableLinesAreSkipped(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Append(entry(BlockedTerritory, "good.go")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	f, err := os.OpenFile(s.file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString("{not json\n\n{\"kind\":\"from_the_future\",\"at\":\"2026-01-01T00:00:00Z\"}\n")
	f.Close()

	if err := s.Append(entry(BlockedLease, "also-good.go")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want the 2 readable ones: %+v", len(got), got)
	}
}

// An unknown kind would render as nothing, so writing one is a bug worth
// reporting rather than a line to silently drop.
func TestAppendRejectsAnUnknownKind(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Append(Entry{Kind: "invented", Path: "x.go"}); err == nil {
		t.Error("Append accepted an unknown kind")
	}
}

func TestAppendStampsTheTime(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Append(entry(BlockedTerritory, "a.go")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := s.Recent(1)
	if got[0].At.IsZero() {
		t.Error("entry has no timestamp")
	}
}

// The number that tells someone whether the tool is earning its place.
func TestCountSinceSplitsByKind(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	start := now

	now = now.Add(time.Minute)
	for range 3 {
		if err := s.Append(entry(BlockedTerritory, "a.go")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Append(entry(EscapedGuard, "b.go")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	counts, err := s.CountSince(start)
	if err != nil {
		t.Fatalf("CountSince: %v", err)
	}
	if counts[BlockedTerritory] != 3 {
		t.Errorf("blocked = %d, want 3", counts[BlockedTerritory])
	}
	if counts[EscapedGuard] != 1 {
		t.Errorf("escaped = %d, want 1", counts[EscapedGuard])
	}

	// Anything before the cutoff must not be counted.
	later, err := s.CountSince(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountSince: %v", err)
	}
	if len(later) != 0 {
		t.Errorf("counts after everything = %v, want empty", later)
	}
}

// The cap has to hold, or this becomes the unbounded log the rest of the design
// deliberately avoids.
func TestJournalIsTrimmedAndKeepsTheNewest(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	// Entries fat enough to cross the size gate quickly.
	fat := strings.Repeat("x", 300)
	total := trimAt + 60
	for i := range total {
		now = now.Add(time.Second)
		e := entry(BlockedTerritory, fat)
		e.Detail = fat
		e.SessionID = "session-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if err := s.Append(e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	got, err := s.Recent(trimAt * 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) > trimAt {
		t.Errorf("journal holds %d entries, want it trimmed at or below %d", len(got), trimAt)
	}
	if len(got) < Keep {
		t.Errorf("journal holds %d entries, want at least the %d kept", len(got), Keep)
	}

	// Trimming must drop the oldest, never the newest.
	last := "session-" + string(rune('a'+(total-1)%26)) + string(rune('0'+(total-1)/26))
	if got[0].SessionID != last {
		t.Errorf("newest = %q, want %q: the trim dropped the wrong end",
			got[0].SessionID, last)
	}
}

// Hooks are separate processes and several can append at once.
func TestConcurrentAppendsAllSurvive(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	const writers = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			e := entry(BlockedTerritory, "f"+string(rune('a'+i))+".go")
			if err := s.Append(e); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.Recent(writers * 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != writers {
		t.Errorf("got %d entries from %d concurrent appends, want all of them",
			len(got), writers)
	}
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	if _, err := open(root, func() time.Time { return now }); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".interlock")); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}
