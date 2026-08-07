// Package instructions maintains Interlock's section in a repository's agent
// instructions file.
//
// This is the only channel into Codex that still works. Codex requires hook
// trust it no longer offers any way to grant, but it reads AGENTS.md on every
// session and it can run commands, which together give the same loop: tell it
// what the state is, let it decide. Proven against a real session, which ran
// the check itself, found a file held by another agent, and refused to edit it
// while editing a free file in the same repository.
//
// The file belongs to the user and usually already says things that matter, so
// Interlock owns exactly one delimited block and never touches a byte outside
// it. The block is rewritten in place when it changes and removed cleanly on
// uninstall.
package instructions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the instructions file every current agent reads.
const FileName = "AGENTS.md"

const (
	startMarker = "<!-- interlock:start -->"
	endMarker   = "<!-- interlock:end -->"
)

// Body is what Interlock asks an agent to do.
//
// It names one command and one rule. Anything longer competes with the user's
// own instructions for the model's attention, and anything vaguer gets
// interpreted rather than followed.
const Body = `## Working alongside other agents

Other AI coding agents may be working in this repository at the same time.

**Before editing a file, run ` + "`interlock check <path>`" + ` from the repository root.**

- Exit code 0 means the file is free. Go ahead.
- Exit code 2 means another agent is editing it right now. Do not edit it, and
  do not reach for another tool or command to get around it. Say you are
  blocked, work on something else, and come back to it later.

This is a scheduling conflict, not a permission one. The other agent will
finish and the file will free up on its own.`

// Change reports what happened to the file.
type Change struct {
	File    string
	Added   bool
	Updated bool
	Removed bool
	Backup  string
}

// Changed reports whether anything was written.
func (c Change) Changed() bool { return c.Added || c.Updated || c.Removed }

// Apply installs or refreshes Interlock's block, leaving everything else alone.
func Apply(root string, dryRun bool) (Change, error) {
	file := filepath.Join(root, FileName)
	change := Change{File: file}

	raw, existed, err := read(file)
	if err != nil {
		return change, err
	}

	desired := startMarker + "\n" + Body + "\n" + endMarker
	current, hasBlock := extract(raw)
	switch {
	case hasBlock && strings.TrimSpace(current) == strings.TrimSpace(desired):
		return change, nil
	case hasBlock:
		change.Updated = true
	default:
		change.Added = true
	}
	if dryRun {
		return change, nil
	}

	var updated string
	if hasBlock {
		updated = replace(raw, desired)
	} else {
		updated = raw
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		if updated != "" {
			updated += "\n"
		}
		updated += desired + "\n"
	}

	if existed {
		backup, err := backupOnce(file)
		if err != nil {
			return change, err
		}
		change.Backup = backup
	}
	return change, write(file, updated)
}

// Remove takes Interlock's block out and leaves the rest of the file.
//
// A file that held nothing but our block is deleted rather than left as an
// empty artefact the user never created.
func Remove(root string, dryRun bool) (Change, error) {
	file := filepath.Join(root, FileName)
	change := Change{File: file}

	raw, existed, err := read(file)
	if err != nil || !existed {
		return change, err
	}
	if _, hasBlock := extract(raw); !hasBlock {
		return change, nil
	}
	change.Removed = true
	if dryRun {
		return change, nil
	}

	updated := strings.TrimSpace(replace(raw, ""))
	if updated == "" {
		return change, os.Remove(file)
	}
	return change, write(file, updated+"\n")
}

// Installed reports whether our block is present and current.
func Installed(root string) (present, current bool) {
	raw, existed, err := read(filepath.Join(root, FileName))
	if err != nil || !existed {
		return false, false
	}
	body, hasBlock := extract(raw)
	if !hasBlock {
		return false, false
	}
	desired := startMarker + "\n" + Body + "\n" + endMarker
	return true, strings.TrimSpace(body) == strings.TrimSpace(desired)
}

// extract returns our block including its markers.
func extract(raw string) (string, bool) {
	start := strings.Index(raw, startMarker)
	if start < 0 {
		return "", false
	}
	end := strings.Index(raw[start:], endMarker)
	if end < 0 {
		// An opening marker with no close is a file someone edited by hand and
		// left broken. Rewriting from it would eat the rest of the document, so
		// it is reported as absent and appended to instead.
		return "", false
	}
	return raw[start : start+end+len(endMarker)], true
}

func replace(raw, with string) string {
	block, ok := extract(raw)
	if !ok {
		return raw
	}
	return strings.Replace(raw, block, with, 1)
}

func read(file string) (string, bool, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", file, err)
	}
	return string(raw), true, nil
}

// write installs the file atomically, so an agent reading it mid-write sees the
// old file or the new one, never half of either.
func write(file, body string) error {
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, ".interlock-instructions-*")
	if err != nil {
		return fmt.Errorf("create temp instructions: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp instructions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp instructions: %w", err)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install instructions: %w", err)
	}
	return nil
}

func backupOnce(file string) (string, error) {
	backup := file + ".interlock-backup"
	if _, err := os.Stat(backup); err == nil {
		return backup, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s for backup: %w", file, err)
	}
	if err := os.WriteFile(backup, raw, 0o644); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return backup, nil
}
