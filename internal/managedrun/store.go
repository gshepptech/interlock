package managedrun

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/runner"
	"github.com/gshepptech/interlock/internal/state"
)

// DefaultCapacity bounds how many run records a repository keeps. A run costs a
// directory, two log files and a prompt, so the cap is about disk and about
// keeping a listing readable rather than about correctness. Terminal runs are
// pruned oldest first to make room; live runs are never pruned, which is why a
// repository that somehow holds two hundred live agents is told to stop one
// rather than quietly having one deleted.
const DefaultCapacity = 200

var (
	// ErrNotFound reports a run id with no record behind it.
	ErrNotFound = errors.New("run not found")
	// ErrCapacity reports that no record can be added without removing a live run.
	ErrCapacity = errors.New("run store is full")
	// ErrConflict reports a move the state machine does not allow.
	ErrConflict = errors.New("run state conflict")
	// ErrGeneration reports that the caller is talking about a different
	// incarnation of this run than the one on disk.
	ErrGeneration = errors.New("run generation mismatch")
	// ErrInvalid reports a malformed argument, as opposed to invalid launch
	// options, which are reported with runner.ErrInvalid.
	ErrInvalid = errors.New("invalid run request")
)

// Stream names one of an agent's two output channels. It is the runner's type
// because the decoder needs it too, and one enum is easier to trust than two.
type Stream = runner.Stream

const (
	Stdout = runner.Stdout
	Stderr = runner.Stderr
)

// Store is the run directory for one repository.
type Store struct {
	root     string
	dir      string
	capacity int
	now      func() time.Time
	newID    func(prefix string, bytes int) (string, error)
}

// Open prepares the run store under root. Nothing is created until a run is,
// so a repository that never launches an agent never grows the directory.
func Open(root string) *Store {
	return &Store{
		root:     root,
		dir:      filepath.Join(root, ".interlock", "runs"),
		capacity: DefaultCapacity,
		now:      time.Now,
		newID:    randomID,
	}
}

// Create is a request for a new run.
type Create struct {
	Options runner.Options
	Prompt  string
	Creator Creator
	// RetryOf names the run this was created from, when the request came from a
	// retry. It is recorded rather than acted on: nothing about the earlier run
	// is reused, because reusing a record would lose the history of the failure.
	RetryOf string
}

