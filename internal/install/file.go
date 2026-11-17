package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// read loads a settings file. A missing file is not an error: it means we are the
// first to write one.
func read(file string) (doc map[string]any, existed bool, err error) {
	doc, snapshot, err := readJSON(file)
	return doc, snapshot.existed, err
}

type fileSnapshot struct {
	raw     []byte
	mode    os.FileMode
	existed bool
}

func readSnapshot(file string) (fileSnapshot, error) {
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{mode: 0o644}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %s: %w", file, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fileSnapshot{}, fmt.Errorf("refusing to modify symlink %s", file)
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("refusing to modify non-regular file %s", file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read %s: %w", file, err)
	}
	return fileSnapshot{raw: raw, mode: info.Mode().Perm(), existed: true}, nil
}

func readJSON(file string) (map[string]any, fileSnapshot, error) {
	snapshot, err := readSnapshot(file)
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	if !snapshot.existed || len(strings.TrimSpace(string(snapshot.raw))) == 0 {
		return map[string]any{}, snapshot, nil
	}
	var doc map[string]any
	raw := snapshot.raw
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Refusing is the only safe move. Overwriting settings we cannot parse
		// would throw away work the user did by hand.
		return nil, snapshot, fmt.Errorf(
			"%s is not valid JSON, so it will not be modified: %w", file, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, snapshot, nil
}

func writeJSON(file string, doc map[string]any, expected fileSnapshot) error {
	enc, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", file, err)
	}
	enc = append(enc, '\n')
	return writeExpected(file, enc, expected)
}

func writeExpected(file string, body []byte, expected fileSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(file), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".interlock-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	mode := expected.mode
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temp file mode: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	current, err := readSnapshot(file)
	if err != nil {
		return err
	}
	if current.existed != expected.existed || !slices.Equal(current.raw, expected.raw) {
		return fmt.Errorf("%s changed while Interlock was preparing an update; no changes were written", file)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install %s: %w", file, err)
	}
	return nil
}

// backupOnce copies the file aside the first time we touch it, and never
// overwrites an existing backup. A second run must not clobber the copy of the
// file as it was before this tool ever ran.
func backupOnce(file string) (string, error) {
	backup := file + ".interlock-backup"
	if info, err := os.Lstat(backup); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing non-regular backup path %s", backup)
		}
		return backup, nil
	}
	snapshot, err := readSnapshot(file)
	if err != nil {
		return "", fmt.Errorf("read %s for backup: %w", file, err)
	}
	f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshot.mode)
	if errors.Is(err, os.ErrExist) {
		return backup, nil
	}
	if err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}
	if _, err := f.Write(snapshot.raw); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return backup, nil
}

// Gitignore adds a line to the repo's .gitignore if it is not already covered.
// Leases and session state are local runtime state, and a tool that quietly adds
// untracked files to someone's repo without saying so is a nuisance.
func Gitignore(root, line string) (bool, error) {
	file := filepath.Join(root, ".gitignore")

	original, err := readSnapshot(file)
	if err != nil {
		return false, err
	}
	raw := original.raw
	for existing := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(existing) == line {
			return false, nil
		}
	}

	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	if err := writeExpected(file, []byte(body), original); err != nil {
		return false, fmt.Errorf("write .gitignore: %w", err)
	}
	return true, nil
}
