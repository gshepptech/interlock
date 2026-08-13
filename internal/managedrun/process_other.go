//go:build !unix

package managedrun

import (
	"errors"
	"os/exec"
)

// Managed runs need process groups, stop and continue signals, and a unix
// socket. Rather than pretend, this build refuses at the one place that would
// otherwise start a process. See lockfile.ErrUnsupported for the same reasoning
// applied to advisory locks.

type sig int

var (
	errNoProcess = errors.New("no such process")

	signalInterrupt sig = 2
	signalKill      sig = 9
	signalPause     sig = 17
	signalResume    sig = 19
)

var errPlatform = errors.New(
	"managed runs are only implemented on unix; Windows support is not built yet")

func startInGroup(*exec.Cmd) error { return errPlatform }

func detach(*exec.Cmd) {}

func signalGroup(int, sig) error { return errPlatform }

func groupAlive(int) bool { return false }

func exitSignal(*exec.ExitError) string { return "" }

func isAddressTooLong(error) bool { return false }

func signalName(signal sig) string {
	switch signal {
	case signalInterrupt:
		return "SIGINT"
	case signalKill:
		return "SIGKILL"
	}
	return "signal"
}
