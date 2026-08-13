package managedrun

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The control protocol is deliberately small: one request, one response, one
// connection. It exists so that a lifecycle command reaches the process that
// actually owns the agent, rather than a stored process id.
//
// The distinction matters more than it sounds. A persisted pid is a number that
// was true when it was written; by the time anything reads it the process may
// have exited and the number may belong to something else entirely. Signalling
// it would be a coordination tool killing an unrelated program. So every
// request names the run and its generation, and only a supervisor that agrees
// on both will act.

// ProtocolVersion is the wire version. A supervisor from a different version is
// refused rather than guessed at; the caller falls back to reading state.
const ProtocolVersion = 1

const (
	// maxFrame bounds one request or response. Frames are a handful of short
	// fields, so anything larger is not a control message.
	maxFrame = 8 * 1024
	// dialTimeout and callTimeout keep a caller from hanging on a supervisor
	// that is present but wedged. Both are generous relative to the work, which
	// is a signal and a status write.
	dialTimeout = 2 * time.Second
	callTimeout = 5 * time.Second
)

// ControlAction is one lifecycle request.
type ControlAction string

const (
	// Probe asks only who is there. Reconciliation uses it, so it must never
	// change anything.
	Probe     ControlAction = "probe"
	Pause     ControlAction = "pause"
	Resume    ControlAction = "resume"
	Stop      ControlAction = "stop"
	ForceStop ControlAction = "force_stop"
)

func (a ControlAction) known() bool {
	switch a {
	case Probe, Pause, Resume, Stop, ForceStop:
		return true
	}
	return false
}

// ControlOutcome is the closed set of answers a supervisor gives.
type ControlOutcome string

const (
	Applied ControlOutcome = "applied"
	// Unchanged means the request was already true. Pausing a paused run is not
	// an error; users press buttons twice and messages get retried.
	Unchanged           ControlOutcome = "unchanged"
	NotRunning          ControlOutcome = "not_running"
	ForceRequired       ControlOutcome = "force_required"
	GenerationMismatch  ControlOutcome = "generation_mismatch"
	Unauthorised        ControlOutcome = "unauthorised"
	MalformedRequest    ControlOutcome = "malformed_request"
	UnsupportedProtocol ControlOutcome = "unsupported_protocol"
)

// Successful reports whether the supervisor did what was asked, including the
// case where there was nothing left to do.
func (o ControlOutcome) Successful() bool { return o == Applied || o == Unchanged }

// ControlRequest is one message to a supervisor.
type ControlRequest struct {
	Version    int           `json:"version"`
	RunID      string        `json:"run_id"`
	Generation string        `json:"generation"`
	Token      string        `json:"token"`
	Action     ControlAction `json:"action"`
}

// ControlResponse is the supervisor's single reply.
type ControlResponse struct {
	Version int            `json:"version"`
	Outcome ControlOutcome `json:"outcome"`
	Status  Status         `json:"status"`
	// Detail is a short safe explanation. It never contains a socket path, a
	// log path, a command line, or a process id.
	Detail string `json:"detail,omitempty"`
}

// ErrNoSupervisor reports that nothing is listening for this run. It is the
// signal that a record may be stale, and it is never on its own a reason to
// declare a run lost: reconciliation checks the supervisor lock too.
var ErrNoSupervisor = errors.New("no supervisor is listening for this run")

// Call sends one request to a run's supervisor and returns its answer.
//
// It authenticates in both directions. The token proves the caller can read the
// run directory, which is the only thing that should be able to control the
// run. The generation proves the answering supervisor is this run's, and not a
// process that inherited an abandoned socket path.
func (c *Store) Call(id string, action ControlAction) (ControlResponse, error) {
	return c.call(id, action, dialTimeout)
}

func (c *Store) call(id string, action ControlAction, timeout time.Duration) (ControlResponse, error) {
	if !ValidID(id) {
		return ControlResponse{}, ErrNotFound
	}
	if !action.known() {
		return ControlResponse{}, fmt.Errorf("%w: unknown control action %q", ErrInvalid, action)
	}
	record, err := c.Load(id)
	if err != nil {
		return ControlResponse{}, err
	}
	token, err := c.Token(id)
	if err != nil {
		return ControlResponse{}, err
	}

	conn, err := net.DialTimeout("unix", c.ControlAddress(id), timeout)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("%w: %s", ErrNoSupervisor, id)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return ControlResponse{}, fmt.Errorf("control deadline: %w", err)
	}

	request := ControlRequest{
		Version: ProtocolVersion, RunID: id, Generation: record.Spec.Generation,
		Token: token, Action: action,
	}
	if err := writeFrame(conn, request); err != nil {
		return ControlResponse{}, err
	}
	var response ControlResponse
	if err := readFrame(conn, &response); err != nil {
		return ControlResponse{}, err
	}
	if response.Version != ProtocolVersion {
		return response, fmt.Errorf("supervisor for %s speaks protocol %d, not %d",
			id, response.Version, ProtocolVersion)
	}
	return response, nil
}

// authorise checks a request against the run it claims to be for. It is the
// supervisor's half of Call, and it is deliberately ordered so that an
// unauthorised caller learns nothing about the run's state.
func authorise(request ControlRequest, id, generation, token string) (ControlOutcome, string) {
	switch {
	case request.Version != ProtocolVersion:
		return UnsupportedProtocol, "this supervisor speaks a different control protocol"
	case request.RunID != id:
		return MalformedRequest, "the request names a different run"
	case subtle.ConstantTimeCompare([]byte(request.Token), []byte(token)) != 1:
		return Unauthorised, "the control token does not match"
	case request.Generation != generation:
		return GenerationMismatch, "this supervisor belongs to a different incarnation of the run"
	case !request.Action.known():
		return MalformedRequest, "unknown control action"
	}
	return Applied, ""
}

func writeFrame(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control frame: %w", err)
	}
	if len(raw) > maxFrame {
		return errors.New("control frame is too large")
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write control frame: %w", err)
	}
	return nil
}

func readFrame(r io.Reader, value any) error {
	reader := bufio.NewReaderSize(io.LimitReader(r, maxFrame+1), maxFrame+1)
	raw, err := reader.ReadBytes('\n')
	if err != nil && len(raw) == 0 {
		return fmt.Errorf("read control frame: %w", err)
	}
	if len(raw) > maxFrame {
		return errors.New("control frame is too large")
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("parse control frame: %w", err)
	}
	return nil
}
