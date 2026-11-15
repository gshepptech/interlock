// Package keep reads what needs doing, so Interlock can start an agent on it.
//
// # The other direction
//
// keep already reads Interlock: its ready queue hides items whose files are
// being edited right now, by shelling out to `interlock status --json`. This is
// the same join pointing the other way, and it is deliberately built the same
// way — a subprocess and a CLI contract, not a Go import.
//
// The reason is that neither tool should depend on the other to be useful. An import would make that false in the direction that matters,
// because Interlock would stop building without keep, and Interlock's whole
// job is keeping agents out of each other's way in repositories that have never
// heard of a work store.
//
// # Every failure is silence
//
// keep missing, failing, slow, or answering with something this cannot read
// all mean the same thing: Interlock has no work list, and every command that
// wanted one says so plainly and does nothing else. None of them is an error
// worth surfacing, because most repositories do not have keep in them.
//
// # One asymmetry worth naming
//
// `interlock status --json` carries a version integer, and keep refuses an
// envelope it does not recognise. `keep ready --json` is a bare array with no
// envelope, so there is no version to check. This validates the shape instead —
// an item without an id is not an item — and treats a malformed element as a
// reason to distrust the whole answer rather than to work from part of it.
package keep

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

// fetchTimeout bounds the subprocess. Nothing here is on a hook path, but a
// user waiting on `interlock run --next` should not wait on a wedged database.
const fetchTimeout = 2 * time.Second

// EnvBinary names the environment variable that overrides how keep is found.
// It mirrors keep's own KEEP_INTERLOCK exactly, including "off", so that
// either tool can be told to ignore the other from either side.
//
//	unset  — look for `keep` on $PATH (the normal case)
//	"off"  — do not consult keep at all
//	other  — use this exact path
const EnvBinary = "INTERLOCK_KEEP"

// Item is one piece of work, narrowed to what Interlock needs to brief an agent
// and to name what it is doing. keep's own record is much wider; carrying
// fields nobody reads would invite code that depends on parts of someone else's
// output that were never promised.
type Item struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Acceptance string `json:"acceptance"`
	Why        string `json:"why"`
	// Paths is comma-separated and repo-relative, in keep's format. It is
	// what makes an item's work scope visible before an agent starts.
	Paths string `json:"paths"`
	Score int    `json:"score"`
}

// Ready runs `keep ready --json` in dir and returns the queue, ranked as
// keep ranked it.
//
// The bool reports whether the answer is usable. It is false, with no error,
// whenever keep is absent, failed, timed out, or said something this cannot
// read.
func Ready(ctx context.Context, dir string) ([]Item, bool) {
	binary, ok := Binary()
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "ready", "--json")
	cmd.Dir = dir
	// Killing the process on timeout is not enough. Anything it spawned
	// inherits the output pipe, so the read keeps blocking after the parent is
	// gone — a two-second bound that measured thirty in a test. WaitDelay gives
	// up on the pipe shortly after the deadline, which is what makes the bound
	// real rather than nominal.
	cmd.WaitDelay = 250 * time.Millisecond
	// Stderr is where keep explains what it held back, which is diagnostic
	// rather than data. Discarding it keeps a held item's explanation out of
	// Interlock's own output, where it would read as Interlock's opinion.
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	var items []Item
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, false
	}
	for _, item := range items {
		// A malformed element means the shape is not what this understands, and
		// working from the rest would be guessing which half to trust.
		if item.ID == "" {
			return nil, false
		}
	}
	return items, true
}

// Binary resolves the keep binary, honouring EnvBinary.
func Binary() (string, bool) {
	value := os.Getenv(EnvBinary)
	switch {
	case value == "off":
		return "", false
	case value != "":
		return value, true
	}
	if path, err := exec.LookPath("keep"); err == nil {
		return path, true
	}
	return "", false
}

// Available reports whether keep can be consulted at all. Commands use it to
// tell a user "there is no work list here" rather than "there is no work".
func Available() bool {
	_, ok := Binary()
	return ok
}

// SplitPaths reads an item's paths field, in keep's format: comma-separated,
// repo-relative, whitespace trimmed. The format is keep's to define, and this
// reads it the same way keep's own SplitPaths does.
func SplitPaths(paths string) []string {
	var out []string
	for part := range strings.SplitSeq(paths, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Brief renders an item as the prompt an agent starts from.
//
// It is deliberately plain and deliberately short. The audience is a model
// about to begin work, so everything that is not the work is noise in its
// context: no identifiers, no scores, no timestamps, no formatting for a
// terminal. What survives is what a person would say to a colleague picking
// the task up.
func (i Item) Brief() string {
	var out strings.Builder
	out.WriteString(i.Title)
	if i.Body != "" {
		out.WriteString("\n\n")
		out.WriteString(i.Body)
	}
	if i.Acceptance != "" {
		out.WriteString("\n\nDone when:\n")
		out.WriteString(i.Acceptance)
	}
	if i.Why != "" {
		out.WriteString("\n\nWhy this exists:\n")
		out.WriteString(i.Why)
	}
	if files := SplitPaths(i.Paths); len(files) > 0 {
		out.WriteString("\n\nExpected to touch: ")
		out.WriteString(strings.Join(files, ", "))
		out.WriteString("\nAnother agent may be in one of these. Run " +
			"`interlock check <path>` before editing, and work on something " +
			"else if it is held.")
	}
	return out.String()
}
