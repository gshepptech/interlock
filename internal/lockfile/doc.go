// Package lockfile provides the one advisory lock this tool uses.
//
// It is flock and not a lock file, and the difference is the whole point. A lock
// file is state that outlives the process that made it, so a crash leaves a
// stale lock and the next run deadlocks. An flock is held by the kernel on
// behalf of an open descriptor: when the process dies, for any reason, the lock
// is released. There is nothing to go stale.
//
// That matters here more than in most programs, because the processes taking
// these locks are agent hooks, which are killed routinely and without warning
// whenever a user interrupts their agent.
package lockfile

import "errors"

// ErrUnsupported reports that this platform has no usable advisory lock.
//
// Failing at construction is deliberate. A silent no-op would let two processes
// both believe they hold the same thing, and a coordination tool that quietly
// stops coordinating is worse than one that refuses to start.
var ErrUnsupported = errors.New(
	"advisory locking is only implemented on unix; Windows support is not built yet")
