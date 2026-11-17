package managedrun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gshepptech/interlock/internal/lockfile"
	"github.com/gshepptech/interlock/internal/runner"
)

// A supervisor is one process that owns one agent.
//
// It exists because the two obvious alternatives are both wrong. If the CLI
// owned the agent, closing the terminal would end the run. If the dashboard
// owned it, restarting the dashboard would end every run. So the process that
// holds the child's output pipes, answers control requests and records what
// happened is a separate, detached process whose only job is that one run, and
// every other part of Interlock is free to come and go.
//
// It is deliberately not a member of the process group it supervises. That is
// what lets it stay responsive while the agent is paused: SIGSTOP goes to the
// group, and the supervisor is outside it.

const (
	// DefaultQuietAfter is how long without output before a live run is
	// described as quiet. It is a statement about output only. The process is
	// known to be alive either way, which is exactly the claim hook-only
	// sessions cannot make.
	DefaultQuietAfter = 90 * time.Second
	// DefaultGrace is how long a graceful stop is given before Interlock says
	// force is required. It never escalates on its own: killing an agent
	// mid-write is a decision for a person.
	DefaultGrace = 20 * time.Second
	// outputPersistEvery throttles status writes while output is flowing. The
	// timestamp is for display, and a file rewritten on every line would be a
	// worse lie than one a second out of date.
	outputPersistEvery = 2 * time.Second
	// maxDecodeLine bounds what is accumulated for decoding. Longer lines still
	// reach the log in full; they are simply displayed rather than parsed.
	maxDecodeLine = 64 * 1024
	// flushGrace is how long the output readers get after the agent exits before
	// their pipes are closed under them. Long enough to drain what is buffered,
	// short enough that a background process the agent left behind cannot pin
	// the supervisor open.
	flushGrace = 500 * time.Millisecond
)

// ErrAlreadySupervised reports that another supervisor already holds this run.
// Starting a second one is a no-change result rather than a failure, because
// the honest outcome is that the run is supervised.
var ErrAlreadySupervised = errors.New("run already has a supervisor")

// Supervisor runs one agent and answers for it.
type Supervisor struct {
	Store *Store
	RunID string
	// Environment supplies the repository root and the launching environment.
	// The run linkage is added here, not by the caller.
	Environment runner.Environment
	Now         func() time.Time
	QuietAfter  time.Duration
	Grace       time.Duration

	mu            sync.Mutex
	generation    string
	group         int
	paused        bool
	stopping      bool
	forced        bool
	forceRequired bool
	lastOutput    time.Time
	lastPersisted time.Time
}

// Serve claims the run, starts the agent, and does not return until the agent
// has exited and its outcome is recorded.
func (s *Supervisor) Serve(ctx context.Context) error {
	if s.Store == nil || !ValidID(s.RunID) {
		return ErrNotFound
	}
	s.applyDefaults()

	unlock, err := s.claim()
	if err != nil {
		return err
	}
	defer unlock()

	record, err := s.Store.Load(s.RunID)
	if err != nil {
		return err
	}
	token, err := s.Store.Token(s.RunID)
	if err != nil {
		return err
	}
	s.generation = record.Spec.Generation

	// Only a supervisor holding the lock may claim a queued run, so a duplicate
	// start cannot move the same record twice.
	if _, _, err := s.Store.Transition(s.RunID, s.generation, Starting, nil); err != nil {
		return err
	}

	adapter, launch, prompt, err := s.prepare(record)
	if err != nil {
		s.fail(ReasonLaunchFailed, err.Error())
		return err
	}
	return s.run(ctx, adapter, launch, prompt, token)
}

// claim takes the run's lock, which is both the exclusion between supervisors
// and the proof of liveness everything else relies on.
//
// The retry is not politeness. Reconciliation asks whether the lock is held by
// trying to take it, which means there is a brief moment when a perfectly good
// run appears to be claimed. Without a second attempt, a supervisor starting in
// exactly that moment would stand down and leave a run nobody ever starts.
func (s *Supervisor) claim() (func(), error) {
	for attempt := range 4 {
		unlock, acquired, err := lockfile.TryAcquire(s.Store.SupervisorLockPath(s.RunID))
		if err != nil {
			return nil, err
		}
		if acquired {
			return unlock, nil
		}
		if attempt < 3 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil, ErrAlreadySupervised
}

func (s *Supervisor) applyDefaults() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.QuietAfter <= 0 {
		s.QuietAfter = DefaultQuietAfter
	}
	if s.Grace <= 0 {
		s.Grace = DefaultGrace
	}
}

