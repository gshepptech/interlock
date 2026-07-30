// Package gitstate reports what changed in a working tree.
//
// It exists for the one case the guard cannot decide in advance: a shell command
// whose targets are not on the command line. `python3 build.py` writes somewhere,
// and nothing before it runs says where. Blocking every such command would stop
// normal work, and allowing it silently means the guard has a hole it does not
// admit to.
//
// So the write is allowed, the tree is snapshotted either side of it, and
// anything that landed outside the agent's territory is reported afterwards.
// After the fact is worse than before, but an agent that is told it wrote in the
// wrong place can revert it, and being told is strictly better than nobody
// knowing.
//
// git is the right tool for this rather than walking the tree: it already knows
// what changed, and it is fast at saying so.
package gitstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrNotAvailable means the tree cannot be watched: no git, or not a repo.
// Callers fall back to reporting the write rather than verifying it.
var ErrNotAvailable = errors.New("git is not available for this working tree")

// timeout bounds the git call. This runs inside an agent's tool call, so a git
// invocation that hangs must not hang the agent.
const timeout = 5 * time.Second

// file is one path git reports, with enough detail to notice a second change to
// a file that was already dirty.
//
// The status alone is not enough: a file modified before the command and again
// by it reads as "M" both times. Size and modification time move when the
// content does.
type file struct {
	Status string `json:"status"`
	Size   int64  `json:"size"`
	ModNs  int64  `json:"mod_ns"`
}

// Snapshot is the state of a working tree at one moment.
type Snapshot struct {
	Files map[string]file `json:"files"`
}

// Take records the working tree.
//
// Paths ignored by .gitignore are not included, because git does not report
// them. That is the right call anyway: a file the repo has chosen not to track
// is not one teams contend over.
func Take(root string) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z",
		"--untracked-files=all")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAvailable, err)
	}

	snap := &Snapshot{Files: map[string]file{}}
	for record := range bytes.SplitSeq(out, []byte{0}) {
		if len(record) < 4 {
			continue
		}
		// "XY path", where XY is the two-character status.
		status := string(record[:2])
		path := string(record[3:])
		if path == "" {
			continue
		}
		snap.Files[path] = statFile(root, path, status)
	}
	return snap, nil
}

func statFile(root, path, status string) file {
	f := file{Status: status}
	if info, err := os.Stat(filepath.Join(root, path)); err == nil {
		f.Size = info.Size()
		f.ModNs = info.ModTime().UnixNano()
	}
	return f
}

// Changed returns the paths that appeared or moved between two snapshots,
// sorted. A missing snapshot on either side yields nothing, because "we do not
// know" must not read as "nothing happened".
func Changed(before, after *Snapshot) []string {
	if before == nil || after == nil {
		return nil
	}

	var changed []string
	for path, now := range after.Files {
		was, existed := before.Files[path]
		switch {
		case !existed:
			changed = append(changed, path)
		case was.Status != now.Status, was.Size != now.Size, was.ModNs != now.ModNs:
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed
}

// Available reports whether a tree can be watched at all, so a caller can say so
// once at install time instead of discovering it mid-run.
func Available(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
