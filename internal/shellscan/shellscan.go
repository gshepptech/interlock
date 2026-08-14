// Package shellscan finds the paths a shell command is likely to write.
//
// Territory and leases are enforced at the tool that edits files, and an agent
// can sidestep both by shelling out to sed. Leaving that open means the tool
// makes a promise it does not keep, which is worse than making no promise.
//
// This cannot be exact. Deciding what a shell command writes is undecidable in
// general: the command may build a path at runtime, read it from a variable, or
// pipe it through xargs. So the design goal is not precision, it is a useful
// answer with an honest confidence attached, letting the caller choose how
// paranoid to be.
package shellscan

import (
	"path/filepath"
	"strings"
)

// Confidence describes how sure the scan is about what a command writes.
type Confidence int

const (
	// None means no write was recognised: the command only reads.
	None Confidence = iota
	// Likely means a writing command was found with concrete paths.
	Likely
	// Opaque means something writes but the target cannot be pinned down: a
	// variable, a subshell, xargs, or an interpreter that could write anything.
	// The caller knows a write is happening and does not know where.
	Opaque
)

func (c Confidence) String() string {
	switch c {
	case None:
		return "none"
	case Likely:
		return "likely"
	case Opaque:
		return "opaque"
	}
	return "unknown"
}

// Result is what a scan found. Paths are cleaned and relative to the command's
// starting directory, with any cd along the way already applied.
type Result struct {
	Confidence Confidence
	Paths      []string
	// Reason names what triggered the finding, so a blocked agent can be told
	// why rather than just refused.
	Reason string
}

// alwaysWriters modify the paths handed to them, with no flag needed.
var alwaysWriters = map[string]bool{
	"tee": true, "cp": true, "mv": true, "rm": true, "touch": true,
	"truncate": true, "install": true, "chmod": true, "chown": true,
	"ln": true, "dd": true, "patch": true, "shred": true, "mkdir": true,
	// Formatters that rewrite in place by default.
	"black": true, "rustfmt": true, "isort": true,
}

// inPlaceWriters only write when told to. Counting sed as a writer regardless
// would flag `sed -n 1,10p file`, which reads, and a tool that blocks reads gets
// switched off.
var inPlaceWriters = map[string]bool{
	"sed": true, "perl": true, "gofmt": true, "goimports": true,
	"prettier": true, "ruff": true, "eslint": true, "clang-format": true,
}

var inPlaceFlags = map[string]bool{
	"-i": true, "--in-place": true, "-w": true, "--write": true,
	"--fix": true, "-fix": true,
}

// interpreters can write anywhere, and nothing on the command line says where.
// Reporting Opaque is the honest answer.
var interpreters = map[string]bool{
	"python": true, "python3": true, "ruby": true, "node": true,
	"sh": true, "bash": true, "zsh": true, "eval": true, "awk": true,
}

// wrappers stand between the shell and the real command.
var wrappers = map[string]bool{
	"xargs": true, "env": true, "sudo": true, "time": true,
	"nohup": true, "nice": true, "command": true,
}

// Scan examines a shell command. Filtering the results to a repo is territory's
// job, since it is the thing that knows the root.
func Scan(command string) Result {
	if strings.TrimSpace(command) == "" {
		return Result{Confidence: None}
	}

	s := &scan{seen: map[string]bool{}}
	for _, segment := range splitCommand(command) {
		s.segment(segment)
	}

	switch {
	case s.opaque:
		return Result{Confidence: Opaque, Paths: s.paths, Reason: s.reason}
	case len(s.paths) > 0:
		return Result{Confidence: Likely, Paths: s.paths, Reason: s.reason}
	default:
		return Result{Confidence: None}
	}
}

type scan struct {
	// dir tracks cd within the command, so a path after `cd src` resolves the
	// same way the shell would resolve it.
	dir    string
	paths  []string
	seen   map[string]bool
	reason string
	opaque bool
}

func (s *scan) markOpaque(reason string) {
	s.opaque = true
	if s.reason == "" || !strings.Contains(s.reason, "unresolvable") {
		s.reason = reason
	}
}

func (s *scan) add(raw string) {
	p := strings.Trim(raw, `"'`)
	if p == "" || strings.HasPrefix(p, "-") {
		return
	}
	resolved := filepath.Clean(filepath.Join(s.dir, p))
	if s.seen[resolved] {
		return
	}
	s.seen[resolved] = true
	s.paths = append(s.paths, resolved)
}