// prepare turns the stored specification into a command and a prompt. Anything
// that fails here fails before a process exists, so the run is durably failed
// and safe to retry.
func (s *Supervisor) prepare(record Record) (runner.Adapter, runner.Launch, []byte, error) {
	adapter, err := runner.For(record.Spec.Options.Runner)
	if err != nil {
		return nil, runner.Launch{}, nil, err
	}
	env := s.Environment
	env.Link = s.link(record)
	launch, err := adapter.Build(record.Spec.Options, env)
	if err != nil {
		return nil, runner.Launch{}, nil, err
	}
	prompt, err := s.Store.Prompt(s.RunID)
	if err != nil {
		return nil, runner.Launch{}, nil, err
	}
	return adapter, launch, prompt, nil
}

// link is how a hook fired by the agent knows which run it belongs to. Without
// it a refusal could not be correlated, and "blocked" would have to be guessed
// from output text, which is precisely the inference this design refuses to
// make.
func (s *Supervisor) link(record Record) []string {
	link := []string{
		"INTERLOCK_RUN_ID=" + record.Spec.ID,
		"INTERLOCK_AGENT=" + string(record.Spec.Options.Runner),
	}
	if team := record.Spec.Options.Team; team != "" {
		link = append(link, "INTERLOCK_TEAM="+team)
	}
	// The agent is told what it is working on, so anything it runs (including
	// a work-store command) can name the item without being passed it again.
	if item := record.Spec.Options.Item; item != "" {
		link = append(link, "INTERLOCK_ITEM="+item)
	}
	return link
}