// Create validates the request, writes the immutable half of the run, and
// leaves it queued for a supervisor to claim.
//
// The order matters. Everything that can be refused is refused before any file
// exists, so a rejected launch leaves nothing behind. After that the record is
// durable: if the process that called this dies before spawning a supervisor,
// the run is a queued record that reconciliation will mark lost, not a silent
// gap.
func (c *Store) Create(input Create) (Record, error) {
	// Check rather than Validate: a request the chosen agent cannot honour is
	// refused here, before any state exists, rather than becoming a run that
	// fails the moment a supervisor picks it up.
	if err := runner.Check(input.Options); err != nil {
		return Record{}, err
	}
	if err := runner.ValidatePrompt(input.Prompt); err != nil {
		return Record{}, err
	}
	if !input.Creator.known() {
		return Record{}, fmt.Errorf("%w: unknown run creator %q", ErrInvalid, input.Creator)
	}
	if input.RetryOf != "" && !ValidID(input.RetryOf) {
		return Record{}, fmt.Errorf("%w: invalid retry source id", ErrInvalid)
	}
	if _, err := runner.ResolveDir(c.root, input.Options.WorkingDir); err != nil {
		return Record{}, err
	}

	unlock, err := c.lockStore()
	if err != nil {
		return Record{}, err
	}
	defer unlock()

	if err := c.makeRoomLocked(); err != nil {
		return Record{}, err
	}

	id, err := c.newID("run-", 12)
	if err != nil {
		return Record{}, err
	}
	generation, err := c.newID("", 16)
	if err != nil {
		return Record{}, err
	}
	dir := c.Dir(id)
	// A pre-existing directory means the identifier collided, which for 96 bits
	// of randomness means something is wrong rather than unlucky. Refusing is
	// better than adopting a directory whose contents are unknown.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Record{}, fmt.Errorf("run directory %s already exists", id)
		}
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(c.dir, 0o755); err != nil {
				return Record{}, fmt.Errorf("create run store: %w", err)
			}
			err = os.Mkdir(dir, 0o700)
		}
		if err != nil {
			return Record{}, fmt.Errorf("create run directory: %w", err)
		}
	}
	// Anything that fails from here leaves a half-built run, so it is removed
	// rather than left for a reader to puzzle over.
	cleanup := func(cause error) (Record, error) {
		_ = os.RemoveAll(dir)
		return Record{}, cause
	}

	spec := Spec{
		Version: state.CurrentVersion, ID: id, Generation: generation,
		Options: input.Options, CreatedAt: c.now(), Creator: input.Creator,
		RetryOf: input.RetryOf,
	}
	if err := writeExclusiveJSON(c.SpecPath(id), spec); err != nil {
		return cleanup(err)
	}
	if err := writeExclusive(c.PromptPath(id), []byte(input.Prompt)); err != nil {
		return cleanup(err)
	}
	token, err := c.newID("", 32)
	if err != nil {
		return cleanup(err)
	}
	if err := writeExclusive(c.TokenPath(id), []byte(token)); err != nil {
		return cleanup(err)
	}

	status := Status{
		Version: state.CurrentVersion, ID: id, Generation: generation,
		State: Queued, Sequence: 1, UpdatedAt: c.now(),
	}
	if err := c.writeStatus(id, status); err != nil {
		return cleanup(err)
	}
	return Record{Spec: spec, Status: status}, nil
}

// Load reads one run.
func (c *Store) Load(id string) (Record, error) {
	if !ValidID(id) {
		return Record{}, ErrNotFound
	}
	spec, err := c.readSpec(id)
	if err != nil {
		return Record{}, err
	}
	status, err := c.readStatus(id)
	if err != nil {
		return Record{}, err
	}
	if status.ID != spec.ID || status.Generation != spec.Generation {
		return Record{}, fmt.Errorf("run %s has a status from a different record", id)
	}
	return Record{Spec: spec, Status: status}, nil
}

// List returns every readable run, newest first.
//
// A record this binary cannot read is skipped rather than failing the listing,
// for the same reason the journal skips a bad line: one unreadable run must not
// blank the page. Inspect is the read path that reports them.
func (c *Store) List() ([]Record, error) {
	ids, err := c.ids()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(ids))
	for _, id := range ids {
		record, err := c.Load(id)
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b Record) int {
		return b.Spec.CreatedAt.Compare(a.Spec.CreatedAt)
	})
	return records, nil
}

// Transition moves a run to a new state under its own lock.
//
// It reports whether anything changed. Asking for the state a run already holds
// is the idempotent case, not an error: a second pause request from an impatient
// user, or a retried control message, must not turn into a failure. mutate runs
// after the state and its timestamps are set, so it can record the exit code or
// signal that explains the move.
func (c *Store) Transition(id, generation string, to State, mutate func(*Status)) (Status, bool, error) {
	return c.update(id, generation, to, mutate)
}

// Update changes status fields without moving the run. The state and the
// transition sequence are preserved, so a refreshed output timestamp can never
// be mistaken for a lifecycle event.
func (c *Store) Update(id, generation string, mutate func(*Status)) (Status, bool, error) {
	return c.update(id, generation, "", mutate)
}

