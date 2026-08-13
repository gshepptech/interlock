package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrUnsupported reports an option this release cannot honour for the chosen
	// runner. Like ErrInvalid it is the caller's to correct, and unlike a launch
	// failure it is never worth retrying unchanged.
	ErrUnsupported = errors.New("unsupported by this runner")
	// ErrNotInstalled reports that the agent's own CLI could not be found.
	ErrNotInstalled = errors.New("agent command not found")
)

// Stream names one of an agent's two output channels.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// Known reports whether the stream is one of the two Interlock captures.
func (s Stream) Known() bool { return s == Stdout || s == Stderr }

// Launch is an exact command, ready for a supervisor to start.
//
// Args excludes the program name, Env is the complete environment rather than
// an addition to the parent's, and neither is ever assembled into a string. A
// supervisor can start a Launch without knowing which agent it belongs to,
// which is the point: process handling has no vendor-specific branches.
type Launch struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	Mode Mode
}

// EventKind is what one decoded line of agent output meant.
type EventKind string

const (
	// Output is a line with no structure to read: log text, a banner, a stack
	// trace. It is still displayed; it just tells Interlock nothing.
	Output EventKind = "output"
	// Activity is a structured line proving the agent is working.
	Activity EventKind = "activity"
	// Result is the agent reporting that it finished its turn.
	Result EventKind = "result"
	// Failure is the agent reporting an error of its own.
	Failure EventKind = "failure"
)

// Event is the safe, bounded reading of one output line. It carries no raw
// agent text beyond a trimmed summary, because everything above this becomes a
// status field or a rendered page.
type Event struct {
	Kind    EventKind
	Summary string
}

// MaxEventSummary bounds a decoded summary. One line of a status display.
const MaxEventSummary = 256

// maxDecodedLine is the longest line worth parsing as structure. A megabyte of
// JSON on one line is not a status update; it is a payload, and it is displayed
// as output rather than parsed.
const maxDecodedLine = 64 * 1024

// Adapter is everything vendor-specific about one agent.
type Adapter interface {
	Name() Name
	Capabilities() Capabilities
	// Build turns options into an exact command, or refuses. It resolves the
	// executable and the working directory, so a Launch it returns is one that
	// can actually be started.
	Build(Options, Environment) (Launch, error)
	// Decode reads one complete output line. It never fails: a line it cannot
	// interpret is output, not an error, because a malformed status line must
	// not be able to stop a run.
	Decode(Stream, []byte) Event
}

// For returns the adapter for a runner.
func For(name Name) (Adapter, error) {
	switch name {
	case Codex:
		return codex{}, nil
	case Claude:
		return claude{}, nil
	}
	return nil, fmt.Errorf("%w: unknown runner %q", ErrInvalid, name)
}

// Environment is everything Build needs from outside the options themselves.
type Environment struct {
	// Root is the repository. The working directory is resolved against it and
	// required to stay inside it.
	Root string
	// Lookup reads the launching environment. Nil means the current process.
	Lookup func(string) (string, bool)
	// Link is Interlock's own NAME=value variables, appended last. They are how
	// a hook fired by the child knows which run it belongs to.
	Link []string
}

func (e Environment) lookup(name string) (string, bool) {
	if e.Lookup != nil {
		return e.Lookup(name)
	}
	return os.LookupEnv(name)
}

// baseline is what every managed agent gets whether it asked or not.
//
// The list is short deliberately. A child that inherits the whole environment
// inherits every credential the launching shell happened to hold, and an agent
// is precisely the kind of process that will read them and put them somewhere.
// What remains is what a program needs to find its home directory, its
// executables, somewhere to write temporary files, and how to format text.
var baseline = []string{
	"HOME", "PATH", "SHELL", "USER", "LOGNAME",
	"TMPDIR", "TEMP", "TMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
}

