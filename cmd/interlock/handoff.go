package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/handoffrequest"
)

func runHandoff(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: interlock handoff request|list|show|resolve")
		return exitUsage
	}
	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store := handoffrequest.Open(root)
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return handoffUsage()
		}
		items, err := store.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "handoff list: %v\n", err)
			return 1
		}
		if len(items) == 0 {
			fmt.Println("no handoff requests")
			return 0
		}
		for _, item := range items {
			status := "open"
			if !item.Open() {
				status = "resolved"
			}
			fmt.Printf("%s  %-8s %s:%s  from %s\n", item.Request.ID, status,
				item.Request.TargetKind, item.Request.Target, item.Request.SourceSession)
		}
		return 0
	case "show":
		if len(args) != 2 {
			return handoffUsage()
		}
		item, err := store.Show(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "handoff show: %v\n", err)
			return 1
		}
		status := "open"
		if !item.Open() {
			status = "resolved"
		}
		fmt.Printf("id: %s\nstatus: %s\nfrom: %s\nto: %s:%s\nnote: %s\n",
			item.Request.ID, status, item.Request.SourceSession,
			item.Request.TargetKind, item.Request.Target, item.Request.Note)
		return 0
	case "request":
		values, ok := parseNamedArgs(args[1:], map[string]bool{
			"--from": true, "--target-kind": true, "--to": true, "--note": true,
		})
		if !ok {
			return handoffUsage()
		}
		service, err := newControlService(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, runErr := service.RequestHandoff(context.Background(), control.ActorCLI,
			handoffrequest.Create{
				SourceSession: values["--from"],
				TargetKind:    handoffrequest.TargetKind(values["--target-kind"]),
				Target:        values["--to"], Note: values["--note"],
			})
		printControlResult(os.Stdout, os.Stderr, result)
		if result.Handoff != nil {
			fmt.Printf("request: %s\n", result.Handoff.Request.ID)
		}
		if runErr != nil || !result.Successful() {
			return 1
		}
		return 0
	case "resolve":
		if len(args) < 2 {
			return handoffUsage()
		}
		values, ok := parseNamedArgs(args[2:], map[string]bool{"--confirm": true})
		if !ok {
			return handoffUsage()
		}
		service, err := newControlService(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		result, runErr := service.ResolveHandoff(
			context.Background(), control.ActorCLI, args[1], values["--confirm"])
		printControlResult(os.Stdout, os.Stderr, result)
		if runErr != nil || !result.Successful() {
			return 1
		}
		return 0
	default:
		return handoffUsage()
	}
}

func parseNamedArgs(args []string, allowed map[string]bool) (map[string]string, bool) {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !allowed[args[i]] || i+1 >= len(args) || values[args[i]] != "" {
			return nil, false
		}
		name := args[i]
		i++
		values[name] = args[i]
	}
	for name := range allowed {
		if values[name] == "" && name != "--note" {
			return nil, false
		}
	}
	return values, true
}

func handoffUsage() int {
	fmt.Fprintln(os.Stderr, "usage: interlock handoff request --from ID --target-kind agent|team --to NAME --note TEXT")
	fmt.Fprintln(os.Stderr, "       interlock handoff list | show ID | resolve ID --confirm ID")
	return exitUsage
}
