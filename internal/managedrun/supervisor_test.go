package managedrun

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gshepptech/interlock/internal/runner"
)

// The fake agent stands in for Codex. It reads its prompt from stdin the way a
// real one does, writes structured lines to stdout, and starts a child of its
// own, so a test can prove that signals reach the whole tree and not just the
// process Interlock started.
const fakeAgent = `#!/bin/sh
cat > "$FAKE_PROMPT"
sh -c 'while :; do sleep 0.05; done' &
echo $! > "$FAKE_CHILD"
echo '{"type":"item.completed","item":{"type":"command"}}'
case "$FAKE_MODE" in
  quick)
    echo '{"type":"turn.completed"}'
    exit 0
    ;;
  fail)
    echo 'something went wrong' >&2
    exit 3
    ;;
  linger)
    trap 'exit 0' INT
    while :; do sleep 0.05 & wait $!; done
    ;;
  stubborn)
    trap '' INT
    while :; do sleep 0.05 & wait $!; done
    ;;
esac
`

type fixture struct {
	store   *Store
	record  Record
	prompt  string
	child   string
	started chan error
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed runs are a unix feature in this release")
	}
	store, _ := testStore(t)
	store.now = time.Now

	dir := t.TempDir()
	agent := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(agent, []byte(fakeAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		store:  store,
		prompt: filepath.Join(dir, "prompt-received"),
		child:  filepath.Join(dir, "child-pid"),
	}
	// The fake reads its instructions from the environment, so it is configured
	// the way a user would configure a real agent: by naming the variables the
	// run is allowed to pass through. The values come from the launching
	// environment, which is what the lookup below stands in for.
	create := request()
	create.Options.Environment = []string{"FAKE_MODE", "FAKE_PROMPT", "FAKE_CHILD"}
	record, err := store.Create(create)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.record = record

	values := map[string]string{
		"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME"),
		"INTERLOCK_CODEX_BIN": agent,
		"FAKE_MODE":           mode,
		"FAKE_PROMPT":         f.prompt,
		"FAKE_CHILD":          f.child,
	}
	if mode == "missing" {
		values["INTERLOCK_CODEX_BIN"] = filepath.Join(dir, "not-here")
	}
	supervisor := &Supervisor{
		Store: store, RunID: f.record.ID(),
		Environment: runner.Environment{
			Root:   store.root,
			Lookup: func(name string) (string, bool) { value, ok := values[name]; return value, ok },
		},
		QuietAfter: 200 * time.Millisecond,
		Grace:      150 * time.Millisecond,
	}
	f.started = make(chan error, 1)
	go func() { f.started <- supervisor.Serve(context.Background()) }()
	t.Cleanup(func() {
		select {
		case <-f.started:
		case <-time.After(10 * time.Second):
			t.Error("the supervisor did not finish")
		}
	})
	return f
}

// await polls for a condition, because these tests are about a real process
// reaching a real state and the alternative is a sleep that is either flaky or
// slow.
func (f *fixture) await(t *testing.T, what string, ok func(Record) bool) Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		record, err := f.store.Load(f.record.ID())
		if err == nil && ok(record) {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; state is %s", what, record.Status.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) awaitState(t *testing.T, state State) Record {
	t.Helper()
	return f.await(t, string(state), func(r Record) bool { return r.Status.State == state })
}

func (f *fixture) awaitTerminal(t *testing.T) Record {
	t.Helper()
	return f.await(t, "the run to finish", func(r Record) bool { return r.Status.State.Terminal() })
}

func (f *fixture) call(t *testing.T, action ControlAction) ControlResponse {
	t.Helper()
	response, err := f.store.Call(f.record.ID(), action)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return response
}

func TestASuccessfulRunIsRecordedEndToEnd(t *testing.T) {
	f := newFixture(t, "quick")
	record := f.awaitTerminal(t)

	if record.Status.State != Completed {
		t.Fatalf("state = %s (%s)", record.Status.State, record.Status.Summary)
	}
	if record.Status.StartedAt.IsZero() || record.Status.EndedAt.IsZero() {
		t.Errorf("timestamps = %#v", record.Status)
	}
	if record.Status.ChildPID == 0 || record.Status.ProcessGroup != record.Status.ChildPID {
		t.Errorf("the child must lead its own process group: %#v", record.Status)
	}
	if record.Status.SupervisorPID != os.Getpid() {
		t.Errorf("supervisor pid = %d", record.Status.SupervisorPID)
	}

	// The prompt reached the agent on stdin, which is the whole reason it is not
	// an argument.
	prompt, err := os.ReadFile(f.prompt)
	if err != nil || string(prompt) != request().Prompt {
		t.Fatalf("prompt delivered = %q err=%v", prompt, err)
	}

	chunk, _, err := f.store.Tail(f.record.ID(), Stdout, 4096)
	if err != nil || !strings.Contains(string(chunk.Bytes), "turn.completed") {
		t.Fatalf("stdout = %q err=%v", chunk.Bytes, err)
	}

	// The socket is cleaned up, so nothing is left listening for a run that has
	// finished.
	if _, err := os.Stat(f.store.ControlAddress(f.record.ID())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control socket survived the run: %v", err)
	}
	if _, err := f.store.Call(f.record.ID(), Probe); !errors.Is(err, ErrNoSupervisor) {
		t.Errorf("Call after the run = %v, want no supervisor", err)
	}
}

