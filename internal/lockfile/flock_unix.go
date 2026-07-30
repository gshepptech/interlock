//go:build unix

package lockfile

import (
	"fmt"
	"os"
	"syscall"
)

// Acquire takes an exclusive advisory lock, blocking until it is available, and
// returns the unlock function.
//
// This is flock and not a lock file, and the difference is the whole reason it
// is safe here. A lock file is state that outlives the process that made it, so
// a crash leaves a stale lock and the next run deadlocks. An flock is held by
// the kernel on behalf of an open descriptor: when the process dies, for any
// reason, the lock is released. There is nothing to go stale.
//
// Callers hold it for microseconds, around a read-decide-write section, and
// never across an agent's tool call.
func Acquire(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() {
		// Closing the descriptor releases the lock. Unlocking first makes the
		// ordering explicit rather than relying on that side effect.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
