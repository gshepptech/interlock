//go:build unix

package managedrun

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// The agent gets its own process group, and every signal is sent to the whole
// group rather than to the agent alone.
//
// Agents spawn things. A Codex or Claude run is a tree: shells, test runners,
// language servers, whatever the model decided to try. Signalling only the
// process Interlock started would leave that tree behind, still holding files
// and still writing to the repository after the run was supposedly stopped.
// A process group is the unit the kernel gives us for "this work and everything
// it started", so it is the unit used here.

// startInGroup starts the command as the leader of a new process group.
func startInGroup(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd.Start()
}

// detach puts a command in its own session, so it survives the terminal and the
// shell that started it. It is how a supervisor outlives the CLI call that
// spawned it.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// signalGroup sends a signal to every member of a process group.
//
// The negative pid is what makes it the group rather than the leader. The guard
// against a non-positive group is not defensive tidiness: kill(-1) means every
// process this user can signal, which on a developer's machine means their
// editor, their browser and their session.
func signalGroup(group int, signal syscall.Signal) error {
	if group <= 1 {
		return fmt.Errorf("refusing to signal process group %d", group)
	}
	if err := syscall.Kill(-group, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return errNoProcess
		}
		return fmt.Errorf("signal process group: %w", err)
	}
	return nil
}

// groupAlive reports whether any member of the group still exists. Signal zero
// performs the permission and existence checks without delivering anything.
func groupAlive(group int) bool {
	if group <= 1 {
		return false
	}
	err := syscall.Kill(-group, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

var (
	errNoProcess = errors.New("no such process")

	signalInterrupt = syscall.SIGINT
	signalKill      = syscall.SIGKILL
	signalPause     = syscall.SIGSTOP
	signalResume    = syscall.SIGCONT
)

// isAddressTooLong reports the errnos a bind produces when a socket path does
// not fit the kernel's fixed-size address field. The two systems disagree about
// which one to use, which is why both are listed.
func isAddressTooLong(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EINVAL || errno == syscall.ENAMETOOLONG
}

// exitSignal reports the signal that ended a process, if one did.
func exitSignal(err *exec.ExitError) string {
	status, ok := err.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return signalName(status.Signal())
}

// signalName renders a signal for a status record. The names are spelled out
// rather than taken from the signal's own String, which gives a description
// ("interrupt") where a status field wants an identifier.
func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGSTOP:
		return "SIGSTOP"
	case syscall.SIGCONT:
		return "SIGCONT"
	}
	return fmt.Sprintf("signal %d", int(signal))
}