func TestAFailedRunKeepsItsExitCode(t *testing.T) {
	f := newFixture(t, "fail")
	record := f.awaitTerminal(t)
	if record.Status.State != Failed || record.Status.Reason != ReasonExitNonZero {
		t.Fatalf("status = %#v", record.Status)
	}
	if record.Status.ExitCode == nil || *record.Status.ExitCode != 3 {
		t.Fatalf("exit code = %v", record.Status.ExitCode)
	}
	chunk, _, err := f.store.Tail(f.record.ID(), Stderr, 4096)
	if err != nil || !strings.Contains(string(chunk.Bytes), "something went wrong") {
		t.Fatalf("stderr = %q err=%v", chunk.Bytes, err)
	}
}

func TestAnAgentThatCannotStartFailsDurably(t *testing.T) {
	f := newFixture(t, "missing")
	record := f.awaitTerminal(t)
	if record.Status.State != Failed || record.Status.Reason != ReasonLaunchFailed {
		t.Fatalf("status = %#v", record.Status)
	}
	if record.Status.Summary == "" {
		t.Error("a launch failure has to say what went wrong")
	}
}

func TestPauseResumeAndGracefulStop(t *testing.T) {
	f := newFixture(t, "linger")
	f.awaitState(t, Running)

	if response := f.call(t, Pause); response.Outcome != Applied {
		t.Fatalf("pause = %#v", response)
	}
	f.awaitState(t, Paused)
	// A second pause is the idempotent case. Users press buttons twice.
	if response := f.call(t, Pause); response.Outcome != Unchanged {
		t.Fatalf("second pause = %#v", response)
	}
	if response := f.call(t, Resume); response.Outcome != Applied {
		t.Fatalf("resume = %#v", response)
	}
	f.awaitState(t, Running)
	if response := f.call(t, Resume); response.Outcome != Unchanged {
		t.Fatalf("second resume = %#v", response)
	}

	if response := f.call(t, Stop); response.Outcome != Applied {
		t.Fatalf("stop = %#v", response)
	}
	record := f.awaitTerminal(t)
	// Whether the agent exits cleanly on the interrupt or dies on it is the
	// agent's business. What Interlock must record either way is that the run
	// was stopped, rather than that something went wrong.
	if record.Status.Reason != ReasonStoppedByUser {
		t.Fatalf("status after a graceful stop = %#v", record.Status)
	}
	if record.Status.ForceRequired {
		t.Error("a run that stopped on request must not still be asking for force")
	}
}

// A paused agent cannot act on an interrupt, so a stop has to wake it first.
// Without that the graceful stop silently never happens.
func TestStoppingAPausedRunWakesItFirst(t *testing.T) {
	f := newFixture(t, "linger")
	f.awaitState(t, Running)
	f.call(t, Pause)
	f.awaitState(t, Paused)

	if response := f.call(t, Stop); response.Outcome != Applied {
		t.Fatalf("stop = %#v", response)
	}
	record := f.awaitTerminal(t)
	if record.Status.Reason != ReasonStoppedByUser {
		t.Fatalf("status = %#v; the interrupt never reached the paused agent", record.Status)
	}
}

func TestForceIsRequiredAndNeverAutomatic(t *testing.T) {
	f := newFixture(t, "stubborn")
	f.awaitState(t, Running)
	child := f.childPID(t)

	if response := f.call(t, Stop); response.Outcome != Applied {
		t.Fatalf("stop = %#v", response)
	}
	f.await(t, "the grace window to expire", func(r Record) bool { return r.Status.ForceRequired })

	// Interlock does not escalate on a timer. The agent is still there.
	if record, _ := f.store.Load(f.record.ID()); record.Status.State != Stopping {
		t.Fatalf("state = %s; a stubborn agent must not be killed automatically", record.Status.State)
	}
	if response := f.call(t, Stop); response.Outcome != ForceRequired {
		t.Fatalf("repeat stop = %#v", response)
	}

	if response := f.call(t, ForceStop); response.Outcome != Applied {
		t.Fatalf("force stop = %#v", response)
	}
	record := f.awaitTerminal(t)
	if record.Status.State != Failed || record.Status.Reason != ReasonForceStopped {
		t.Fatalf("status = %#v", record.Status)
	}
	if record.Status.Signal != "SIGKILL" {
		t.Errorf("signal = %q", record.Status.Signal)
	}

	// The agent started a child of its own. Signalling only the process
	// Interlock launched would have left it running, still holding the
	// repository open after the run was supposedly over.
	waitForExit(t, child)
}

