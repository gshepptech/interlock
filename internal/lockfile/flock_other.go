//go:build !unix

package lockfile

// See ErrUnsupported for why this fails loudly instead of degrading into a
// no-op: a silent no-op would make every caller race.

func Acquire(string) (func(), error) { return nil, ErrUnsupported }

func TryAcquire(string) (func(), bool, error) { return nil, false, ErrUnsupported }

func Held(string) (bool, error) { return false, ErrUnsupported }

// Supported reports whether advisory locks are implemented on this platform.
func Supported() bool { return false }