func (s *Supervisor) run(ctx context.Context, adapter runner.Adapter, launch runner.Launch,
	prompt []byte, token string) error {
	stdout, err := s.Store.OpenLog(s.RunID, Stdout)
	if err != nil {
		s.fail(ReasonLaunchFailed, err.Error())
		return err
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := s.Store.OpenLog(s.RunID, Stderr)
	if err != nil {
		s.fail(ReasonLaunchFailed, err.Error())
		return err
	}
	defer func() { _ = stderr.Close() }()

	cmd := exec.Command(launch.Path, launch.Args...)
	cmd.Dir, cmd.Env = launch.Dir, launch.Env
	// The pipes are made here rather than with StdinPipe and friends because
	// this supervisor has to be able to stop reading. An agent leaves children
	// behind, and a child inherits the output pipe: waiting for end-of-file
	// would mean waiting for every process the agent ever started, and a run
	// whose agent exited an hour ago would still be "running".
	pipes, err := openPipes()
	if err != nil {
		s.fail(ReasonLaunchFailed, err.Error())
		return err
	}
	defer pipes.closeParent()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes.stdinRead, pipes.stdoutWrite, pipes.stderrWrite

	if err := startInGroup(cmd); err != nil {
		pipes.closeChild()
		s.fail(ReasonLaunchFailed, err.Error())
		return fmt.Errorf("start agent: %w", err)
	}
	// The child has its own copies now, so this process lets go of the ends it
	// handed over. Otherwise nothing would ever see end-of-file.
	pipes.closeChild()
	// The child leads its own group, so its pid is the group id.
	s.mu.Lock()
	s.group, s.lastOutput = cmd.Process.Pid, s.Now()
	s.mu.Unlock()

	listener, err := s.listen()
	if err != nil {
		// The agent is already running, so there is no honest way to report a
		// launch failure. Kill what we started rather than leave an agent nobody
		// can control.
		_ = signalGroup(cmd.Process.Pid, signalKill)
		_ = cmd.Wait()
		s.fail(ReasonLaunchFailed, err.Error())
		return err
	}

	// The prompt is the last thing the agent needs before it can work, and
	// closing stdin is what tells it there is no more.
	go func() {
		_, _ = pipes.stdinWrite.Write(prompt)
		_ = pipes.stdinWrite.Close()
	}()

	if _, _, err := s.Store.Transition(s.RunID, s.generation, Running, func(status *Status) {
		status.SupervisorPID = os.Getpid()
		status.ChildPID, status.ProcessGroup = cmd.Process.Pid, cmd.Process.Pid
		status.LastOutputAt = s.Now()
	}); err != nil {
		return err
	}

	done := make(chan struct{})
	drained := make(chan struct{})
	var streams sync.WaitGroup
	streams.Go(func() { s.drain(adapter, Stdout, pipes.stdoutRead, stdout) })
	streams.Go(func() { s.drain(adapter, Stderr, pipes.stderrRead, stderr) })
	go func() {
		streams.Wait()
		close(drained)
	}()
	go s.accept(listener, token, done)
	go s.watchQuiet(done)
	go s.watchContext(ctx, done)

	waitErr := cmd.Wait()
	// The agent has gone. Anything already in the pipes is still worth having,
	// so the readers get a moment to finish before the ends are closed under
	// them. What that closing prevents is a background process the agent left
	// behind holding this supervisor open indefinitely.
	select {
	case <-drained:
	case <-time.After(flushGrace):
		pipes.closeParent()
		<-drained
	}

	close(done)
	_ = listener.Close()
	_ = os.Remove(s.Store.ControlAddress(s.RunID))
	_ = os.Remove(s.Store.ControlAddrPath(s.RunID))

	return s.finish(waitErr)
}

// pipes are the three descriptors between this process and the agent. They are
// tracked as a set because which end belongs to whom is the entire subtlety:
// the parent must let go of the child's ends or nothing sees end-of-file, and
// it must keep its own so it can stop reading when the agent is gone.
type pipeSet struct {
	stdinRead, stdinWrite    *os.File
	stdoutRead, stdoutWrite  *os.File
	stderrRead, stderrWrite  *os.File
	childClosed, parentClose sync.Once
}

func openPipes() (*pipeSet, error) {
	set := &pipeSet{}
	var err error
	if set.stdinRead, set.stdinWrite, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("open agent input: %w", err)
	}
	if set.stdoutRead, set.stdoutWrite, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("open agent output: %w", err)
	}
	if set.stderrRead, set.stderrWrite, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("open agent errors: %w", err)
	}
	return set, nil
}

func (p *pipeSet) closeChild() {
	p.childClosed.Do(func() {
		_ = p.stdinRead.Close()
		_ = p.stdoutWrite.Close()
		_ = p.stderrWrite.Close()
	})
}

func (p *pipeSet) closeParent() {
	p.parentClose.Do(func() {
		_ = p.stdinWrite.Close()
		_ = p.stdoutRead.Close()
		_ = p.stderrRead.Close()
	})
}

// listen publishes the control socket, beside the run's other files when the
// path fits and somewhere short when it does not.
//
// A leftover socket file from a supervisor that died is removed first: the file
// outlives the process, so its presence proves nothing, which is exactly why
// every request carries the run generation as well.
func (s *Supervisor) listen() (net.Listener, error) {
	preferred := s.Store.SocketPath(s.RunID)
	if len(preferred) <= MaxSocketPath {
		listener, err := bind(preferred)
		if err == nil {
			return listener, nil
		}
		// A bind can still fail on length: the limit differs between systems and
		// is not worth guessing twice.
		if !isAddressTooLong(err) {
			return nil, err
		}
	}
	fallback := FallbackSocketPath(s.RunID)
	listener, err := bind(fallback)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(s.Store.ControlAddrPath(s.RunID), []byte(fallback)); err != nil {
		_ = listener.Close()
		_ = os.Remove(fallback)
		return nil, err
	}
	return listener, nil
}

func bind(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clear stale control socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("open control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("secure control socket: %w", err)
	}
	return listener, nil
}

// accept answers control requests one at a time. Serialising them is not a
// performance compromise: these are signals and state transitions, and two of
// them overlapping is a race nobody needs.
func (s *Supervisor) accept(listener net.Listener, token string, done <-chan struct{}) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.answer(conn, token)
		_ = conn.Close()
	}
}