// resolveEnv assembles the child's complete environment: the baseline, then the
// names the user explicitly allowed, then Interlock's own linkage last so
// nothing can shadow it.
func resolveEnv(o Options, env Environment) []string {
	seen := make(map[string]bool, len(baseline)+len(o.Environment))
	out := make([]string, 0, len(baseline)+len(o.Environment)+len(env.Link))
	for _, name := range append(append([]string{}, baseline...), o.Environment...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if value, ok := env.lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return append(out, env.Link...)
}

// build is the shared half of every adapter's Build: the checks that do not
// depend on which agent is being launched.
func build(o Options, env Environment, caps Capabilities, executable string,
	search []string) (string, string, error) {
	if err := o.Validate(); err != nil {
		return "", "", err
	}
	if o.Runner != Name(executable) {
		return "", "", fmt.Errorf("%w: options are for %s", ErrInvalid, o.Runner)
	}
	if o.Interactive && !caps.Interactive {
		return "", "", fmt.Errorf(
			"%w: %s cannot be run interactively by Interlock in this release; "+
				"omit --interactive to run it headless", ErrUnsupported, o.Runner)
	}
	if !o.Interactive && !caps.Headless {
		return "", "", fmt.Errorf("%w: %s has no headless mode", ErrUnsupported, o.Runner)
	}
	if o.Model != "" && !caps.ModelSelection {
		return "", "", fmt.Errorf("%w: %s does not accept a model", ErrUnsupported, o.Runner)
	}
	dir, err := ResolveDir(env.Root, o.WorkingDir)
	if err != nil {
		return "", "", err
	}
	path, err := findExecutable(executable, env, search)
	if err != nil {
		return "", "", err
	}
	return path, dir, nil
}

// findExecutable locates an agent's CLI.
//
// The override is checked first and, if it is set and wrong, is an error rather
// than a fallback: someone who pointed Interlock at a specific binary wants to
// know it is missing, not to silently get a different one. The extra search
// paths exist because Codex has moved between application bundles, and a single
// hard-coded path would have broken silently the week it moved.
func findExecutable(name string, env Environment, search []string) (string, error) {
	override := "INTERLOCK_" + strings.ToUpper(name) + "_BIN"
	if path, ok := env.lookup(override); ok && path != "" {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
		return "", fmt.Errorf("%w: %s is set to %s, which is not a program",
			ErrNotInstalled, override, path)
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, candidate := range search {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	where := "on PATH"
	if len(search) > 0 {
		where += " and in " + strings.Join(search, ", ")
	}
	return "", fmt.Errorf("%w: cannot find %s %s; set %s to its location",
		ErrNotInstalled, name, where, override)
}

// decodeLine is the shared decoder. Structure is a bonus: a line that is not
// JSON, or is too large to be a status message, is displayed as output. The
// alternative, treating an unparseable line as a failure, would let an agent
// that prints a banner look like an agent that crashed.
func decodeLine(raw []byte, classify func(map[string]any) (EventKind, string)) Event {
	text := SafeText(string(raw), MaxEventSummary)
	if len(raw) > maxDecodedLine {
		return Event{Kind: Output, Summary: text}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Event{Kind: Output, Summary: text}
	}
	kind, summary := classify(payload)
	if summary == "" {
		summary = text
	}
	return Event{Kind: kind, Summary: SafeText(summary, MaxEventSummary)}
}

func stringField(payload map[string]any, name string) string {
	value, _ := payload[name].(string)
	return value
}

func boolField(payload map[string]any, name string) bool {
	value, _ := payload[name].(bool)
	return value
}

// SafeText makes agent output fit to store and to render.
//
// Two hazards, one function. Control characters reach a terminal that will
// interpret them, and escape sequences are how output rewrites a screen it was
// only supposed to appear on. Both are removed here rather than at each of the
// several places that display a run, because a single sanitiser is a thing that
// can be tested; a convention is not.
func SafeText(text string, max int) string {
	if text == "" {
		return ""
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	text = StripANSI(text)
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if max > 0 && len(text) > max {
		text = strings.ToValidUTF8(text[:max], "")
	}
	return text
}

// StripANSI removes terminal escape sequences.
//
// It handles the two shapes agent output actually contains: a control sequence
// introducer, which ends at its final byte, and an operating system command,
// which runs until a bell or a string terminator. Anything else is a
// two-character escape and both characters are dropped, so no sequence survives
// even when this does not know what it was.
func StripANSI(text string) string {
	if !strings.ContainsRune(text, 0x1b) {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] != 0x1b {
			out.WriteRune(runes[i])
			continue
		}
		i++
		if i >= len(runes) {
			break
		}
		switch runes[i] {
		case '[':
			// CSI: parameter and intermediate bytes, then one final byte.
			for i++; i < len(runes) && runes[i] >= 0x20 && runes[i] <= 0x3f; i++ {
			}
		case ']':
			// OSC: runs to a bell or a string terminator.
			for i++; i < len(runes); i++ {
				if runes[i] == 0x07 {
					break
				}
				if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '\\' {
					i++
					break
				}
			}
		}
	}
	return out.String()
}
