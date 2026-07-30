//go:build !unix

package lease

// See ErrUnsupported in lease.go for why this fails loudly instead of degrading
// into a no-op: a silent no-op would make Acquire race.

func lockFile(string) (func(), error) { return nil, ErrUnsupported }
