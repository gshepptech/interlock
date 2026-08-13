//go:build !unix

package managedrun

import (
	"fmt"
	"io"
	"os"
)

const maxSecureRead = 1 << 20

// readSecure is the portable fallback. It cannot refuse a symlink atomically the
// way the unix build does, so it checks first and accepts the small window
// between the check and the open. Managed runs are a unix feature in this
// release, and this exists so the package still builds and its pure logic can be
// tested elsewhere.
func readSecure(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSecureRead+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > maxSecureRead {
		return nil, fmt.Errorf("%s is larger than a run record should ever be", path)
	}
	return raw, nil
}
