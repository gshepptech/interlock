// Package runner turns a user's launch options into the exact command for one
// coding agent.
//
// Everything vendor-specific about starting Codex or Claude lives here and
// nowhere else: the executable name, the argument order, how the first prompt
// reaches the process, and how its output decodes back into something Interlock
// can display. Nothing in this package starts a process or reads persisted
// state, so an adapter can be exercised in a test without a supervisor, a run
// directory, or a real agent installed.
//
// Two rules shape the whole package. Commands are argument slices, never shell
// strings, so no part of a prompt or a model name can ever be interpreted as a
// command. And an option this release cannot honour is a validation error
// rather than a flag quietly dropped: an agent launched with less isolation
// than the user asked for is worse than an agent that refused to start.
package runner

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ErrInvalid marks options the caller can correct and resubmit. It is
// deliberately separate from a launch failure, which is durable and retried
// rather than corrected.
var ErrInvalid = errors.New("invalid runner options")

// Field limits. These are display and storage bounds rather than security
// boundaries: the argument slice is never a shell string, so length is about
// keeping a run list readable and a status file small.
const (
	MaxNameLen        = 64
	MaxModelLen       = 64
	MaxTeamLen        = 64
	MaxWorkingDirLen  = 512
	MaxEnvironment    = 32
	MaxEnvironmentLen = 64
	// MaxPromptBytes bounds the first prompt. It is the same number the HTTP
	// launcher enforces, so a prompt that a browser accepted cannot then be
	// refused by the store.
	MaxPromptBytes = 32 * 1024
)

// Name is one supported agent CLI. The set is closed because each member needs
// its own adapter; an unknown name has no command to construct.
type Name string

const (
	Codex  Name = "codex"
	Claude Name = "claude"
)

// Known reports whether this release has an adapter for the runner.
func (n Name) Known() bool { return n == Codex || n == Claude }

// Names lists the launchable runners in display order.
func Names() []Name { return []Name{Codex, Claude} }

// Mode is how the agent is driven. Headless means the agent runs to completion
// against a structured output stream. Interactive means it expects a terminal
// and a person.
type Mode string

const (
	Headless    Mode = "headless"
	Interactive Mode = "interactive"
)

// Capabilities is what an installed CLI can actually do, as opposed to what a
// user asked for. The service compares the two and refuses the mismatch, which
// is why every field is stated positively and none defaults to true.
type Capabilities struct {
	Interactive    bool
	Headless       bool
	StructuredLog  bool
	ModelSelection bool
}

// Options is the user's half of a run: what to launch and where. It is
// deliberately small, and it deliberately does not contain the prompt.
//
// The prompt is the one field that routinely carries private detail about the
// work, so it travels separately as a file the supervisor streams on stdin.
// Keeping it out of Options keeps it out of the run record, the process list,
// the journal, the dashboard, and every log line that renders a run.
type Options struct {
	Runner Name `json:"runner"`
	// Name is a human label for the run, so a list of eight agents can be read
	// at a glance. It has no effect on the command.
	Name  string `json:"name,omitempty"`
	Model string `json:"model,omitempty"`
	Team  string `json:"team,omitempty"`
	// Item names the work this run is doing, as an opaque identifier from
	// whatever tracks work here — in practice a keep item id. Interlock never
	// interprets it and never fetches anything with it. It exists so that a
	// finished run can be traced back to what it was for, and so a repository
	// with no work store behaves identically without one.
	Item string `json:"item,omitempty"`
	// WorkingDir is slash-separated and relative to the repository root. It is
	// stored relative so a run record stays meaningful if the checkout moves.
	WorkingDir string `json:"working_dir,omitempty"`
	// Environment names variables to copy from the launching environment. Names
	// only: a NAME=value pair would put a secret in a stored record, and this
	// field is persisted.
	Environment []string `json:"environment,omitempty"`
	// Interactive asks for an agent a person can type into. No adapter in this
	// release offers it, so it is accepted as a request and refused with a
	// capability error rather than being silently downgraded to headless. See
	// Capabilities.
	Interactive bool `json:"interactive,omitempty"`
}

// Mode reports how the options ask for the agent to be driven. Headless is the
// default because it is the mode a detached supervisor can honestly own: there
// is no terminal to hand back to, so an agent that expects one has nobody to
// talk to.
func (o Options) Mode() Mode {
	if o.Interactive {
		return Interactive
	}
	return Headless
}