func (s *Supervisor) answer(conn net.Conn, token string) {
	_ = conn.SetDeadline(time.Now().Add(callTimeout))
	var request ControlRequest
	if err := readFrame(conn, &request); err != nil {
		_ = writeFrame(conn, ControlResponse{
			Version: ProtocolVersion, Outcome: MalformedRequest,
			Detail: "the request could not be read",
		})
		return
	}
	outcome, detail := authorise(request, s.RunID, s.generation, token)
	if outcome != Applied {
		// An unauthorised caller learns nothing about the run, not even its state.
		_ = writeFrame(conn, ControlResponse{Version: ProtocolVersion, Outcome: outcome, Detail: detail})
		return
	}
	outcome, detail = s.apply(request.Action)
	status, err := s.Store.Load(s.RunID)
	response := ControlResponse{Version: ProtocolVersion, Outcome: outcome, Detail: detail}
	if err == nil {
		response.Status = status.Status
	}
	_ = writeFrame(conn, response)
}

// apply performs one lifecycle request against the live process group.
func (s *Supervisor) apply(action ControlAction) (ControlOutcome, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if action == Probe {
		return Unchanged, "supervisor is live"
	}
	if s.group == 0 || !groupAlive(s.group) {
		return NotRunning, "the agent is no longer running"
	}

	switch action {
	case Pause:
		if s.paused {
			return Unchanged, "the run is already paused"
		}
		if err := signalGroup(s.group, signalPause); err != nil {
			return NotRunning, "the agent is no longer running"
		}
		s.paused = true
		s.transition(Paused, nil)
		return Applied, "the agent and everything it started are stopped"

	case Resume:
		if !s.paused {
			return Unchanged, "the run is not paused"
		}
		if err := signalGroup(s.group, signalResume); err != nil {
			return NotRunning, "the agent is no longer running"
		}
		s.paused = false
		s.transition(Running, nil)
		return Applied, "the agent has been resumed"

	case Stop:
		if s.stopping {
			if s.forceRequired {
				return ForceRequired, "the agent has not exited; force is required"
			}
			return Unchanged, "the run is already stopping"
		}
		// A stopped process cannot act on an interrupt, so it has to be woken
		// first or the graceful stop would silently never happen.
		if s.paused {
			_ = signalGroup(s.group, signalResume)
			s.paused = false
		}
		if err := signalGroup(s.group, signalInterrupt); err != nil {
			return NotRunning, "the agent is no longer running"
		}
		s.stopping = true
		s.transition(Stopping, func(status *Status) { status.Reason = ReasonStoppedByUser })
		time.AfterFunc(s.Grace, s.graceExpired)
		return Applied, "the agent was asked to stop"

	case ForceStop:
		if err := signalGroup(s.group, signalKill); err != nil {
			return NotRunning, "the agent is no longer running"
		}
		s.forced, s.stopping = true, true
		return Applied, "the agent and everything it started were killed"
	}
	return MalformedRequest, "unknown control action"
}

// graceExpired records that a graceful stop did not work. It does not escalate:
// Interlock never kills an agent on a timer, because the agent may be part way
// through writing a file and a person is better placed to decide.
func (s *Supervisor) graceExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopping || s.forced || s.group == 0 || !groupAlive(s.group) {
		return
	}
	s.forceRequired = true
	_, _, _ = s.Store.Update(s.RunID, s.generation, func(status *Status) { status.ForceRequired = true })
}

// watchContext turns cancellation into the same graceful stop a user would ask
// for, rather than an abrupt kill.
func (s *Supervisor) watchContext(ctx context.Context, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		s.apply(Stop)
	case <-done:
	}
}

// watchQuiet moves a live run to quiet when nothing has been heard for a while,
// and back again when it speaks. Both directions are opportunistic: if the run
// has since paused or started stopping, the transition is refused by the state
// machine and that is the correct outcome.
func (s *Supervisor) watchQuiet(done <-chan struct{}) {
	ticker := time.NewTicker(s.QuietAfter / 2)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.mu.Lock()
			silent := s.Now().Sub(s.lastOutput) >= s.QuietAfter && !s.paused && !s.stopping
			s.mu.Unlock()
			if silent {
				_, _, _ = s.Store.Transition(s.RunID, s.generation, Quiet, nil)
			}
		}
	}
}