func (s *scan) segment(segment string) {
	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return
	}

	// A redirection writes its target whatever the command in front of it is.
	if target, ok := redirectTarget(segment); ok {
		if isOpaque(target) {
			s.markOpaque("redirection to an unresolvable target")
		} else {
			s.add(target)
			if s.reason == "" {
				s.reason = "shell redirection"
			}
		}
	}

	// Strip wrappers to reach the command that actually does the work. A
	// wrapper is also a reason to distrust the arguments: xargs feeds them in
	// from a pipe, so nothing on this line names the target.
	wrapped := false
	for len(fields) > 0 && wrappers[baseName(fields[0])] {
		if baseName(fields[0]) == "xargs" {
			wrapped = true
		}
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return
	}

	cmd := baseName(fields[0])
	args := fields[1:]

	if cmd == "cd" {
		s.changeDir(args)
		return
	}

	switch {
	case interpreters[cmd]:
		s.markOpaque(cmd + " can write anywhere and the target is not on the command line")
		return
	case alwaysWriters[cmd]:
	case inPlaceWriters[cmd]:
		if !hasInPlaceFlag(args) {
			return // reading, not writing
		}
	default:
		return
	}

	if wrapped {
		s.markOpaque(cmd + " fed by xargs, so the targets are not on the command line")
		return
	}
	if isOpaque(segment) {
		s.markOpaque(cmd + " with an unresolvable target")
		return
	}

	s.reason = cmd
	for _, f := range args {
		if looksLikePath(f) {
			s.add(f)
		}
	}
}

// changeDir follows a literal cd so later relative paths resolve correctly. A cd
// to somewhere unresolvable makes every path after it meaningless, which is
// exactly what Opaque is for.
func (s *scan) changeDir(args []string) {
	var target string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			target = strings.Trim(a, `"'`)
			break
		}
	}
	if target == "" {
		return
	}
	if isOpaque(target) {
		s.markOpaque("cd to an unresolvable directory")
		return
	}
	if filepath.IsAbs(target) {
		s.dir = filepath.Clean(target)
		return
	}
	s.dir = filepath.Clean(filepath.Join(s.dir, target))
}

func hasInPlaceFlag(args []string) bool {
	for _, a := range args {
		a = strings.Trim(a, `"'`)
		if inPlaceFlags[a] {
			return true
		}
		// sed -i'' and sed -i.bak attach the suffix to the flag.
		if strings.HasPrefix(a, "-i") && !strings.HasPrefix(a, "-in") {
			return true
		}
		// `ruff format` and `prettier --write` both write; the subcommand is
		// the tell for the former.
		if a == "format" {
			return true
		}
	}
	return false
}

// splitCommand breaks on the operators that separate one command from the next,
// so `ls && sed -i x f` is two commands rather than one starting with ls.
func splitCommand(command string) []string {
	replacer := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "\n", "\n")
	return strings.Split(replacer.Replace(command), "\n")
}

// redirectTarget returns the file a segment redirects into. Both > and >> write;
// a bare < only reads.
func redirectTarget(segment string) (string, bool) {
	idx := strings.LastIndex(segment, ">")
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(segment[idx+1:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	target := fields[0]

	// `2>&1` duplicates a file descriptor. There is no file, so there is
	// nothing to hold -- and a lease on a path named "&1" is pure noise in
	// every view that lists what an agent is working on.
	if strings.HasPrefix(target, "&") {
		return "", false
	}
	// Writing to a device is not editing anything two agents can collide
	// over. `>/dev/null` appears in most commands an agent runs; leasing it
	// would mean every agent holds the same "file" forever.
	if isDevice(target) {
		return "", false
	}
	return target, true
}

// isDevice reports whether path is a character device a shell redirects to
// rather than a file in the repository.
func isDevice(path string) bool {
	return path == "/dev/null" || path == "/dev/tty" ||
		path == "/dev/stdout" || path == "/dev/stderr" ||
		strings.HasPrefix(path, "/dev/fd/")
}

// isOpaque reports whether a string contains anything the shell would expand.
func isOpaque(s string) bool {
	for _, m := range []string{"$(", "`", "${", "xargs", "eval"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	// A bare $VAR or $1, which the literal markers above miss.
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '$' {
			continue
		}
		c := s[i+1]
		if c == '_' || c == '@' || c == '*' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

// looksLikePath is a loose heuristic on purpose. A false positive costs a
// territory check against a path that matches nothing. A false negative costs an
// unnoticed write, which is the failure that matters.
func looksLikePath(f string) bool {
	f = strings.Trim(f, `"'`)
	if f == "" || strings.HasPrefix(f, "-") {
		return false
	}
	if strings.Contains(f, "/") {
		return true
	}
	// A dotted name with a short extension reads as a filename. Requiring the
	// extension keeps sed scripts like s/a/b/ out, and those contain slashes.
	if dot := strings.LastIndex(f, "."); dot > 0 && dot < len(f)-1 {
		ext := f[dot+1:]
		return len(ext) <= 5 && !strings.ContainsAny(ext, "/\\ ")
	}
	return false
}

func baseName(cmd string) string {
	return filepath.Base(strings.Trim(cmd, `"'`))
}