// The other half of that claim: the signal reaches the run's group and nothing
// else. A process outside the group is untouched.
func TestSignalsDoNotReachAnUnrelatedProcess(t *testing.T) {
	f := newFixture(t, "stubborn")
	f.awaitState(t, Running)

	bystander := startSleeper(t)
	f.call(t, ForceStop)
	f.awaitTerminal(t)

	if !alive(bystander) {
		t.Fatal("an unrelated process was killed")
	}
}

func TestAnUnauthorisedRequestChangesNothing(t *testing.T) {
	f := newFixture(t, "linger")
	f.awaitState(t, Running)
	record, _ := f.store.Load(f.record.ID())

	cases := []struct {
		name    string
		request ControlRequest
		want    ControlOutcome
	}{
		{"wrong token", ControlRequest{
			Version: ProtocolVersion, RunID: f.record.ID(),
			Generation: record.Spec.Generation, Token: strings.Repeat("0", 64), Action: Stop,
		}, Unauthorised},
		{"wrong generation", ControlRequest{
			Version: ProtocolVersion, RunID: f.record.ID(),
			Generation: strings.Repeat("a", 32), Token: f.token(t), Action: Stop,
		}, GenerationMismatch},
		{"another run", ControlRequest{
			Version: ProtocolVersion, RunID: "run-" + strings.Repeat("b", 24),
			Generation: record.Spec.Generation, Token: f.token(t), Action: Stop,
		}, MalformedRequest},
		{"another protocol", ControlRequest{
			Version: ProtocolVersion + 1, RunID: f.record.ID(),
			Generation: record.Spec.Generation, Token: f.token(t), Action: Stop,
		}, UnsupportedProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := f.rawCall(t, tc.request)
			if response.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", response.Outcome, tc.want)
			}
			// A refused caller learns nothing about the run, not even its state.
			if response.Status.State != "" {
				t.Errorf("a refused request was told the run is %s", response.Status.State)
			}
		})
	}

	if record, _ := f.store.Load(f.record.ID()); record.Status.State != Running {
		t.Fatalf("state = %s; a refused request must change nothing", record.Status.State)
	}
	f.call(t, Stop)
	f.awaitTerminal(t)
}

func TestASecondSupervisorStandsDown(t *testing.T) {
	f := newFixture(t, "linger")
	f.awaitState(t, Running)

	second := &Supervisor{Store: f.store, RunID: f.record.ID()}
	if err := second.Serve(context.Background()); !errors.Is(err, ErrAlreadySupervised) {
		t.Fatalf("second supervisor = %v, want already supervised", err)
	}
	if record, _ := f.store.Load(f.record.ID()); record.Status.State != Running {
		t.Fatalf("state = %s; a second supervisor must not disturb the run", record.Status.State)
	}
	f.call(t, Stop)
	f.awaitTerminal(t)
}

// Cancelling the supervisor's context is a request to stop the agent politely,
// not permission to abandon it.
func TestCancellingTheSupervisorStopsTheAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed runs are a unix feature in this release")
	}
	store, _ := testStore(t)
	store.now = time.Now
	dir := t.TempDir()
	agent := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(agent, []byte(fakeAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	record := mustCreate(t, store)
	values := map[string]string{
		"PATH": os.Getenv("PATH"), "INTERLOCK_CODEX_BIN": agent,
	}
	supervisor := &Supervisor{
		Store: store, RunID: record.ID(),
		Environment: runner.Environment{
			Root:   store.root,
			Lookup: func(name string) (string, bool) { value, ok := values[name]; return value, ok },
			Link: []string{"FAKE_MODE=linger",
				"FAKE_PROMPT=" + filepath.Join(dir, "p"), "FAKE_CHILD=" + filepath.Join(dir, "c")},
		},
		Grace: 150 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Serve(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if loaded, err := store.Load(record.ID()); err == nil && loaded.Status.State == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancellation = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling did not end the run")
	}
	loaded, _ := store.Load(record.ID())
	if !loaded.Status.State.Terminal() {
		t.Fatalf("state = %s", loaded.Status.State)
	}
}

func (f *fixture) token(t *testing.T) string {
	t.Helper()
	token, err := f.store.Token(f.record.ID())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *fixture) rawCall(t *testing.T, request ControlRequest) ControlResponse {
	t.Helper()
	conn, err := net.DialTimeout("unix", f.store.ControlAddress(f.record.ID()), dialTimeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(callTimeout))
	if err := writeFrame(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	var response ControlResponse
	if err := readFrame(conn, &response); err != nil {
		t.Fatalf("read: %v", err)
	}
	return response
}

func (f *fixture) childPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(f.child)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never recorded its own child")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd.Process.Pid
}

func alive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived the run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
