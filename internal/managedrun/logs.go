package managedrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gshepptech/interlock/internal/runner"
)

// MaxLogBytes is how much of one stream is kept in the active file before it
// rotates. One rotated file is kept behind it, so a run costs at most twice
// this per stream.
//
// An agent can produce output indefinitely, and the failure this bound prevents
// is not disk exhaustion in the abstract: it is a coordination tool filling a
// developer's disk while they are not watching, which would be a far worse
// outcome than losing the first half of a chatty agent's transcript.
const MaxLogBytes = 4 << 20

// MaxReadBytes bounds a single read. The dashboard asks for far less; this is
// the ceiling that stops any caller from pulling four megabytes into a page.
const MaxReadBytes = 1 << 20

// LogWriter appends one stream of an agent's output.
//
// There is exactly one of these per stream per run, held by the supervisor for
// the life of the child, which is what makes the whole log design lock-free:
// rotation only ever happens on the writer's own goroutine, and readers deal
// with rotation by noticing it rather than by preventing it.
type LogWriter struct {
	path    string
	rotated string
	file    *os.File
	size    int64
	max     int64
}

// OpenLog opens a run's log for appending, continuing an existing file.
func (c *Store) OpenLog(id string, stream Stream) (*LogWriter, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	if !stream.Known() {
		return nil, fmt.Errorf("%w: unknown stream %q", ErrInvalid, stream)
	}
	path := c.LogPath(id, stream)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s log: %w", stream, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect %s log: %w", stream, err)
	}
	return &LogWriter{
		path: path, rotated: c.RotatedLogPath(id, stream),
		file: file, size: info.Size(), max: MaxLogBytes,
	}, nil
}

// Write appends output, rotating first if this write would overflow the active
// file. A write larger than the whole budget is still written rather than
// dropped: losing a line because it was long is harder to explain than a file
// that briefly exceeds its bound.
func (w *LogWriter) Write(p []byte) (int, error) {
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close releases the file. The log stays where it is; a finished run keeps its
// output until the record is pruned.
func (w *LogWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *LogWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	if err := os.Rename(w.path, w.rotated); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log after rotation: %w", err)
	}
	w.file, w.size = file, 0
	return nil
}

// Cursor is a reader's position in a stream.
//
// It holds the identity of the file the offset belongs to, not just the offset,
// because a log rotates underneath a follower. An offset alone would silently
// start reading the middle of a new file as though it were the end of the old
// one.
type Cursor struct {
	identity os.FileInfo
	offset   int64
}

// Started reports whether the cursor has a position yet.
func (c Cursor) Started() bool { return c.identity != nil }

// LogChunk is a bounded piece of a stream.
type LogChunk struct {
	// Bytes is raw agent output. It is sanitised where it is displayed rather
	// than here, because the terminal and the browser need different treatment
	// and neither should be applied twice.
	Bytes []byte
	// Truncated marks output that was skipped to respect the read bound.
	Truncated bool
}

// Text renders a chunk for a person: escape sequences removed, one line at a
// time, safe to put in a terminal or, after the template's escaping, a page.
func (c LogChunk) Text() string {
	if len(c.Bytes) == 0 {
		return ""
	}
	lines := strings.Split(string(c.Bytes), "\n")
	for i, line := range lines {
		lines[i] = runner.SafeText(line, 0)
	}
	return strings.Join(lines, "\n")
}

// Tail reads the end of a stream, spanning the rotated file when the active one
// is shorter than the request.
func (c *Store) Tail(id string, stream Stream, limit int64) (LogChunk, Cursor, error) {
	if !ValidID(id) {
		return LogChunk{}, Cursor{}, ErrNotFound
	}
	if !stream.Known() {
		return LogChunk{}, Cursor{}, fmt.Errorf("%w: unknown stream %q", ErrInvalid, stream)
	}
	limit = clampLimit(limit)

	active := c.LogPath(id, stream)
	info, err := os.Stat(active)
	if errors.Is(err, os.ErrNotExist) {
		return LogChunk{}, Cursor{}, nil
	}
	if err != nil {
		return LogChunk{}, Cursor{}, fmt.Errorf("inspect %s log: %w", stream, err)
	}

	chunk := LogChunk{}
	from := max(info.Size()-limit, 0)
	chunk.Truncated = from > 0
	raw, err := readRange(active, from, limit)
	if err != nil {
		return LogChunk{}, Cursor{}, err
	}
	// Only reach back into the rotated file when the active one did not fill the
	// request, so a busy run never pays for a second read.
	if remaining := limit - int64(len(raw)); remaining > 0 {
		older, olderTruncated, err := tailRotated(c.RotatedLogPath(id, stream), remaining)
		if err != nil {
			return LogChunk{}, Cursor{}, err
		}
		chunk.Truncated = chunk.Truncated || olderTruncated
		raw = append(older, raw...)
	}
	chunk.Bytes = raw
	return chunk, Cursor{identity: info, offset: info.Size()}, nil
}

