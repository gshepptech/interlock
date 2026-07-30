//go:build !unix

package lockfile

// See ErrUnsupported for why this fails loudly instead of degrading into a
// no-op: a silent no-op would make every caller race.

func Acquire(string) (func(), error) { return nil, ErrUnsupported }
