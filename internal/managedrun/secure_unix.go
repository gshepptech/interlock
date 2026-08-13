//go:build unix

package managedrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// maxSecureRead bounds a hardened read. The files read this way are a prompt, a
// token, and two small JSON records, so anything larger is a sign that something
// other than Interlock wrote it.
const maxSecureRead = 1 << 20

// readSecure reads one of a run's own files, refusing anything that is not a
// plain file this user owns.
//
// A run directory holds a prompt and a control token, both of which are secrets,
// and a status file that decides whether a signal may be sent. The check is not
// that the path is trusted, because it is: it is built from a validated run id.
// The check is that the *file at the end of it* is what it claims to be, because
// the run directory sits inside a repository that other tools, other agents, and
// other users of a shared machine can write to. O_NOFOLLOW closes the window
// between deciding a path is safe and opening it; the ownership check closes the
// case where a hard link was left in place of the file.
func readSecure(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if isSymlinkRefusal(err) {
			return nil, fmt.Errorf("%s is a symlink and was not read", path)
		}
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if err := checkOwner(path, info); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSecureRead+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > maxSecureRead {
		return nil, fmt.Errorf("%s is larger than a run record should ever be", path)
	}
	return raw, nil
}

// checkOwner refuses a file that belongs to someone else. On a single-user
// machine this never fires; on a shared one it is the difference between reading
// this repository's run state and reading a file another account planted.
func checkOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); uid >= 0 && int(stat.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not by this user", path, stat.Uid)
	}
	return nil
}

// isSymlinkRefusal reports the errno O_NOFOLLOW produces, which differs between
// Linux (ELOOP) and the BSDs including macOS (EMLINK).
func isSymlinkRefusal(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ELOOP || errno == syscall.EMLINK
}