func (c *Store) update(id, generation string, to State, mutate func(*Status)) (Status, bool, error) {
	if !ValidID(id) {
		return Status{}, false, ErrNotFound
	}
	if generation == "" {
		return Status{}, false, fmt.Errorf("%w: a generation is required to change a run", ErrInvalid)
	}
	if to != "" && !to.Known() {
		return Status{}, false, fmt.Errorf("%w: unknown target state %q", ErrInvalid, to)
	}
	unlock, err := c.lockRun(id)
	if err != nil {
		return Status{}, false, err
	}
	defer unlock()

	status, err := c.readStatus(id)
	if err != nil {
		return Status{}, false, err
	}
	if status.Generation != generation {
		return status, false, fmt.Errorf("%w: run %s is generation %s", ErrGeneration, id, status.Generation)
	}

	switch {
	case to == "" && status.State.Terminal():
		return status, false, fmt.Errorf("%w: run %s has finished as %s",
			ErrConflict, id, status.State)
	case to == "":
		// A field update, not a move.
	case to == status.State:
		return status, false, nil
	case !CanTransition(status.State, to):
		return status, false, fmt.Errorf("%w: run %s cannot move from %s to %s",
			ErrConflict, id, status.State, to)
	}

	updated := status
	now := c.now()
	updated.UpdatedAt = now
	if to != "" {
		updated.State = to
		updated.Sequence++
		if to == Running && updated.StartedAt.IsZero() {
			updated.StartedAt = now
		}
		if to.Terminal() && updated.EndedAt.IsZero() {
			updated.EndedAt = now
		}
	}
	if mutate != nil {
		mutate(&updated)
		// mutate is trusted with detail, not with identity or lifecycle. Anything
		// it changed that it has no business changing is put back.
		updated.Version, updated.ID, updated.Generation = status.Version, status.ID, status.Generation
		updated.Sequence = status.Sequence
		if to == "" {
			updated.State = status.State
		} else {
			updated.State, updated.Sequence = to, status.Sequence+1
		}
	}
	updated.Summary = summarise(updated.Summary)
	if !updated.Reason.known() {
		updated.Reason = ReasonNone
	}
	if updated.State == status.State && updated.equivalent(status) {
		return status, false, nil
	}
	if err := c.writeStatus(id, updated); err != nil {
		return status, false, err
	}
	return updated, true, nil
}

// equivalent reports whether two statuses differ only in when they were written.
// Without this a supervisor polling for output would rewrite the file every
// second and every reader would see a run that never settles.
func (a Status) equivalent(b Status) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return a == b
}

// Token reads the control token a supervisor requires before acting on a
// request. It is a secret, so the read is hardened the same way the prompt is.
func (c *Store) Token(id string) (string, error) {
	raw, err := readSecure(c.TokenPath(id))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("run %s has no control token", id)
	}
	return token, nil
}

// Prompt reads the text the agent was started with. Only the supervisor needs
// it, and only to stream it to the child on stdin.
func (c *Store) Prompt(id string) ([]byte, error) { return readSecure(c.PromptPath(id)) }

// Inspect reports records this binary cannot read, without changing any of them.
func (c *Store) Inspect() ([]state.Issue, error) {
	ids, err := c.ids()
	if err != nil {
		return nil, err
	}
	var issues []state.Issue
	for _, id := range ids {
		if _, err := c.readSpec(id); err != nil {
			issues = append(issues, state.Issue{Store: "managedrun", Path: c.SpecPath(id), Err: err})
			continue
		}
		if _, err := c.readStatus(id); err != nil {
			issues = append(issues, state.Issue{Store: "managedrun", Path: c.StatusPath(id), Err: err})
		}
	}
	return issues, nil
}

// Paths. Every sensitive file lives inside a run directory whose name is a
// validated identifier, so no caller-supplied text ever reaches a path segment.

// Dir is the directory holding one run's files.
func (c *Store) Dir(id string) string { return filepath.Join(c.dir, id) }

// Root is the directory holding every run.
func (c *Store) Root() string { return c.dir }

