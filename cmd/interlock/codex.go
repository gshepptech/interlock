package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/install"
	"github.com/gshepptech/interlock/internal/runner"
)

// runCodex launches Codex with hook trust disabled, so its hooks run and its
// writes are blocked outright rather than relying on it to check first.
//
// Doing this by hand is a long path and a flag nobody remembers. Doing it here
// also makes it safer: the flag disables Codex's vetting of hook commands, and
// that vetting is the only thing standing between a cloned repository and
// arbitrary execution, so this refuses to launch when the hooks file contains
// anything Interlock did not write.
func runCodex(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	var target install.Target
	for _, candidate := range install.Plan(cfg.Root) {
		if candidate.Agent == "codex" {
			target = candidate
		}
	}
	wired, err := install.Installed(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", target.File, err)
		return 1
	}
	if len(wired) == 0 {
		fmt.Fprintf(os.Stderr,
			"Interlock's hooks are not installed in this repository, so running "+
				"Codex with trust disabled would gain nothing.\nRun: interlock init\n")
		return 1
	}

	foreign, err := install.ForeignHooks(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", target.File, err)
		return 1
	}
	if len(foreign) > 0 {
		fmt.Fprintf(os.Stderr,
			"Refusing to disable Codex's hook vetting.\n\n"+
				"%s contains hooks Interlock did not write, and the flag this "+
				"command passes would run them without review:\n\n", target.File)
		for _, command := range foreign {
			fmt.Fprintf(os.Stderr, "  %s\n", command)
		}
		fmt.Fprintf(os.Stderr,
			"\nRemove them, or start Codex yourself if you know they are safe.\n")
		return 1
	}

	binary, err := findCodex()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Replace this process rather than wrapping it. Codex is interactive, and a
	// parent sitting in the middle would have to forward the terminal, signals
	// and exit status correctly to gain nothing.
	argv := append([]string{binary, runner.CodexHookTrustFlag}, args...)
	// Codex's hooks will be live, so its own claims must not make its own
	// check refuse its own work.
	env := append(os.Environ(), "INTERLOCK_AGENT=codex")
	if err := syscall.Exec(binary, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "cannot start %s: %v\n", binary, err)
		return 1
	}
	return 0
}

func findCodex() (string, error) {
	if override := os.Getenv("INTERLOCK_CODEX_BIN"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("INTERLOCK_CODEX_BIN is set to %s, which does not exist", override)
	}
	if found, err := exec.LookPath("codex"); err == nil {
		return found, nil
	}
	for _, candidate := range runner.CodexSearchPaths {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"cannot find the codex binary. Looked on PATH and in %s.\n"+
			"Set INTERLOCK_CODEX_BIN to its location.",
		strings.Join(runner.CodexSearchPaths, ", "))
}