// drain copies one stream to its log and reads what it can from it.
//
// Every byte reaches the log whatever happens. Decoding is best effort on top
// of that: a line too long to be a status message is written and displayed but
// not parsed, because a malformed line must never be able to stop a run.
func (s *Supervisor) drain(adapter runner.Adapter, stream Stream, source io.Reader, log *LogWriter) {
	reader := bufio.NewReaderSize(source, 32*1024)
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			_, _ = log.Write(chunk)
			if len(line) < maxDecodeLine {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) > 0 {
			s.observe(adapter.Decode(stream, trimEOL(line)))
			line = line[:0]
		}
		if err != nil {
			return
		}
	}
}

func trimEOL(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}

// observe records that the agent said something.
func (s *Supervisor) observe(event runner.Event) {
	now := s.Now()
	s.mu.Lock()
	s.lastOutput = now
	persist := now.Sub(s.lastPersisted) >= outputPersistEvery
	if persist {
		s.lastPersisted = now
	}
	quiet := !s.paused && !s.stopping
	s.mu.Unlock()

	if event.Kind == runner.Result || event.Kind == runner.Failure {
		_, _, _ = s.Store.Update(s.RunID, s.generation, func(status *Status) {
			status.LastOutputAt, status.Summary = now, event.Summary
		})
		return
	}
	if !persist {
		return
	}
	// Output is proof the agent is working, so it ends any quiet spell. The
	// transition is refused if the run has moved on, which is what should happen.
	if quiet {
		_, _, _ = s.Store.Transition(s.RunID, s.generation, Running, func(status *Status) {
			status.LastOutputAt = now
		})
	}
	_, _, _ = s.Store.Update(s.RunID, s.generation, func(status *Status) { status.LastOutputAt = now })
}

// transition is the locked-path helper. The caller already holds s.mu, and the
// store takes its own lock, so nothing here waits on anything that waits on it.
func (s *Supervisor) transition(state State, mutate func(*Status)) {
	_, _, _ = s.Store.Transition(s.RunID, s.generation, state, mutate)
}

// finish records how the agent ended, exactly once.
func (s *Supervisor) finish(waitErr error) error {
	s.mu.Lock()
	forced, stopping := s.forced, s.stopping
	s.mu.Unlock()

	state, reason, summary := Completed, ReasonNone, ""
	var code *int
	signal := ""

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		if stopping {
			reason = ReasonStoppedByUser
		}
	case errors.As(waitErr, &exitErr):
		state = Failed
		if status, ok := exitErr.Sys().(interface{ Signaled() bool }); ok && status.Signaled() {
			signal = exitSignal(exitErr)
			// Which signal ended it is not the interesting part; who asked is.
			// An agent that died on the interrupt Interlock sent was stopped, and
			// saying "terminated by a signal" would make a normal stop read like
			// something went wrong.
			switch {
			case forced:
				reason = ReasonForceStopped
			case stopping:
				reason = ReasonStoppedByUser
			default:
				reason = ReasonSignaled
			}
		} else {
			value := exitErr.ExitCode()
			code, reason = &value, ReasonExitNonZero
			if stopping {
				reason = ReasonStoppedByUser
			}
		}
		summary = reason.Describe()
	default:
		state, reason, summary = Failed, ReasonLaunchFailed, waitErr.Error()
	}

	_, _, err := s.Store.Transition(s.RunID, s.generation, state, func(status *Status) {
		status.ExitCode, status.Signal = code, signal
		status.Reason, status.Summary = reason, summary
		status.ForceRequired = false
	})
	return err
}

// fail records a run that never got going. It is durable so that a retry is a
// decision rather than a guess about what happened.
func (s *Supervisor) fail(reason Reason, summary string) {
	_, _, _ = s.Store.Transition(s.RunID, s.generation, Failed, func(status *Status) {
		status.Reason, status.Summary = reason, summary
	})
}