func (c *Store) SpecPath(id string) string   { return filepath.Join(c.Dir(id), "spec.json") }
func (c *Store) StatusPath(id string) string { return filepath.Join(c.Dir(id), "status.json") }
func (c *Store) PromptPath(id string) string { return filepath.Join(c.Dir(id), "prompt") }
func (c *Store) TokenPath(id string) string  { return filepath.Join(c.Dir(id), "control.token") }

// SocketPath is where a live supervisor prefers to listen. Its presence proves
// nothing on its own: a socket file outlives the process that made it, which is
// why every request carries the run generation.
func (c *Store) SocketPath(id string) string { return filepath.Join(c.Dir(id), "control.sock") }

// ControlAddrPath records where a supervisor actually listened, and exists only
// when that was not the preferred path. See ControlAddress.
func (c *Store) ControlAddrPath(id string) string {
	return filepath.Join(c.Dir(id), "control.addr")
}

// MaxSocketPath is the practical limit on a unix socket path. The kernel's
// sockaddr_un is 104 bytes on the BSDs and 108 on Linux, and a socket cannot be
// bound at a longer path however sensible that path looks.
const MaxSocketPath = 100

// ControlAddress is where to connect for a run.
//
// A run's socket belongs beside the rest of its files, and usually is there.
// But the address of a unix socket is a fixed-size field, and a repository a
// few directories deep plus a run identifier can exceed it. Rather than fail a
// launch over the length of somebody's checkout path, a supervisor in that
// position binds somewhere short and leaves this note behind saying where.
func (c *Store) ControlAddress(id string) string {
	preferred := c.SocketPath(id)
	raw, err := readSecure(c.ControlAddrPath(id))
	if err != nil {
		return preferred
	}
	address := strings.TrimSpace(string(raw))
	if !validSocketPath(address) {
		return preferred
	}
	return address
}

// FallbackSocketPath is the short address used when the run directory is too
// deep. The name carries the tail of the run id, which is random, so two runs
// never collide and no other user can predict it.
func FallbackSocketPath(id string) string {
	return filepath.Join(os.TempDir(), "interlock-"+id[len(id)-12:]+".sock")
}

func validSocketPath(address string) bool {
	return filepath.IsAbs(address) && len(address) <= MaxSocketPath &&
		strings.HasSuffix(address, ".sock") && !strings.ContainsAny(address, "\n\r\x00")
}

// SupervisorLockPath is held for the lifetime of a supervisor. The kernel drops
// it when that process dies, for any reason, which is what makes "no supervisor"
// a fact rather than a guess.
func (c *Store) SupervisorLockPath(id string) string {
	return filepath.Join(c.Dir(id), "supervisor.lock")
}

// LogPath is the active log for one stream; RotatedLogPath is the one before it.
func (c *Store) LogPath(id string, stream Stream) string {
	return filepath.Join(c.Dir(id), string(stream)+".log")
}

func (c *Store) RotatedLogPath(id string, stream Stream) string {
	return c.LogPath(id, stream) + ".1"
}

// makeRoomLocked prunes finished runs until there is space, and refuses when
// there is not. It runs under the store lock, so nothing can be created between
// the count and the decision.
func (c *Store) makeRoomLocked() error {
	ids, err := c.ids()
	if err != nil {
		return err
	}
	if len(ids) < c.capacity {
		return nil
	}

	type finished struct {
		id string
		at time.Time
	}
	var terminal []finished
	live := 0
	for _, id := range ids {
		record, err := c.Load(id)
		if err != nil {
			// A record this binary cannot read is left alone. It may be state from
			// a newer Interlock, and deleting the audit trail of a run we simply
			// do not understand is the one unrecoverable mistake available here.
			live++
			continue
		}
		if record.Status.State.Terminal() {
			terminal = append(terminal, finished{id: id, at: record.Spec.CreatedAt})
			continue
		}
		live++
	}
	slices.SortFunc(terminal, func(a, b finished) int { return a.at.Compare(b.at) })

	count := len(ids)
	for _, item := range terminal {
		if count < c.capacity {
			break
		}
		if err := c.remove(item.id); err != nil {
			return err
		}
		count--
	}
	if count >= c.capacity {
		return fmt.Errorf("%w: %d of %d records are live or unreadable; stop a run or clean up first",
			ErrCapacity, live, c.capacity)
	}
	return nil
}

