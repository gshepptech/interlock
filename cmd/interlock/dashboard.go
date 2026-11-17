package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/controlhttp"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/keep"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/session"
)

// runDashboard serves the live view.
//
// It binds to loopback only and has no flag to change that. This page shows a
// person's development activity, and there is no version of exposing it on a
// network interface that is worth the convenience.
func runDashboard(args []string) int {
	addr := "127.0.0.1:7717"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr", "-addr":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--addr needs a value like 127.0.0.1:7717")
				return exitUsage
			}
			i++
			addr = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", args[i])
			return exitUsage
		}
	}
	if host, _, err := net.SplitHostPort(addr); err != nil {
		fmt.Fprintf(os.Stderr, "cannot parse --addr %q: %v\n", addr, err)
		return exitUsage
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fmt.Fprintf(os.Stderr,
			"refusing to listen on %q: use an IP-literal loopback address; this page shows your development activity "+
				"and is served on loopback only\n", host)
		return exitUsage
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}
	// A repo with no declaration is the normal case, so the only thing worth
	// failing on here is a declaration that exists and cannot be read.
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// The repository boundary is fixed for the server lifetime. Runtime state is
	// re-read per request, while config changes take effect after a restart.
	load := dashboardLoad(cfg)
	controller, err := newControlService(cfg.Root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	controller.Config = cfg
	handler, err := controlhttp.New(controller, addr, load)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runs, err := newRunService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	handler.AddRuns(runs)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	fmt.Printf("interlock dashboard on http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

func dashboardLoad(cfg *config.Config) controlhttp.Load {
	return func() (dashboard.View, []handoffrequest.Item, error) {
		sessions, err := readSessions(cfg)
		if err != nil {
			return dashboard.View{}, nil, err
		}
		leases, err := readLeases(cfg)
		if err != nil {
			return dashboard.View{}, nil, err
		}
		requests, err := handoffrequest.Open(cfg.Root).List()
		if err != nil {
			return dashboard.View{}, nil, err
		}
		now := time.Now()
		view := dashboard.Build(cfg, sessions, leases, readEvents(cfg), now)
		// Reconciling here means the page never shows a run as live on the
		// strength of a record alone; if no supervisor answers, it is marked
		// lost before anyone sees a control for it.
		if records, err := managedrun.Open(cfg.Root).Reconcile(); err == nil {
			view.Runs = dashboard.RunViews(records, now)
		}

		// The read side of the join. keep has already excluded whatever
		// collides with the leases this same page shows below, so the panel
		// renders the queue rather than re-deriving it. Two answers to
		// "what is workable" would drift, and the drift would show as a
		// panel recommending work it is itself reporting as held.
		//
		// Absent keep leaves HasKeep false and the section unrendered,
		// which is most repositories: Interlock keeps agents apart whether
		// or not a work store exists.
		if items, ok := keep.Ready(context.Background(), cfg.Root); ok {
			view.HasKeep = true
			for _, it := range items {
				view.Queue = append(view.Queue, dashboard.QueueView{
					ID: it.ID, Score: it.Score, Type: it.Type,
					Title: it.Title, Paths: it.Paths,
				})
			}
		}
		return view, requests, nil
	}
}

func readSessions(cfg *config.Config) ([]*session.Session, error) {
	store, err := session.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	return store.All()
}

func readLeases(cfg *config.Config) ([]*lease.Lease, error) {
	store, err := lease.Open(cfg.Root)
	if err != nil {
		// Leases are unavailable on platforms without an advisory lock. The rest
		// of the page is still worth showing.
		return nil, nil
	}
	return store.Active()
}

// readEvents is best-effort: an unreadable journal costs the page its activity
// feed and must not cost it the rest.
func readEvents(cfg *config.Config) []journal.Entry {
	store, err := journal.Open(cfg.Root)
	if err != nil {
		return nil
	}
	events, err := store.Recent(25)
	if err != nil {
		return nil
	}
	return events
}
