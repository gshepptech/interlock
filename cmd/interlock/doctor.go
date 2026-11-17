package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/diagnostic"
)

// runDoctor diagnoses the ways this tool can be installed and silently not work.
//
// That is the failure mode worth building a command for: Codex skips project
// hooks in an untrusted repo without saying anything, so a half-installed setup
// looks exactly like a working one until two agents collide.
func runDoctor(args []string) int {
	var fix, smoke bool
	for _, arg := range args {
		switch arg {
		case "--fix", "-fix":
			fix = true
		case "--smoke", "-smoke":
			smoke = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", arg)
			return exitUsage
		}
	}

	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	service, err := newControlService(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var result control.Result
	switch {
	case fix:
		result, err = service.RepairInstallation(
			context.Background(), control.ActorCLI, "repair", smoke)
		for _, repair := range result.Repairs {
			mark := "ok  "
			if repair.Changed {
				mark = "FIX "
			}
			fmt.Printf("%s  %-28s %s\n", mark, repair.ID, repair.Detail)
		}
		fmt.Println()
	case smoke:
		result, err = service.VerifyInstallation(context.Background(), control.ActorCLI)
	default:
		result, err = service.CheckInstallation(context.Background())
	}
	if result.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", result.Warning)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
	}
	if result.Diagnostic == nil {
		return 1
	}
	report := *result.Diagnostic
	problems := 0
	for _, check := range report.Checks {
		mark := "ok  "
		switch check.Severity {
		case diagnostic.Warn:
			mark = "WARN"
		case diagnostic.Fail:
			mark = "FAIL"
			problems++
		}
		fmt.Printf("%s  %-28s %s\n", mark, check.Subject, check.Detail)
	}

	fmt.Println()
	if problems == 0 && err == nil && result.Successful() {
		fmt.Println("Everything is wired up.")
		return 0
	}
	fmt.Printf("%d problem(s). Anything marked FAIL means part of this is not enforcing.\n",
		problems)
	return 1
}
