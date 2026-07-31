// Package state defines compatibility rules shared by Interlock's local stores.
package state

import "fmt"

// CurrentVersion is the schema version written by this Interlock release.
const CurrentVersion = 1

// Issue is one unreadable or incompatible persisted record found by a
// read-only store inspection.
type Issue struct {
	Store string
	Path  string
	Err   error
}

func (i Issue) Error() string {
	return fmt.Sprintf("%s %s: %v", i.Store, i.Path, i.Err)
}

// UnsupportedVersionError reports state written by a newer incompatible
// Interlock version. Mutators must preserve the referenced file unchanged.
type UnsupportedVersionError struct {
	Store     string
	Path      string
	Found     int
	Supported int
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("%s state %s has version %d; this binary supports up to %d",
		e.Store, e.Path, e.Found, e.Supported)
}

// Validate accepts legacy unversioned records and the current schema. Unknown
// future versions are rejected so an older binary cannot reinterpret or
// overwrite them. Negative versions are malformed input rather than future
// state.
func Validate(store, path string, version int) error {
	switch {
	case version < 0:
		return fmt.Errorf("%s state %s has invalid version %d", store, path, version)
	case version > CurrentVersion:
		return &UnsupportedVersionError{
			Store: store, Path: path, Found: version, Supported: CurrentVersion,
		}
	default:
		return nil
	}
}
