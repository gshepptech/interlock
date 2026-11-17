package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/hookio"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/managedrun"
	"github.com/gshepptech/interlock/internal/observed"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/territory"
)

// dumpPayload appends the raw hook payload to a file when INTERLOCK_HOOK_DUMP
// names one.
//
// Every vendor-specific bug this tool has had began with an envelope changing
// shape, and the only way to see one is to catch it in flight: a hook runs
// inside the agent's process tree, with the agent's stdin, and cannot be
// reproduced by hand. The fixtures in internal/hookio/testdata all came from a
// capture like this, and the next one will too.
//
// It writes to a path the operator chose, and never fails the hook: a debugging
// aid that could stop someone's work would be worse than no aid at all.
func dumpPayload(agent hookio.Agent, event string, raw []byte) {
	path := os.Getenv("INTERLOCK_HOOK_DUMP")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	record, err := json.Marshal(map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano), "agent": agent,
		"event": event, "payload": json.RawMessage(raw),
	})
	if err != nil {
		return
	}
	_, _ = file.Write(append(record, '\n'))
}

// runHook is the hot path. Its one hard rule: this tool must never be the reason
// someone's work stopped. A territory violation and a held lease are the only
// things that block. Anything else that goes wrong, a payload we cannot read, a
// config we cannot parse, a repo with no config at all, allows the call and says
// so on stderr. The cost of allowing is a collision the user could already have
// had. The cost of blocking on our own bug is a session that cannot write to its
// own repo.
func runHook(args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, "usage: interlock hook <event> <agent>\n")
		return exitUsage
	}
	event, agent := args[0], hookio.Agent(args[1])

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return allow("could not read hook payload: %v", err)
	}
	dumpPayload(agent, event, raw)

	ev, err := hookio.Parse(agent, raw)
	if err != nil {
		return allow("could not parse hook payload: %v", err)
	}
	if ev.Name == "" {
		ev.Name = event
	}

	cwd := ev.CWD
	if cwd == "" {
		// Some events carry no cwd. Falling back to ours is right, because the
		// agent spawned us in its own working directory.
		if cwd, err = os.Getwd(); err != nil {
			return allow("no cwd in payload and cannot determine ours: %v", err)
		}
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		return allow("could not load config: %v", err)
	}

	team := cfg.TeamFor(string(agent))
	// Evidence that this agent really does invoke our hooks. Recorded before
	// anything can decline or fail, because the fact of the call is the point.
	markObserved(cfg, string(agent))

	switch event {
	case "SessionStart":
		note(cfg, ev, team, agent)
		emitContext(cfg.Summary(team) + presence(cfg, ev.SessionID))
		return exitOK
	case "PreToolUse":
		note(cfg, ev, team, agent)
		return checkWrite(cfg, team, agent, ev)
	case "PostToolUse":
		note(cfg, ev, team, agent)
		recordTouches(cfg, ev)
		renew(cfg, team, agent, ev)
		verifyAfter(cfg, team, agent, ev)
		return exitOK
	case "SessionEnd", "Stop":
		release(cfg, ev.SessionID)
		endSession(cfg, ev.SessionID)
		clearRemedy(cfg, ev.SessionID)
		return exitOK
	default:
		note(cfg, ev, team, agent)
		return exitOK
	}
}

// renew extends the leases this session holds on the files it just touched, so a
// file under active work never expires beneath its holder.
func renew(cfg *config.Config, team string, agent hookio.Agent, ev *hookio.Event) {
	if len(ev.Files) == 0 {
		return
	}
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return
	}
	holder := lease.Holder{
		Team: team, Agent: string(agent),
		SessionID: ev.SessionID, PID: os.Getpid(),
	}
	for _, file := range ev.Files {
		decision, _, err := cfg.Map.Check(team, file)
		if err != nil {
			continue
		}
		hold, implicit := lease.ImplicitTTL, true
		if decision == territory.Shared {
			hold, implicit = lease.DefaultTTL, false
		}
		// Errors are ignored on purpose: a failed renewal costs the claim its
		// time to live, and there is nothing useful to tell the agent.
		_, _, _ = store.AcquireFor(rel(cfg.Root, file), holder, hold, implicit)
	}
}

// release frees this session's shared files on a tidy exit, rather than making
// the next agent wait out the time to live.
func release(cfg *config.Config, sessionID string) {
	if sessionID == "" {
		return
	}
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return
	}
	_, _ = store.Release(sessionID)
}

// note records that this session is alive and what it is.
//
// Every event does this, not just SessionStart, because SessionStart is not
// guaranteed to have fired: hooks can be installed mid-session, and a resumed
// session may never send one. Errors are dropped, because failing to record
// presence is not worth telling anyone about and certainly not worth blocking.
func note(cfg *config.Config, ev *hookio.Event, team string, agent hookio.Agent) {
	if ev.SessionID == "" {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	_ = store.See(ev.SessionID, session.Sighting{
		Agent: string(agent), Team: team, Model: ev.Model, CWD: ev.CWD,
		RunID: managedRunID(),
	})
}

// managedRunID reports the run this hook belongs to, when Interlock started the
// agent that fired it.
//
// The value comes from the environment the supervisor built, so it is present
// exactly when Interlock owns the process. It is validated rather than trusted:
// the variable is inherited by everything the agent spawns, and an agent can
// set environment variables itself.
func managedRunID() string {
	id := os.Getenv("INTERLOCK_RUN_ID")
	if !managedrun.ValidID(id) {
		return ""
	}
	return id
}

// recordTouches remembers the files a session just wrote, so the dashboard can
// show what it is working on rather than only that it exists.
func recordTouches(cfg *config.Config, ev *hookio.Event) {
	if ev.SessionID == "" || len(ev.Files) == 0 {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	paths := make([]string, 0, len(ev.Files))
	for _, f := range ev.Files {
		paths = append(paths, rel(cfg.Root, f))
	}
	_ = store.Touched(ev.SessionID, ev.ToolName, paths)
}

func endSession(cfg *config.Config, sessionID string) {
	if sessionID == "" {
		return
	}
	store, err := session.Open(cfg.Root)
	if err != nil {
		return
	}
	_ = store.End(sessionID)
}

// emitContext returns text both agents inject into the model's context. The
// shape is identical for Claude Code and Codex; verified against both.
func emitContext(text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": text,
		},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		// Nothing useful to say to the model, and no reason to fail the session.
		return
	}
	fmt.Println(string(enc))
}

// emitPostContext injects text at PostToolUse. The event name has to match the
// event that is firing or the agent ignores the payload.
func emitPostContext(text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": text,
		},
	}
	if enc, err := json.Marshal(out); err == nil {
		fmt.Println(string(enc))
	}
}

// record writes a journal entry. Failing to record that something happened is
// never a reason to change what happened, so the error is dropped.
func record(cfg *config.Config, e journal.Entry) {
	store, err := journal.Open(cfg.Root)
	if err != nil {
		return
	}
	_ = store.Append(e)
}

// markObserved records that an agent invoked a hook here. Errors are dropped:
// failing to note the call is not a reason to fail the call.
func markObserved(cfg *config.Config, agent string) {
	if agent == "" {
		return
	}
	store, err := observed.Open(cfg.Root)
	if err != nil {
		return
	}
	_ = store.Mark(agent)
}
