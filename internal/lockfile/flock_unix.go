//go:build unix

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Supported reports whether advisory locks are implemented on this platform.
func Supported() bool { return true }

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
		_ = f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() {
		// Closing the descriptor releases the lock. Unlocking first makes the
		// ordering explicit rather than relying on that side effect.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// TryAcquire takes the lock if it is free and reports failure rather than
// waiting.
//
// Acquire is right for the microsecond-long read-decide-write sections
// everywhere else. This exists for the one lock that is held for the whole life
// of a process: a supervisor's claim on its run. There, "someone else already
// holds this" is the answer, not a queue to join, and whether the lock is held
// is also how anything else decides that a supervisor is alive.
func TryAcquire(path string) (unlock func(), acquired bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

// Held reports whether another process is holding the lock at path. It is a
// question about liveness, so it never leaves the lock taken: the answer is
// what happens when we try, and then we let go again.
func Held(path string) (bool, error) {
	unlock, acquired, err := TryAcquire(path)
	if err != nil {
		return false, err
	}
	if !acquired {
		return true, nil
	}
	unlock()
	return false, nil
}