// Follow reads what has arrived since the cursor.
//
// Three things can have happened between calls: nothing, more output, or a
// rotation. The rotation case is why the cursor carries file identity: the
// remainder of the file the reader was in is read first, then the new file from
// its start, so a follower sees every byte exactly once across the boundary.
func (c *Store) Follow(id string, stream Stream, cursor Cursor, limit int64) (LogChunk, Cursor, error) {
	if !cursor.Started() {
		return c.Tail(id, stream, limit)
	}
	if !ValidID(id) {
		return LogChunk{}, cursor, ErrNotFound
	}
	if !stream.Known() {
		return LogChunk{}, cursor, fmt.Errorf("%w: unknown stream %q", ErrInvalid, stream)
	}
	limit = clampLimit(limit)

	active := c.LogPath(id, stream)
	info, err := os.Stat(active)
	if errors.Is(err, os.ErrNotExist) {
		return LogChunk{}, cursor, nil
	}
	if err != nil {
		return LogChunk{}, cursor, fmt.Errorf("inspect %s log: %w", stream, err)
	}

	if os.SameFile(cursor.identity, info) {
		from := cursor.offset
		truncated := false
		// A file that shrank was replaced by something other than rotation. The
		// honest response is to say output was lost and resynchronise.
		if info.Size() < from {
			from, truncated = 0, true
		}
		raw, err := readRange(active, from, limit)
		if err != nil {
			return LogChunk{}, cursor, err
		}
		read := from + int64(len(raw))
		return LogChunk{Bytes: raw, Truncated: truncated || read < info.Size()},
			Cursor{identity: info, offset: read}, nil
	}

	// Rotation. Finish the file the cursor was in, then start the new one.
	var raw []byte
	truncated := false
	rotated := c.RotatedLogPath(id, stream)
	if rotatedInfo, err := os.Stat(rotated); err == nil && os.SameFile(cursor.identity, rotatedInfo) {
		tail, err := readRange(rotated, cursor.offset, limit)
		if err != nil {
			return LogChunk{}, cursor, err
		}
		raw = tail
		truncated = cursor.offset+int64(len(tail)) < rotatedInfo.Size()
	} else {
		// The cursor's file is gone entirely, so some output was lost.
		truncated = true
	}
	fresh, err := readRange(active, 0, limit-int64(len(raw)))
	if err != nil {
		return LogChunk{}, cursor, err
	}
	raw = append(raw, fresh...)
	return LogChunk{Bytes: raw, Truncated: truncated || int64(len(fresh)) < info.Size()},
		Cursor{identity: info, offset: int64(len(fresh))}, nil
}

func tailRotated(path string, limit int64) ([]byte, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect rotated log: %w", err)
	}
	from := max(info.Size()-limit, 0)
	raw, err := readRange(path, from, limit)
	return raw, from > 0, err
}

// readRange reads at most limit bytes starting at from. It opens, reads and
// closes within the call, so no descriptor is ever held across a rotation.
func readRange(path string, from, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = file.Close() }()
	if from > 0 {
		if _, err := file.Seek(from, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek log: %w", err)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return raw, nil
}

func clampLimit(limit int64) int64 {
	if limit <= 0 {
		return 64 * 1024
	}
	return min(limit, MaxReadBytes)
}

// Redact masks values that look like credentials before output is displayed.
//
// This is defence in depth and not a promise. Nothing can guarantee that
// arbitrary model output contains no secrets, and pretending otherwise would
// encourage exactly the wrong habit. What it does do is stop the two cases that
// actually happen: an environment value the run was given being echoed back,
// and the common shape of an assignment to something named like a credential.
func Redact(text string, values []string) string {
	const mask = "[redacted]"
	for _, value := range values {
		// A short value would match half the output; a credential is not short.
		if len(value) >= 8 {
			text = strings.ReplaceAll(text, value, mask)
		}
	}
	var out strings.Builder
	out.Grow(len(text))
	for line := range strings.SplitSeq(text, "\n") {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(redactLine(line, mask))
	}
	return out.String()
}

// credentialNames are the words that, immediately before an assignment, mean
// the thing after it should not be displayed.
var credentialNames = []string{"token", "secret", "password", "passwd", "apikey",
	"api_key", "access_key", "private_key", "credential", "authorization"}

func redactLine(line, mask string) string {
	lower := strings.ToLower(line)
	for _, name := range credentialNames {
		index := strings.Index(lower, name)
		if index < 0 {
			continue
		}
		rest := line[index+len(name):]
		// Step over a closing quote and any spacing, so a JSON key is treated
		// the same as a shell assignment.
		gap := 0
		for gap < len(rest) && strings.ContainsRune(" \t\"'", rune(rest[gap])) {
			gap++
		}
		if gap >= len(rest) || (rest[gap] != '=' && rest[gap] != ':') {
			continue
		}
		if value := strings.TrimLeft(rest[gap+1:], " \t\"'"); len(value) < 4 {
			continue
		}
		return line[:index+len(name)+gap+1] + mask
	}
	return line
}
