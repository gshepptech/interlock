package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/session"
)

// statusJSON is `interlock status --json`.
//
// # Why this exists
//
// `interlock status` is written for a person: durations as prose ("expired
// 6h16m23s ago"), alignment, and a shape that is free to change whenever the
// panel does. keep needs the same facts to answer a different question:
// which items are workable right now, given what Interlock is holding. Parsing
// prose would couple keep to Interlock's presentation rather than
// to its meaning.
//
// `interlock check <path>...` already answers "may I edit this" with an exit
// code, and stays the right tool for that. It cannot answer "what is held,
// by whom, and until when", which is what a board has to show.
//
// # The contract
//
// Every field here is a fact Interlock owns. Nothing is derived for the
// reader's convenience except Expired, because "expires_at is in the past"
// is a comparison against a clock the reader may not share.
//
// Absent state is an empty array, never null: a consumer that ranges over
// the result must not have to special-case "no sessions" separately from
// "no session field".
type statusJSON struct {
	Version int `json:"version"`

	Repo     string `json:"repo"`
	Declared bool   `json:"declared"`

	Sessions []sessionJSON `json:"sessions"`
	Leases   []leaseJSON   `json:"leases"`

	// Errors carries what could not be read, per subsystem, rather than
	// failing the whole call. `interlock status` already prints "sessions
	// unavailable: ..." and carries on to the leases; a machine reader
	// deserves the same partial answer rather than an exit code that
	// discards the half that worked.
	Errors []string `json:"errors"`
}

type sessionJSON struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Team      string `json:"team,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	LastSeen  string `json:"last_seen"`
	ExpiresAt string `json:"expires_at"`
	Expired   bool   `json:"expired"`
}

type leaseJSON struct {
	Path      string `json:"path"`
	Agent     string `json:"agent"`
	Team      string `json:"team,omitempty"`
	SessionID string `json:"session_id"`
	Implicit  bool   `json:"implicit"`
	Acquired  string `json:"acquired"`
	ExpiresAt string `json:"expires_at"`
}

// statusJSONVersion is bumped when a field changes meaning or disappears.
// A consumer that does not recognise it should say so rather than guess --
// the same envelope discipline keep's registry uses.
const statusJSONVersion = 1

// runStatusJSON writes the machine-readable status to w.
//
// It never returns a non-zero exit for a subsystem it could not read: a
// board that shows the sessions but not the leases is more useful than one
// that shows nothing, and the Errors array says which half is missing.
func runStatusJSON(w io.Writer, cfg *config.Config, now time.Time) int {
	out := statusJSON{
		Version:  statusJSONVersion,
		Repo:     cfg.Root,
		Declared: cfg.Declared,
		Sessions: []sessionJSON{},
		Leases:   []leaseJSON{},
		Errors:   []string{},
	}

	if sessionStore, err := session.Open(cfg.Root); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("sessions unavailable: %v", err))
	} else if observed, err := sessionStore.All(); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("cannot read sessions: %v", err))
	} else {
		for _, s := range observed {
			expires := s.LastSeen.Add(session.IdleAfter)
			out.Sessions = append(out.Sessions, sessionJSON{
				ID:        s.ID,
				Agent:     s.Agent,
				Team:      s.Team,
				CWD:       s.CWD,
				RunID:     s.RunID,
				LastSeen:  s.LastSeen.UTC().Format(time.RFC3339),
				ExpiresAt: expires.UTC().Format(time.RFC3339),
				Expired:   !expires.After(now),
			})
		}
	}

	if store, err := lease.Open(cfg.Root); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("leases unavailable: %v", err))
	} else if active, err := store.Active(); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("cannot read leases: %v", err))
	} else {
		for _, l := range active {
			out.Leases = append(out.Leases, leaseJSON{
				Path:      l.Path,
				Agent:     l.Agent,
				Team:      l.Team,
				SessionID: l.SessionID,
				Implicit:  l.Implicit,
				Acquired:  l.Acquired.UTC().Format(time.RFC3339),
				ExpiresAt: l.Renewed.Add(l.Hold).UTC().Format(time.RFC3339),
			})
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "interlock status --json: %v\n", err)
		return 1
	}
	return 0
}

// runStatusCmd dispatches `interlock status` between its human and machine
// forms. The flag is matched literally, matching the dispatch shape the rest
// of this file uses. There is no flag package in play here and no ambiguity
// to resolve, since status takes no positional argument at all.
func runStatusCmd(args []string) int {
	wantJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			wantJSON = true
		default:
			fmt.Fprintf(os.Stderr, "interlock status: unexpected argument %q\n", a)
			return 64
		}
	}
	if !wantJSON {
		return runStatus()
	}

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
	return runStatusJSON(os.Stdout, cfg, time.Now())
}
