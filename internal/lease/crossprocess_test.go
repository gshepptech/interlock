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

// Every real acquisition happens in a separate process: each hook invocation is
// its own short-lived binary. Racing goroutines share a process, and flock has
// different semantics within one, so the in-process test does not prove the
// deployed behaviour. This one does.

const (
	envDir     = "INTERLOCK_TEST_LEASE_DIR"
	envSession = "INTERLOCK_TEST_LEASE_SESSION"
)

// TestHelperAcquireOnce is a child process, not a test. It skips unless the
// parent has set the environment, which is the standard way to re-exec a test
// binary as a helper.
func TestHelperAcquireOnce(t *testing.T) {
	dir := os.Getenv(envDir)
	if dir == "" {
		t.Skip("not running as a helper")
	}

	s, err := open(dir, DefaultTTL, time.Now)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Wait for the parent's starting gun so the children genuinely overlap
	// rather than being serialised by process startup.
	gate := filepath.Join(dir, "go")
	for range 20000 {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(200 * time.Microsecond)
	}

	granted, _, err := s.Acquire("openapi.yaml", Holder{
		Team:      "team",
		Agent:     "claude",
		SessionID: os.Getenv(envSession),
		PID:       os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if granted {
		// The parent counts these lines.
		t.Log("HELPER_RESULT granted")
		return
	}
	t.Log("HELPER_RESULT refused")
}

func TestCrossProcessAcquireGrantsExactlyOne(t *testing.T) {
	if _, err := open(t.TempDir(), DefaultTTL, time.Now); err == ErrUnsupported {
		t.Skip("advisory locking not implemented on this platform")
	}

	// Not t.TempDir(): the children need a path that outlives no test cleanup
	// ordering surprises, and this keeps the gate file and store together.
	dir := t.TempDir()

	const children = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		refused int
	)

	wg.Add(children)
	for i := range children {
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestHelperAcquireOnce", "-test.v")
			cmd.Env = append(os.Environ(),
				envDir+"="+dir,
				envSession+"=session-"+string(rune('a'+i)),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("helper %d: %v\n%s", i, err, out)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(string(out), "HELPER_RESULT granted"):
				granted++
			case strings.Contains(string(out), "HELPER_RESULT refused"):
				refused++
			default:
				t.Errorf("helper %d reported nothing usable:\n%s", i, out)
			}
		}()
	}

	// Let every child reach the gate, then release them together.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	wg.Wait()

	if granted != 1 {
		t.Fatalf("granted=%d refused=%d across %d processes, want exactly 1 granted",
			granted, refused, children)
	}
	if granted+refused != children {
		t.Errorf("accounted for %d of %d children", granted+refused, children)
	}
}
