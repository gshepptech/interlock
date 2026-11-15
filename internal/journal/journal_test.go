package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/state"
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

func TestAppendWritesCurrentVersion(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	if err := s.Append(entry(BlockedLease, "a.go")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Recent(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Recent = %+v err=%v", got, err)
	}
	if got[0].Version != state.CurrentVersion {
		t.Errorf("version = %d, want %d", got[0].Version, state.CurrentVersion)
	}
}

func TestControlEntriesUseJournalVersionTwoAndTypedFields(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	want := Entry{
		Kind: ControlAction, Actor: ActorDashboard, Action: ReleaseLeases,
		Target: "session-1", Result: Completed, Detail: "released 2 leases",
	}
	if err := s.Append(want); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Recent(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Recent = %+v err=%v", got, err)
	}
	if got[0].Version != ControlVersion || got[0].Actor != want.Actor ||
		got[0].Action != want.Action || got[0].Result != want.Result {
		t.Fatalf("control entry = %#v", got[0])
	}
}

// A journal written before managed runs existed is history, and history is not
// improved by discarding it. Version 2 rows stay readable, version 3 rows are
// what this binary writes, and anything newer is left alone.
func TestControlRecordsFromEitherVersionAreStillRead(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := Entry{
		Version: 2, At: now, Kind: ControlAction, Actor: ActorCLI,
		Action: ReleaseLeases, Target: "session-1", Result: Completed,
	}
	line, err := json.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.file, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Append(Entry{
		Kind: ControlAction, Actor: ActorDashboard, Action: RunStop,
		Target: "run-0123456789abcdef01234567", Result: Completed,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Recent(2)
	if err != nil || len(got) != 2 {
		t.Fatalf("Recent = %#v err=%v", got, err)
	}
	if got[0].Action != RunStop || got[0].Version != ControlVersion {
		t.Fatalf("newest = %#v", got[0])
	}
	if got[1].Action != ReleaseLeases || got[1].Version != 2 {
		t.Fatalf("oldest = %#v; a version 2 record must still be readable", got[1])
	}
}

func TestAppendRejectsUntypedControlFields(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	for name, candidate := range map[string]Entry{
		"actor":  {Kind: ControlAction, Actor: "browser", Action: CleanupSession, Result: Completed},
		"action": {Kind: ControlAction, Actor: ActorCLI, Action: "shell", Result: Completed},
		"result": {Kind: ControlAction, Actor: ActorCLI, Action: CleanupSession, Result: "maybe"},
	} {
		if err := s.Append(candidate); err == nil {
			t.Errorf("Append accepted unknown %s", name)
		}
	}
}

func TestMixedJournalPreventsPhaseZeroTrimAndPreservesControlOnPhaseOneTrim(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var body strings.Builder
	for i := range trimAt + 20 {
		e := entry(BlockedLease, "old.go")
		e.Version = state.CurrentVersion
		e.At = now.Add(time.Duration(i) * time.Second)
		e.Detail = strings.Repeat("x", 300)
		raw, _ := json.Marshal(e)
		body.Write(raw)
		body.WriteByte('\n')
	}
	control := Entry{
		Version: ControlVersion, At: now.Add(time.Hour), Kind: ControlAction,
		Actor: ActorCLI, Action: CleanupAll, Result: Completed, Detail: "done",
	}
	raw, _ := json.Marshal(control)
	body.Write(raw)
	body.WriteByte('\n')
	if err := os.WriteFile(s.file, []byte(body.String()), 0o644); err != nil {
		t.Fatalf("seed mixed journal: %v", err)
	}

	// An older binary's compatibility decision: version 2 is future,
	// so its trim path must leave the entire file alone.
	olderBinaryWouldTrim := true
	for line := range strings.SplitSeq(body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var candidate Entry
		if json.Unmarshal([]byte(line), &candidate) != nil {
			continue
		}
		if err := state.Validate("journal", s.file, candidate.Version); err != nil {
			var future *state.UnsupportedVersionError
			if errors.As(err, &future) {
				olderBinaryWouldTrim = false
				break
			}
		}
	}
	if olderBinaryWouldTrim {
		t.Fatal("an older binary would trim a journal containing control records")
	}

	if err := s.trimIfLong(); err != nil {
		t.Fatalf("trim: %v", err)
	}
	entries, err := s.Recent(Keep + 1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != Keep || entries[0].Kind != ControlAction {
		t.Fatalf("trimmed entries = %d newest=%#v", len(entries), entries[0])
	}
}

func TestFutureJournalLinePreventsDestructiveTrim(t *testing.T) {
	now := time.Now()
	s := store(t, &now)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := []byte(`{"version":99,"kind":"blocked_lease","path":"future.go"}` + "\n" +
		strings.Repeat("x", 70*1024))
	if err := os.WriteFile(s.file, original, 0o644); err != nil {
		t.Fatalf("seed future journal: %v", err)
	}
	if err := s.trimIfLong(); err != nil {
		t.Fatalf("trimIfLong: %v", err)
	}
	got, err := os.ReadFile(s.file)
	if err != nil || string(got) != string(original) {
		t.Fatalf("future journal changed: bytes=%d err=%v", len(got), err)
	}
}

func TestMismatchedKindVersionsPreventDestructiveTrim(t *testing.T) {
	for name, mismatch := range map[string]Entry{
		"control_v1": {
			Version: state.CurrentVersion, Kind: ControlAction, Actor: ActorCLI,
			Action: CleanupAll, Result: Completed,
		},
		"coordination_v2": {
			Version: ControlVersion, Kind: BlockedLease, Agent: "claude", Path: "x.go",
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			s := store(t, &now)
			if err := os.MkdirAll(s.dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			raw, _ := json.Marshal(mismatch)
			original := append(append(raw, '\n'), []byte(strings.Repeat("x", 70*1024))...)
			if err := os.WriteFile(s.file, original, 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := s.trimIfLong(); err != nil {
				t.Fatalf("trimIfLong: %v", err)
			}
			after, err := os.ReadFile(s.file)
			if err != nil || string(after) != string(original) {
				t.Fatalf("mismatched known record was trimmed: bytes=%d err=%v", len(after), err)
			}
			issues, err := s.Inspect()
			if err != nil || len(issues) == 0 || !errors.Is(issues[0].Err, errKindVersion) {
				t.Fatalf("Inspect = %#v err=%v", issues, err)
			}
		})
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

func TestOpenDoesNotCreateTheDirectory(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	if _, err := open(root, func() time.Time { return now }); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".interlock")); !os.IsNotExist(err) {
		t.Errorf("open mutated the repository: %v", err)
	}
}
