package managedrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/runner"
	"github.com/gshepptech/interlock/internal/state"
)

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	store := Open(root)
	store.now = func() time.Time { return now }
	sequence := 0
	store.newID = func(prefix string, size int) (string, error) {
		sequence++
		return prefix + fmt.Sprintf("%0*x", size*2, sequence), nil
	}
	return store, &now
}

func request() Create {
	return Create{
		Options: runner.Options{Runner: runner.Codex, Name: "api"},
		Prompt:  "Implement the invitation API.",
		Creator: CreatorCLI,
	}
}

func mustCreate(t *testing.T, store *Store) Record {
	t.Helper()
	record, err := store.Create(request())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return record
}

func TestOpenTouchesNothing(t *testing.T) {
	root := t.TempDir()
	_ = Open(root)
	if _, err := os.Stat(filepath.Join(root, ".interlock")); !os.IsNotExist(err) {
		t.Fatalf("Open created state: %v", err)
	}
}

func TestCreateWritesAnImmutableRecord(t *testing.T) {
	store, now := testStore(t)
	record := mustCreate(t, store)

	if !ValidID(record.ID()) || !ValidGeneration(record.Spec.Generation) {
		t.Fatalf("identifiers = %q / %q", record.ID(), record.Spec.Generation)
	}
	if record.Spec.Version != state.CurrentVersion || !record.Spec.CreatedAt.Equal(*now) {
		t.Fatalf("spec = %#v", record.Spec)
	}
	if record.Status.State != Queued || record.Status.Sequence != 1 {
		t.Fatalf("status = %#v", record.Status)
	}

	prompt, err := store.Prompt(record.ID())
	if err != nil || string(prompt) != request().Prompt {
		t.Fatalf("Prompt = %q err=%v", prompt, err)
	}
	token, err := store.Token(record.ID())
	if err != nil || len(token) != 64 {
		t.Fatalf("Token = %q err=%v", token, err)
	}

	// The specification is written with exclusive creation, so a second writer
	// loses rather than silently replacing what was asked for.
	if err := writeExclusiveJSON(store.SpecPath(record.ID()), record.Spec); !errors.Is(err, os.ErrExist) {
		t.Fatalf("rewriting spec.json = %v, want exists", err)
	}

	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{store.Dir(record.ID()), 0o700},
		{store.SpecPath(record.ID()), 0o600},
		{store.StatusPath(record.ID()), 0o600},
		{store.PromptPath(record.ID()), 0o600},
		{store.TokenPath(record.ID()), 0o600},
	} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatalf("stat %s: %v", item.path, err)
		}
		if info.Mode().Perm() != item.mode {
			t.Errorf("%s mode = %o, want %o", item.path, info.Mode().Perm(), item.mode)
		}
	}
}

