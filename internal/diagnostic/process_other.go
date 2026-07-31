//go:build !unix

package diagnostic

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
