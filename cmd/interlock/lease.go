package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gshepptech/interlock/internal/control"
)

func runLease(args []string) int {
	if len(args) < 2 || args[0] != "release" {
		fmt.Fprintln(os.Stderr, "usage: interlock lease release ID --confirm ID")
		return exitUsage
	}
	sessionID := args[1]
	confirmation := ""
	for i := 2; i < len(args); i++ {
		if args[i] != "--confirm" || i+1 >= len(args) {
			fmt.Fprintln(os.Stderr, "usage: interlock lease release ID --confirm ID")
			return exitUsage
		}
		i++
		confirmation = args[i]
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
	result, runErr := service.ReleaseSessionLeases(
		context.Background(), control.ActorCLI, sessionID, confirmation)
	printControlResult(os.Stdout, os.Stderr, result)
	if runErr != nil || !result.Successful() {
		return 1
	}
	return 0
}