// remove deletes one run directory. The name is validated and the directory is
// checked not to be a symlink first, because this is the only place in the
// package that deletes a tree.
func (c *Store) remove(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: refusing to remove %q", ErrInvalid, id)
	}
	dir := c.Dir(id)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect run directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("run path %s is not a directory", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove run %s: %w", id, err)
	}
	return nil
}

func (c *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run store: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && ValidID(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}

func (c *Store) readSpec(id string) (Spec, error) {
	path := c.SpecPath(id)
	raw, err := readSecure(path)
	if errors.Is(err, os.ErrNotExist) {
		return Spec{}, ErrNotFound
	}
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := state.Validate("managedrun", path, spec.Version); err != nil {
		return Spec{}, err
	}
	if spec.ID != id || !ValidGeneration(spec.Generation) || !spec.Creator.known() ||
		spec.CreatedAt.IsZero() || spec.Options.Validate() != nil {
		return Spec{}, fmt.Errorf("run spec %s has invalid fields", path)
	}
	return spec, nil
}

func (c *Store) readStatus(id string) (Status, error) {
	path := c.StatusPath(id)
	raw, err := readSecure(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, ErrNotFound
	}
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(raw, &status); err != nil {
		return Status{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := state.Validate("managedrun", path, status.Version); err != nil {
		return Status{}, err
	}
	if status.ID != id || !ValidGeneration(status.Generation) || !status.State.Known() ||
		status.Sequence == 0 || !status.Reason.known() {
		return Status{}, fmt.Errorf("run status %s has invalid fields", path)
	}
	return status, nil
}

func (c *Store) writeStatus(id string, status Status) error {
	status.Version = state.CurrentVersion
	return writeAtomicJSON(c.StatusPath(id), status)
}

func (c *Store) lockStore() (func(), error) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run store: %w", err)
	}
	return lockfile.Acquire(filepath.Join(c.dir, ".lock"))
}

// lockRun serialises writers for one run. It uses a fixed set of shards rather
// than a lock file per run so the store does not grow an inode per run purely
// to hold a lock, while the same run always lands on the same shard.
func (c *Store) lockRun(id string) (func(), error) {
	return lockfile.AcquireKeyed(c.dir, id)
}

// ValidID reports whether a string is a well-formed run identifier. It is
// deliberately exact rather than merely safe: every run id reaching a path, a
// socket, an HTTP form or a log read goes through here, and a narrow rule is
// easier to trust than a list of rejected characters.
func ValidID(id string) bool {
	const prefix = "run-"
	if len(id) != len(prefix)+24 || !strings.HasPrefix(id, prefix) {
		return false
	}
	return isHex(id[len(prefix):])
}

// ValidGeneration reports whether a string is a well-formed generation marker.
func ValidGeneration(generation string) bool {
	return len(generation) == 32 && isHex(generation)
}

func isHex(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return value != ""
}

func randomID(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate run identifier: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

func writeExclusiveJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	return writeExclusive(path, raw)
}

// writeExclusive creates a file that must not already exist. It is how the
// immutable half of a run is written: O_EXCL means a second writer loses rather
// than overwrites, and it cannot be tricked into following a symlink that was
// planted at the target.
func writeExclusive(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeAtomicJSON replaces a file in one step. A reader that opens the path
// mid-write sees the previous whole file or the next one, never a partial
// record, which is what lets List read without taking any lock.
func writeAtomicJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp record: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp record: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("secure temp record: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install record: %w", err)
	}
	return nil
}