func TestCreateRefusesBadRequestsWithoutLeavingState(t *testing.T) {
	cases := []struct {
		name  string
		input Create
		want  error
	}{
		{"unknown runner", Create{
			Options: runner.Options{Runner: "gemini"}, Prompt: "go", Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"empty prompt", Create{
			Options: runner.Options{Runner: runner.Codex}, Prompt: "   ", Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"oversized prompt", Create{
			Options: runner.Options{Runner: runner.Codex},
			Prompt:  strings.Repeat("a", runner.MaxPromptBytes+1), Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"environment value", Create{
			Options: runner.Options{Runner: runner.Codex, Environment: []string{"TOKEN=secret"}},
			Prompt:  "go", Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"escaping working directory", Create{
			Options: runner.Options{Runner: runner.Codex, WorkingDir: "../elsewhere"},
			Prompt:  "go", Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"missing working directory", Create{
			Options: runner.Options{Runner: runner.Codex, WorkingDir: "nowhere"},
			Prompt:  "go", Creator: CreatorCLI,
		}, runner.ErrInvalid},
		{"unknown creator", Create{
			Options: runner.Options{Runner: runner.Codex}, Prompt: "go", Creator: "robot",
		}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := testStore(t)
			if _, err := store.Create(tc.input); !errors.Is(err, tc.want) {
				t.Fatalf("Create = %v, want %v", err, tc.want)
			}
			records, err := store.List()
			if err != nil || len(records) != 0 {
				t.Fatalf("List = %#v err=%v; a refused launch must leave nothing", records, err)
			}
		})
	}
}

func TestTransitionTableIsClosed(t *testing.T) {
	for _, from := range States() {
		for _, to := range States() {
			legal := CanTransition(from, to)
			switch {
			case from == to && legal:
				t.Errorf("%s -> %s reported as a transition", from, to)
			case from.Terminal() && legal:
				t.Errorf("%s is terminal but allows %s", from, to)
			case to == Lost && from.Live() && !legal:
				t.Errorf("%s must be able to become lost", from)
			}
		}
	}
	if CanTransition(Queued, Running) {
		t.Error("queued must reach running through starting")
	}
	if CanTransition(Paused, Blocked) {
		t.Error("a paused agent cannot be blocked by a hook it cannot run")
	}
	if !CanTransition(Stopping, Failed) || !CanTransition(Stopping, Completed) {
		t.Error("a stopping run must be able to finish either way")
	}
}

func TestTransitionRecordsTimestampsAndRefusesIllegalMoves(t *testing.T) {
	store, now := testStore(t)
	record := mustCreate(t, store)
	id, generation := record.ID(), record.Spec.Generation

	if _, _, err := store.Transition(id, generation, Running, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued -> running = %v, want conflict", err)
	}

	*now = now.Add(time.Second)
	if _, changed, err := store.Transition(id, generation, Starting, nil); err != nil || !changed {
		t.Fatalf("Transition to starting = changed %v err %v", changed, err)
	}
	*now = now.Add(time.Second)
	started := *now
	status, changed, err := store.Transition(id, generation, Running, nil)
	if err != nil || !changed || !status.StartedAt.Equal(started) || status.Sequence != 3 {
		t.Fatalf("Transition to running = %#v changed=%v err=%v", status, changed, err)
	}

	// A second request for the state already held is the idempotent case, not a
	// failure: users press pause twice and control messages get retried.
	status, changed, err = store.Transition(id, generation, Running, nil)
	if err != nil || changed || status.Sequence != 3 {
		t.Fatalf("repeat transition = %#v changed=%v err=%v", status, changed, err)
	}

	*now = now.Add(time.Second)
	ended := *now
	code := 2
	status, _, err = store.Transition(id, generation, Failed, func(s *Status) {
		s.ExitCode, s.Reason, s.Summary = &code, ReasonExitNonZero, "tests failed\x07"
	})
	if err != nil || !status.EndedAt.Equal(ended) || *status.ExitCode != 2 {
		t.Fatalf("Transition to failed = %#v err=%v", status, err)
	}
	if status.Summary != "tests failed" {
		t.Errorf("summary = %q; control characters must not survive", status.Summary)
	}

	if _, _, err := store.Transition(id, generation, Running, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("resurrecting a terminal run = %v, want conflict", err)
	}
	if _, _, err := store.Update(id, generation, func(s *Status) { s.Summary = "later" }); !errors.Is(err, ErrConflict) {
		t.Fatalf("updating a terminal run = %v, want conflict", err)
	}
}

func TestUpdateKeepsStateAndSequence(t *testing.T) {
	store, now := testStore(t)
	record := mustCreate(t, store)
	id, generation := record.ID(), record.Spec.Generation

	*now = now.Add(time.Second)
	output := *now
	status, changed, err := store.Update(id, generation, func(s *Status) {
		s.LastOutputAt, s.ChildPID, s.State, s.Sequence = output, 4242, Completed, 99
	})
	if err != nil || !changed {
		t.Fatalf("Update = %#v changed=%v err=%v", status, changed, err)
	}
	if status.State != Queued || status.Sequence != 1 {
		t.Fatalf("Update moved the run: %#v", status)
	}
	if !status.LastOutputAt.Equal(output) || status.ChildPID != 4242 {
		t.Fatalf("Update lost its fields: %#v", status)
	}

	// Rewriting the same values is not a change. Without that, a supervisor
	// polling for output would churn the file and every reader would see a run
	// that never settles.
	*now = now.Add(time.Second)
	if _, changed, err := store.Update(id, generation, func(s *Status) { s.ChildPID = 4242 }); err != nil || changed {
		t.Fatalf("no-op Update = changed %v err %v", changed, err)
	}
}

func TestGenerationMustMatch(t *testing.T) {
	store, _ := testStore(t)
	record := mustCreate(t, store)
	other := strings.Repeat("a", 32)
	if _, _, err := store.Transition(record.ID(), other, Starting, nil); !errors.Is(err, ErrGeneration) {
		t.Fatalf("Transition with a stale generation = %v, want generation mismatch", err)
	}
	if _, _, err := store.Update(record.ID(), "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update without a generation = %v, want invalid", err)
	}
}

func TestLoadAndListIgnoreUnknownIdentifiers(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.Load("run-../../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load with a traversal id = %v, want not found", err)
	}
	if _, err := store.Load("run-" + strings.Repeat("0", 24)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load of a missing run = %v, want not found", err)
	}
	if err := os.MkdirAll(filepath.Join(store.Root(), "not-a-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 0 {
		t.Fatalf("List = %#v err=%v", records, err)
	}
}

func TestListIsNewestFirstAndSkipsUnreadableRecords(t *testing.T) {
	store, now := testStore(t)
	first := mustCreate(t, store)
	*now = now.Add(time.Minute)
	second := mustCreate(t, store)

	records, err := store.List()
	if err != nil || len(records) != 2 || records[0].ID() != second.ID() {
		t.Fatalf("List = %#v err=%v", records, err)
	}

	if err := os.WriteFile(store.StatusPath(first.ID()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err = store.List()
	if err != nil || len(records) != 1 || records[0].ID() != second.ID() {
		t.Fatalf("List with a broken record = %#v err=%v", records, err)
	}
	issues, err := store.Inspect()
	if err != nil || len(issues) != 1 || !strings.Contains(issues[0].Path, first.ID()) {
		t.Fatalf("Inspect = %#v err=%v", issues, err)
	}
}

func TestFutureSchemaIsReadOnlyAndNeverPruned(t *testing.T) {
	store, now := testStore(t)
	store.capacity = 2
	future := mustCreate(t, store)
	raw, err := json.Marshal(map[string]any{
		"version": state.CurrentVersion + 1, "id": future.ID(),
		"generation": future.Spec.Generation, "state": Completed, "sequence": 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatusPath(future.ID()), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var unsupported *state.UnsupportedVersionError
	if _, err := store.Load(future.ID()); !errors.As(err, &unsupported) {
		t.Fatalf("Load of future state = %v, want unsupported version", err)
	}
	if _, _, err := store.Transition(future.ID(), future.Spec.Generation, Starting, nil); !errors.As(err, &unsupported) {
		t.Fatalf("Transition on future state = %v, want unsupported version", err)
	}

	// The record reads as finished, so a naive prune would delete it. It must be
	// treated as live instead: deleting state written by a newer binary is the
	// one mistake here that cannot be undone.
	*now = now.Add(time.Minute)
	if _, err := store.Create(request()); err != nil {
		t.Fatalf("Create alongside future state: %v", err)
	}
	*now = now.Add(time.Minute)
	if _, err := store.Create(request()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Create at capacity = %v, want capacity", err)
	}
	if _, err := os.Stat(store.StatusPath(future.ID())); err != nil {
		t.Fatalf("future record was removed: %v", err)
	}
}

func TestCapacityPrunesOldestFinishedRunsOnly(t *testing.T) {
	store, now := testStore(t)
	store.capacity = 3

	var ids []string
	for i := range 3 {
		record := mustCreate(t, store)
		ids = append(ids, record.ID())
		if i < 2 {
			if _, _, err := store.Transition(record.ID(), record.Spec.Generation, Failed, nil); err != nil {
				t.Fatal(err)
			}
		}
		*now = now.Add(time.Minute)
	}

	fresh, err := store.Create(request())
	if err != nil {
		t.Fatalf("Create at capacity: %v", err)
	}
	records, err := store.List()
	if err != nil || len(records) != 3 {
		t.Fatalf("List after prune = %#v err=%v", records, err)
	}
	if _, err := store.Load(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest finished run survived: %v", err)
	}
	for _, id := range []string{ids[1], ids[2], fresh.ID()} {
		if _, err := store.Load(id); err != nil {
			t.Fatalf("Load %s: %v", id, err)
		}
	}

	// One finished run is left, so one more launch is affordable. After that
	// every record is live, there is nothing safe to remove, and the answer is a
	// refusal rather than a deletion.
	*now = now.Add(time.Minute)
	if _, err := store.Create(request()); err != nil {
		t.Fatalf("Create with one finished run left: %v", err)
	}
	*now = now.Add(time.Minute)
	if _, err := store.Create(request()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Create with only live runs = %v, want capacity", err)
	}
	if _, err := store.Load(ids[2]); err != nil {
		t.Fatalf("a live run was pruned to make room: %v", err)
	}
}

func TestConcurrentTransitionsProduceOneWinner(t *testing.T) {
	store, _ := testStore(t)
	store.now = time.Now
	record := mustCreate(t, store)
	id, generation := record.ID(), record.Spec.Generation

	const racers = 8
	var wg sync.WaitGroup
	changes := make(chan bool, racers)
	for range racers {
		wg.Go(func() {
			_, changed, err := store.Transition(id, generation, Starting, nil)
			if err != nil {
				t.Errorf("Transition: %v", err)
			}
			changes <- changed
		})
	}
	wg.Wait()
	close(changes)

	won := 0
	for changed := range changes {
		if changed {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d racers reported a change, want exactly 1", won)
	}
	final, err := store.Load(id)
	if err != nil || final.Status.State != Starting || final.Status.Sequence != 2 {
		t.Fatalf("final = %#v err=%v", final.Status, err)
	}
}

func TestSecretsAreNotReadThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hardened read is a unix build")
	}
	store, _ := testStore(t)
	record := mustCreate(t, store)
	elsewhere := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(elsewhere, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.TokenPath(record.ID())); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, store.TokenPath(record.ID())); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Token(record.ID()); err == nil {
		t.Fatal("Token followed a symlink")
	}
}

func TestRecordDescribesItsOutcome(t *testing.T) {
	code := 3
	cases := []struct {
		name   string
		status Status
		want   string
	}{
		{"completed", Status{State: Completed}, "completed"},
		{"exit code", Status{State: Failed, ExitCode: &code}, "failed with exit code 3"},
		{"signal", Status{State: Failed, Signal: "SIGKILL"}, "failed after SIGKILL"},
		{"force required", Status{State: Stopping, ForceRequired: true},
			"stopping; the agent has not exited and force is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Record{Status: tc.status}).Describe(); got != tc.want {
				t.Fatalf("Describe = %q, want %q", got, tc.want)
			}
		})
	}
}
