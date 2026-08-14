package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gshepptech/interlock/internal/rounce"
)

// The work-list side of the join.
//
// rounce already reads Interlock, so its queue hides work whose files are being
// edited right now. This is the other direction: Interlock reads the queue so a
// person, or a script, can start an agent on the top item without copying a
// brief between two terminals.
//
// Everything here degrades to nothing. A repository with no rounce store gets a
// sentence saying so and no error, because that is most repositories and this is
// a convenience rather than part of the guard.

// runNext shows what is workable right now, as rounce ranked it.
func runNext(args []string) int {
	if len(args) > 0 {
		fmt.Fprint(os.Stderr, "usage: interlock next\n")
		return exitUsage
	}
	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	items, ok := rounce.Ready(context.Background(), root)
	if !ok {
		fmt.Fprintln(os.Stderr, noQueue())
		return 1
	}
	printQueue(os.Stdout, items)
	return exitOK
}

func printQueue(out io.Writer, items []rounce.Item) {
	if len(items) == 0 {
		fmt.Fprintln(out, "nothing is workable right now")
		return
	}
	for _, item := range items {
		fmt.Fprintf(out, "%s  %3d  %-5s %s\n", item.ID, item.Score, item.Type,
			truncate(item.Title, 60))
	}
}

// briefFromQueue turns --next or --item into the prompt an agent starts from.
//
// It returns the brief, the item it came from, and an exit code. The item id is
// returned rather than assumed, because --next does not know which item it will
// get until it asks.
func briefFromQueue(wanted string) (string, string, int) {
	root, err := currentRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return "", "", 1
	}
	items, ok := rounce.Ready(context.Background(), root)
	if !ok {
		fmt.Fprintln(os.Stderr, noQueue())
		return "", "", 1
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "nothing is workable right now")
		return "", "", 1
	}

	item := items[0]
	if wanted != "" {
		found := false
		for _, candidate := range items {
			if candidate.ID == wanted {
				item, found = candidate, true
				break
			}
		}
		if !found {
			// Deliberately not "no such item". It may exist and be blocked, or
			// held by another agent, and saying it is missing would send someone
			// looking for the wrong problem.
			fmt.Fprintf(os.Stderr,
				"%s is not in the ready queue; it may be blocked, or its files may be held\n",
				wanted)
			return "", "", 1
		}
	}
	return item.Brief(), item.ID, exitOK
}

// noQueue is the one sentence every command here shares when there is no work
// list. It names the two ordinary causes rather than reporting a failure,
// because neither is one.
func noQueue() string {
	if !rounce.Available() {
		return "no work list here: rounce is not installed, or " +
			rounce.EnvBinary + " is off"
	}
	return "no work list here: rounce is installed but this repository has no store"
}
