package remedy

import (
	"slices"
	"testing"
	"time"
)

func store(t *testing.T, clock *time.Time) *Store {
	t.Helper()
	s, err := open(t.TempDir(), TTL, func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// The deadlock this package exists to break: told to revert, then blocked from
// reverting.
func TestAGrantedPathBecomesWritable(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if s.Allowed("s1", "src/components/Widget.tsx") {
		t.Fatal("allowed before anything was granted")
	}
	if err := s.Grant("s1", []string{"src/components/Widget.tsx"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !s.Allowed("s1", "src/components/Widget.tsx") {
		t.Error("the session cannot undo what it was told to undo")
	}
}

// The grant must be as narrow as the mistake. Anything wider is a hole in
// territory rather than a way out of a deadlock.
func TestOnlyTheGrantedPathAndOnlyThatSession(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"src/components/Widget.tsx"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if s.Allowed("s1", "src/components/Other.tsx") {
		t.Error("a neighbouring path was allowed")
	}
	if s.Allowed("s1", "src/components/") {
		t.Error("the containing directory was allowed")
	}
	if s.Allowed("s2", "src/components/Widget.tsx") {
		t.Error("another session inherited the grant")
	}
}

// An agent may need more than one tool call to revert something. A permission
// that evaporated on first read would recreate the deadlock.
func TestReadingDoesNotConsumeTheGrant(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	for i := range 3 {
		if !s.Allowed("s1", "a.go") {
			t.Fatalf("grant was gone by read %d", i+1)
		}
	}
}

// It must never become a standing permission.
func TestGrantExpires(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	now = now.Add(TTL + time.Minute)

	if s.Allowed("s1", "a.go") {
		t.Error("an expired grant still allows writes")
	}
	if got := s.Outstanding("s1"); len(got) != 0 {
		t.Errorf("Outstanding = %v after expiry, want none", got)
	}
}

// Being told twice must not buy a longer window than being told once, but it
// should refresh rather than stack.
func TestRegrantRefreshesRatherThanDuplicating(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	now = now.Add(TTL - time.Minute)
	if err := s.Grant("s1", []string{"a.go"}); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	// The refresh moved the clock forward, so it survives past the original
	// expiry but not indefinitely.
	now = now.Add(2 * time.Minute)
	if !s.Allowed("s1", "a.go") {
		t.Error("a refreshed grant expired on the original schedule")
	}
	if got := s.Outstanding("s1"); len(got) != 1 {
		t.Errorf("Outstanding = %v, want exactly one entry, not a duplicate", got)
	}
}

func TestOutstandingListsWhatIsStillOwed(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"b.go", "a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	got := s.Outstanding("s1")
	if !slices.Equal(got, []string{"a.go", "b.go"}) {
		t.Errorf("Outstanding = %v, want a.go then b.go sorted", got)
	}
}

func TestClearDropsEverythingForOneSession(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("s1", []string{"a.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := s.Grant("s2", []string{"b.go"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := s.Clear("s1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if s.Allowed("s1", "a.go") {
		t.Error("s1 still has its grant after Clear")
	}
	if !s.Allowed("s2", "b.go") {
		t.Error("Clear took another session's grant")
	}
}

func TestClearOnAnUnknownSessionIsNotAnError(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Clear("never-existed"); err != nil {
		t.Errorf("Clear: %v, want nil", err)
	}
}

func TestEmptyInputsAreIgnored(t *testing.T) {
	now := time.Now()
	s := store(t, &now)

	if err := s.Grant("", []string{"a.go"}); err != nil {
		t.Errorf("Grant with no session: %v", err)
	}
	if err := s.Grant("s1", nil); err != nil {
		t.Errorf("Grant with no paths: %v", err)
	}
	if s.Allowed("", "a.go") {
		t.Error("an empty session id was allowed")
	}
}