// Dir is the working directory as a clean repo-relative path, with the empty
// value meaning the repository root.
func (o Options) Dir() string {
	if o.WorkingDir == "" {
		return "."
	}
	return path.Clean(o.WorkingDir)
}

// Label is what a list should call this run.
func (o Options) Label() string {
	if o.Name != "" {
		return o.Name
	}
	return string(o.Runner)
}

// Validate checks everything that can be judged without touching the disk.
// Containment of the working directory needs the repository and is checked by
// ResolveDir instead.
func (o Options) Validate() error {
	if !o.Runner.Known() {
		return fmt.Errorf("%w: unknown runner %q", ErrInvalid, o.Runner)
	}
	if err := label("name", o.Name, MaxNameLen); err != nil {
		return err
	}
	if err := label("model", o.Model, MaxModelLen); err != nil {
		return err
	}
	if err := label("team", o.Team, MaxTeamLen); err != nil {
		return err
	}
	if err := label("item", o.Item, MaxNameLen); err != nil {
		return err
	}
	if err := validateDir(o.WorkingDir); err != nil {
		return err
	}
	return validateEnvironment(o.Environment)
}

// ValidatePrompt checks the text that will be streamed to the agent. An empty
// prompt is refused because every supported runner requires one, and a run that
// starts with nothing to do only produces a confusing failure later.
func ValidatePrompt(prompt string) error {
	switch {
	case strings.TrimSpace(prompt) == "":
		return fmt.Errorf("%w: prompt is required", ErrInvalid)
	case len(prompt) > MaxPromptBytes:
		return fmt.Errorf("%w: prompt must be at most %d bytes", ErrInvalid, MaxPromptBytes)
	case !utf8.ValidString(prompt):
		return fmt.Errorf("%w: prompt must be valid UTF-8", ErrInvalid)
	case strings.ContainsRune(prompt, 0):
		return fmt.Errorf("%w: prompt must not contain a null byte", ErrInvalid)
	}
	return nil
}

// label accepts the conservative character set shared by run names, models and
// teams. It is narrow on purpose: these strings are rendered in HTML, written
// into JSON, and printed in terminal tables, and the set below is safe in all
// three without any escaping decision being load-bearing.
func label(field, value string, max int) error {
	if value == "" {
		return nil
	}
	if len(value) > max {
		return fmt.Errorf("%w: %s must be at most %d characters", ErrInvalid, field, max)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_.:", r):
		default:
			return fmt.Errorf(
				"%w: %s may contain only letters, digits, and -_.:", ErrInvalid, field)
		}
	}
	return nil
}

func validateDir(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > MaxWorkingDirLen {
		return fmt.Errorf("%w: working directory path is too long", ErrInvalid)
	}
	if strings.ContainsAny(dir, "\x00\\") {
		return fmt.Errorf("%w: working directory must be a slash-separated path", ErrInvalid)
	}
	if path.IsAbs(dir) {
		return fmt.Errorf("%w: working directory must be relative to the repository", ErrInvalid)
	}
	clean := path.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: working directory must stay inside the repository", ErrInvalid)
	}
	return nil
}

func validateEnvironment(names []string) error {
	if len(names) > MaxEnvironment {
		return fmt.Errorf("%w: at most %d environment names may be passed",
			ErrInvalid, MaxEnvironment)
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if err := validateEnvName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("%w: environment name %s was given twice", ErrInvalid, name)
		}
		seen[name] = true
	}
	return nil
}

// validateEnvName accepts the portable shell variable name, and only that. The
// leading digit and the lowercase name are excluded because they are the shapes
// that tend to arrive from a form field rather than from a real environment.
func validateEnvName(name string) error {
	if name == "" || len(name) > MaxEnvironmentLen {
		return fmt.Errorf("%w: environment name must be 1 to %d characters",
			ErrInvalid, MaxEnvironmentLen)
	}
	if strings.Contains(name, "=") {
		return fmt.Errorf(
			"%w: pass environment names only; values are read from the launching environment",
			ErrInvalid)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("%w: environment name %q must match [A-Z_][A-Z0-9_]*",
				ErrInvalid, name)
		}
	}
	return nil
}
