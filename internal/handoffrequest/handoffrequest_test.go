package handoffrequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/state"
)

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Now().UTC()
	store := Open(root)
	store.now = func() time.Time { return now }
	sequence := 0
	store.newID = func() (string, error) {
		sequence++
		return fmt.Sprintf("hr-%04d", sequence), nil
	}
	return store, &now
}

func input(note string) Create {
	return Create{
		SourceSession: "session-1", TargetKind: Agent, Target: "codex",
		Note: note, Requester: CLI,
	}
}

func TestOpenIsReadOnly(t *testing.T) {
	root := t.TempDir()
	_ = Open(root)
	if _, err := os.Stat(filepath.Join(root, ".interlock")); !os.IsNotExist(err) {
		t.Fatalf("Open created state: %v", err)
	}
}

func TestCreateListShowResolveAndDeduplicate(t *testing.T) {
	store, _ := testStore(t)
	request, created, err := store.Create(input("Please continue the API work."))
	if err != nil || !created {
		t.Fatalf("Create = %#v created=%v err=%v", request, created, err)
	}
	if request.Version != state.CurrentVersion || request.ID == "" {
		t.Fatalf("request = %#v", request)
	}
	duplicate, created, err := store.Create(input("Please continue the API work."))
	if err != nil || created || duplicate.ID != request.ID {
		t.Fatalf("duplicate = %#v created=%v err=%v", duplicate, created, err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || !items[0].Open() {
		t.Fatalf("List = %#v err=%v", items, err)
	}
	shown, err := store.Show(request.ID)
	if err != nil || shown.Request.Note != request.Note {
		t.Fatalf("Show = %#v err=%v", shown, err)
	}

	resolution, resolved, err := store.Resolve(request.ID, Dashboard)
	if err != nil || !resolved || resolution.Version != state.CurrentVersion {
		t.Fatalf("Resolve = %#v resolved=%v err=%v", resolution, resolved, err)
	}
	again, resolved, err := store.Resolve(request.ID, Dashboard)
	if err != nil || resolved || again.ResolvedAt != resolution.ResolvedAt {
		t.Fatalf("second Resolve = %#v resolved=%v err=%v", again, resolved, err)
	}
	shown, err = store.Show(request.ID)
	if err != nil || shown.Open() {
		t.Fatalf("resolved Show = %#v err=%v", shown, err)
	}
}

func TestConcurrentDuplicateCreateProducesOneOpenRequest(t *testing.T) {
	store, _ := testStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := store.Create(input("same"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %d err=%v", len(items), err)
	}
}

func TestCrossProcessCreateAndResolveRemainUnique(t *testing.T) {
	if mode := os.Getenv("INTERLOCK_HANDOFF_HELPER"); mode != "" {
		store := Open(os.Getenv("INTERLOCK_HANDOFF_ROOT"))
		switch mode {
		case "create":
			_, _, err := store.Create(input("cross-process"))
			if err != nil {
				t.Fatal(err)
			}
		case "resolve":
			_, _, err := store.Resolve(os.Getenv("INTERLOCK_HANDOFF_ID"), CLI)
			if err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	root := t.TempDir()
	run := func(mode, id string) {
		t.Helper()
		const workers = 10
		commands := make([]*exec.Cmd, 0, workers)
		for range workers {
			command := exec.Command(os.Args[0], "-test.run=TestCrossProcessCreateAndResolveRemainUnique$")
			command.Env = append(os.Environ(),
				"INTERLOCK_HANDOFF_HELPER="+mode,
				"INTERLOCK_HANDOFF_ROOT="+root,
				"INTERLOCK_HANDOFF_ID="+id,
			)
			if err := command.Start(); err != nil {
				t.Fatalf("start %s helper: %v", mode, err)
			}
			commands = append(commands, command)
		}
		for _, command := range commands {
			if err := command.Wait(); err != nil {
				t.Fatalf("wait for %s helper: %v", mode, err)
			}
		}
	}

	run("create", "")
	store := Open(root)
	items, err := store.List()
	if err != nil || len(items) != 1 || !items[0].Open() {
		t.Fatalf("cross-process create = %#v err=%v", items, err)
	}
	run("resolve", items[0].Request.ID)
	items, err = store.List()
	if err != nil || len(items) != 1 || items[0].Open() {
		t.Fatalf("cross-process resolve = %#v err=%v", items, err)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	resolutionCount := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".resolution.json") {
			resolutionCount++
		}
	}
	if resolutionCount != 1 {
		t.Fatalf("resolution files = %d", resolutionCount)
	}
}

func TestCapacityRefusesOpenQueueAndPrunesOldestResolved(t *testing.T) {
	store, now := testStore(t)
	var first Request
	for i := range MaxRequests {
		*now = (*now).Add(time.Second)
		request, created, err := store.Create(input(fmt.Sprintf("request-%d", i)))
		if err != nil || !created {
			t.Fatalf("Create %d: created=%v err=%v", i, created, err)
		}
		if i == 0 {
			first = request
		}
	}
	if _, _, err := store.Create(input("overflow")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow error = %v, want ErrCapacity", err)
	}
	if _, _, err := store.Resolve(first.ID, CLI); err != nil {
		t.Fatalf("Resolve oldest: %v", err)
	}
	created, ok, err := store.Create(input("after-prune"))
	if err != nil || !ok || created.ID == "" {
		t.Fatalf("Create after prune = %#v created=%v err=%v", created, ok, err)
	}
	if _, err := store.Show(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest resolved request still present: %v", err)
	}
	items, err := store.List()
	if err != nil || len(items) != MaxRequests {
		t.Fatalf("List after prune = %d err=%v", len(items), err)
	}
}

func TestInterruptedPruneCannotReopenAResolvedRequest(t *testing.T) {
	store, _ := testStore(t)
	request, _, err := store.Create(input("resolved"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resolution, _, err := store.Resolve(request.ID, CLI)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.Remove(store.requestPath(request.ID)); err != nil {
		t.Fatalf("simulate request-first prune: %v", err)
	}
	items, err := store.List()
	if err != nil || len(items) != 0 {
		t.Fatalf("orphan resolution resurrected request: %#v err=%v", items, err)
	}
	if _, _, err := store.Resolve(resolution.RequestID, CLI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan resolution was resolvable: %v", err)
	}
	issues, err := store.Inspect()
	if err != nil || len(issues) != 1 || !strings.Contains(issues[0].Err.Error(), "orphan resolution") {
		t.Fatalf("Inspect = %#v err=%v", issues, err)
	}
}

func TestFutureRequestIsReportedAndNeverResolved(t *testing.T) {
	store, _ := testStore(t)
	request, _, err := store.Create(input("future"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	file := store.requestPath(request.ID)
	raw, _ := os.ReadFile(file)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	doc["version"] = float64(99)
	original, _ := json.Marshal(doc)
	if err := os.WriteFile(file, original, 0o644); err != nil {
		t.Fatalf("write future: %v", err)
	}
	_, _, err = store.Resolve(request.ID, CLI)
	var unsupported *state.UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Resolve error = %v, want UnsupportedVersionError", err)
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(original) {
		t.Fatal("future request changed")
	}
	issues, err := store.Inspect()
	if err != nil || len(issues) != 1 {
		t.Fatalf("Inspect = %#v err=%v", issues, err)
	}
}

func TestRecordIdentityMustMatchFilename(t *testing.T) {
	store, _ := testStore(t)
	request, _, err := store.Create(input("identity"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	requestPath := store.requestPath(request.ID)
	raw, _ := os.ReadFile(requestPath)
	var requestDoc map[string]any
	_ = json.Unmarshal(raw, &requestDoc)
	requestDoc["id"] = "hr-different"
	raw, _ = json.Marshal(requestDoc)
	if err := os.WriteFile(requestPath, raw, 0o600); err != nil {
		t.Fatalf("rewrite request: %v", err)
	}
	if items, err := store.List(); err != nil || len(items) != 0 {
		t.Fatalf("mismatched request listed: %#v err=%v", items, err)
	}
	issues, err := store.Inspect()
	if err != nil || len(issues) != 1 {
		t.Fatalf("request Inspect = %#v err=%v", issues, err)
	}

	store, _ = testStore(t)
	request, _, err = store.Create(input("resolution identity"))
	if err != nil {
		t.Fatalf("Create resolution fixture: %v", err)
	}
	if _, _, err := store.Resolve(request.ID, CLI); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resolutionPath := store.resolutionPath(request.ID)
	raw, _ = os.ReadFile(resolutionPath)
	var resolutionDoc map[string]any
	_ = json.Unmarshal(raw, &resolutionDoc)
	resolutionDoc["request_id"] = "hr-different"
	raw, _ = json.Marshal(resolutionDoc)
	if err := os.WriteFile(resolutionPath, raw, 0o600); err != nil {
		t.Fatalf("rewrite resolution: %v", err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || !items[0].Open() {
		t.Fatalf("mismatched resolution closed request: %#v err=%v", items, err)
	}
	issues, err = store.Inspect()
	if err != nil || len(issues) != 1 {
		t.Fatalf("resolution Inspect = %#v err=%v", issues, err)
	}
}

func TestValidationBoundsInputs(t *testing.T) {
	store, _ := testStore(t)
	for name, candidate := range map[string]Create{
		"source":    {SourceSession: "bad id", TargetKind: Agent, Target: "codex", Requester: CLI},
		"kind":      {SourceSession: "s1", TargetKind: "person", Target: "codex", Requester: CLI},
		"target":    {SourceSession: "s1", TargetKind: Team, Target: "bad target", Requester: CLI},
		"requester": {SourceSession: "s1", TargetKind: Team, Target: "api", Requester: "web"},
		"note":      {SourceSession: "s1", TargetKind: Team, Target: "api", Requester: CLI, Note: string(make([]byte, MaxNote+1))},
	} {
		if _, _, err := store.Create(candidate); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create invalid %s error = %v", name, err)
		}
	}
}
