package lease

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func store(t *testing.T, ttl time.Duration, clock *time.Time) *Store {
	t.Helper()
	s, err := open(t.TempDir(), ttl, func() time.Time { return *clock })
	if err != nil {
		if err == ErrUnsupported {
			t.Skip("advisory locking not implemented on this platform")
		}
		t.Fatalf("open store: %v", err)
	}
	return s
}

func holder(session, team string) Holder {
	return Holder{Team: team, Agent: "claude", SessionID: session, PID: os.Getpid()}
}

func TestFreePathIsGranted(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	granted, l, err := s.Acquire("src/types/api.ts", holder("s1", "backend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !granted {
		t.Fatal("not granted on a free path")
	}
	if l.Team != "backend" || l.Path != "src/types/api.ts" {
		t.Errorf("lease = %+v, want backend holding src/types/api.ts", l)
	}
}

func TestSecondSessionIsRefusedAndToldWho(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	if granted, _, err := s.Acquire("openapi.yaml", holder("s1", "backend")); err != nil || !granted {
		t.Fatalf("first Acquire: granted=%v err=%v", granted, err)
	}

	granted, current, err := s.Acquire("openapi.yaml", holder("s2", "frontend"))
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if granted {
		t.Fatal("granted to a second session while the first still holds it")
	}
	// Refusing without naming the holder would leave the blocked agent nothing
	// to act on, which is the whole point of the message.
	if current == nil || current.SessionID != "s1" || current.Team != "backend" {
		t.Fatalf("current = %+v, want the s1/backend holder", current)
	}
}

// An agent editing one file across several tool calls in a turn is the normal
// case. Treating its own lease as contention would block it from its own work.
func TestSameSessionRenewsInsteadOfBeingRefused(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	_, first, err := s.Acquire("src/types/api.ts", holder("s1", "backend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	now = now.Add(30 * time.Second)
	granted, second, err := s.Acquire("src/types/api.ts", holder("s1", "backend"))
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	if !granted {
		t.Fatal("own session refused its own lease")
	}
	if !second.Renewed.After(first.Renewed) {
		t.Error("Renewed did not advance, so an active file would expire under its holder")
	}
	if !second.Acquired.Equal(first.Acquired) {
		t.Error("Acquired changed on renewal, losing how long the file has been held")
	}
}

// Renewal on every touch is what lets an actively worked file stay held past the
// time to live.
func TestRenewalKeepsALeaseAliveIndefinitely(t *testing.T) {
	now := time.Now()
	s := store(t, time.Minute, &now)

	for range 10 {
		if granted, _, err := s.Acquire("openapi.yaml", holder("s1", "backend")); err != nil || !granted {
			t.Fatalf("renew: granted=%v err=%v", granted, err)
		}
		now = now.Add(50 * time.Second)
	}

	granted, current, err := s.Acquire("openapi.yaml", holder("s2", "frontend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if granted {
		t.Fatal("a continuously renewed lease was taken over")
	}
	if current.SessionID != "s1" {
		t.Errorf("holder = %s, want s1", current.SessionID)
	}
}

// A crashed agent must not hold a contract file for the rest of the session.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	now := time.Now()
	s := store(t, time.Minute, &now)

	if _, _, err := s.Acquire("openapi.yaml", holder("s1", "backend")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	now = now.Add(2 * time.Minute)

	granted, l, err := s.Acquire("openapi.yaml", holder("s2", "frontend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !granted {
		t.Fatal("expired lease was not taken over")
	}
	if l.SessionID != "s2" {
		t.Errorf("holder = %s, want s2", l.SessionID)
	}
}

// A regression test for the bug the cross-process test found. The hook is a
// short-lived process, so the pid on a valid lease is nearly always dead. If a
// dead pid could be stolen from, every lease would be stealable immediately and
// the layer would do nothing at all.
func TestDeadRecordedProcessDoesNotMakeALeaseStealable(t *testing.T) {
	now := time.Now()
	s := store(t, time.Hour, &now)

	// A genuinely dead pid, rather than one guessed to be free.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn throwaway process: %v", err)
	}
	dead := cmd.Process.Pid

	if _, _, err := s.Acquire("openapi.yaml", Holder{
		Team: "backend", Agent: "codex", SessionID: "exited-hook", PID: dead,
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	granted, current, err := s.Acquire("openapi.yaml", holder("s2", "frontend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if granted {
		t.Fatal("a lease was stolen because its recorded pid had exited; " +
			"pid must not decide liveness, only the ttl does")
	}
	if current.SessionID != "exited-hook" {
		t.Errorf("holder = %s, want the original", current.SessionID)
	}
}

func TestReleaseFreesOnlyItsOwnSessionsLeases(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	for _, p := range []string{"a.ts", "b.ts"} {
		if _, _, err := s.Acquire(p, holder("s1", "backend")); err != nil {
			t.Fatalf("Acquire %s: %v", p, err)
		}
	}
	if _, _, err := s.Acquire("c.ts", holder("s2", "frontend")); err != nil {
		t.Fatalf("Acquire c.ts: %v", err)
	}

	released, err := s.Release("s1")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released != 2 {
		t.Errorf("released %d, want 2", released)
	}

	active, err := s.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 1 || active[0].SessionID != "s2" {
		t.Fatalf("active = %+v, want only s2's lease", active)
	}
}

func TestActiveOmitsExpiredWithoutDeletingThem(t *testing.T) {
	now := time.Now()
	s := store(t, time.Minute, &now)

	if _, _, err := s.Acquire("stale.ts", holder("s1", "backend")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	now = now.Add(2 * time.Minute)

	active, err := s.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active = %+v, want none", active)
	}

	// Reading must not mutate, so a reader and a writer never contend.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var found bool
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			found = true
		}
	}
	if !found {
		t.Error("Active deleted the expired lease; reads must not mutate")
	}
}

// The property the whole layer rests on. Without it, two agents can both be told
// they hold the same contract file.
func TestConcurrentAcquireGrantsExactlyOne(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	const racers = 24
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		winner  string
	)

	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			session := "s" + string(rune('a'+i%26)) + string(rune('0'+i/26))
			ok, _, err := s.Acquire("openapi.yaml", holder(session, "team"))
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				winner = session
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != 1 {
		t.Fatalf("%d racers were granted the same path, want exactly 1", granted)
	}

	// The winner must actually be recorded, not merely counted.
	active, err := s.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 1 || active[0].SessionID != winner {
		t.Fatalf("active = %+v, want just %s", active, winner)
	}
}

// A lease file we cannot read must not block a contract file forever.
func TestUnreadableLeaseIsReplaced(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	if err := os.WriteFile(s.pathFor("openapi.yaml"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("plant corrupt lease: %v", err)
	}

	granted, l, err := s.Acquire("openapi.yaml", holder("s1", "backend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !granted || l.SessionID != "s1" {
		t.Fatalf("granted=%v lease=%+v, want s1 to take over a corrupt lease", granted, l)
	}
}

// Paths contain separators and can be long, so the filename is derived. Two
// different paths must not collide onto one lease.
func TestDifferentPathsDoNotShareALease(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	if granted, _, _ := s.Acquire("src/types/a.ts", holder("s1", "backend")); !granted {
		t.Fatal("first Acquire refused")
	}
	granted, _, err := s.Acquire("src/types/b.ts", holder("s2", "frontend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !granted {
		t.Fatal("a different path was refused, so paths are colliding")
	}
}

// The common case has no teams at all, and a message reading `the "" team`
// reads like a bug to whoever sees it, agent or human.
func TestDescribeReadsWellWithNoTeam(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	_, l, err := s.Acquire("src/shared.ts", Holder{
		Agent: "claude", SessionID: "abcdef123456", PID: os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got := l.Describe(now.Add(30 * time.Second))
	if strings.Contains(got, `""`) {
		t.Errorf("Describe() = %q, contains an empty quoted team", got)
	}
	if !strings.Contains(got, "claude") || !strings.Contains(got, "src/shared.ts") {
		t.Errorf("Describe() = %q, missing the holder or the path", got)
	}
}

func TestDescribeNamesTheHolderAndHowLong(t *testing.T) {
	now := time.Now()
	s := store(t, DefaultTTL, &now)

	_, l, err := s.Acquire("openapi.yaml", holder("session-abcdef123456", "frontend"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got := l.Describe(now.Add(90 * time.Second))
	for _, want := range []string{"openapi.yaml", "frontend", "claude", "1m30s"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
}
