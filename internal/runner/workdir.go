package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveDir turns a repo-relative working directory into the absolute path a
// child process may be started in.
//
// The syntax check in Validate is not enough on its own. A path with no ".." in
// it can still leave the repository through a symlink, and the difference only
// shows up once the filesystem is consulted. So this resolves both the root and
// the candidate, then requires the result to be the root or beneath it. A
// directory that passes here is one the agent can be launched in; anything else
// is refused before a run record exists.
func ResolveDir(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: repository root is required", ErrInvalid)
	}
	if err := validateDir(rel); err != nil {
		return "", err
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}

	candidate := filepath.Join(base, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: working directory %s does not exist", ErrInvalid, displayDir(rel))
	}
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: working directory %s is not a directory", ErrInvalid, displayDir(rel))
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: working directory %s resolves outside the repository",
			ErrInvalid, displayDir(rel))
	}
	return resolved, nil
}

func displayDir(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}
