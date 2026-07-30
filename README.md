# agentcoord

Lets coding agents from different vendors work the same repo at the same time
without overwriting each other.

You start Claude Code in one terminal and Codex in another, yourself, in one
shared checkout. Each one is told what it owns and who else is here. If either
tries to write outside its territory, or onto a shared file another agent is
holding, the write is blocked and the agent is told why, so it redirects instead
of failing.

No orchestrator. Neither agent is launched or owned by this tool. No worktrees.

## Status

Working and verified end to end against Claude Code and Codex on one machine.
See [what is not built yet](#what-is-not-built-yet) before relying on it.

## Why this exists

Every other approach to two agents in one repo gives each agent its own git
worktree, so they cannot collide because they cannot see each other's files.
That works, and it means you are no longer working in one checkout.

Claude Code's own agent teams share a checkout and have no file conflict
enforcement at all. The documented answer is "break the work so each teammate
owns a different set of files", which is an instruction to the human. This is
that instruction, enforced.

## How it works

Hooks, not MCP. An MCP tool only runs if the model decides to call it; a hook
runs whether the model cooperates or not.

**Territory** is the cheap layer. Most collisions are not subtle races over one
file, they are two teams in a directory they were never meant to share.
Declaring who owns what removes those with no runtime state and no locking.

**Leases** cover what is left: the few files both teams legitimately need, like
the type definitions or the API contract. One holder at a time, renewed on every
touch, released on a tidy exit, expiring after three minutes otherwise.

One property shapes the lease design: **an agent cannot wait.** There is no way
to park a tool call and resume it when a lock frees, so a held file produces a
redirect ("someone else has this, go and do something else") and never a queue.

**Shell scanning** closes the obvious bypass. Territory enforced only at the edit
tool would be sidestepped by `sed -i`, so shell commands are scanned for the
paths they are likely to write.

## Try it

```bash
go build -o bin/agentcoord ./cmd/agentcoord
cd /your/repo
/path/to/bin/agentcoord init      # writes .agentcoord.json, prints hook wiring
/path/to/bin/agentcoord status    # territory, plus who holds what right now
```

Territory is declared per repo in `.agentcoord.json`:

```json
{
  "teams": {
    "backend":  { "owns": ["src/api/**", "migrations/**"], "shared": ["src/types/**"], "agents": ["claude"] },
    "frontend": { "owns": ["src/components/**"], "agents": ["codex"] }
  }
}
```

A team can span vendors. Two Claude sessions and a Codex session can all be
`backend`. Set `AGENTCOORD_TEAM` to put one session on a team other than its
agent's default.

Add `.agentcoord/` to `.gitignore`. Leases are local runtime state.

### Codex needs the repo trusted

Codex loads project-local `.codex/hooks.json` only for a trusted project. Add a
`projects."<path>".trust_level = "trusted"` entry to `~/.codex/config.toml`, or
the hooks are skipped without saying so. Codex also exposes no session-end event,
so its leases are freed by the time to live rather than on exit.

## The verification that matters

All four run against real Claude Code and Codex sessions in one shared checkout.

**Territory, both directions:**

| Agent | Team | Own territory | Other team's territory |
| --- | --- | --- | --- |
| Claude Code | backend | `src/api/users.go` created | `src/components/Button.tsx` blocked |
| Codex | frontend | `src/components/Card.tsx` created | `src/api/orders.go` blocked |

**Leases, across vendors.** Claude took `src/types/api.ts` and kept it. Codex,
asked to edit the same file, said:

> `src/types/api.ts` is currently leased by the backend team, agent `claude`,
> session `70197b4e`. Because I am on the frontend team and the shared file is
> already held by backend, I left the file unchanged.

**The shell bypass.** Claude, on backend, told to use `sed` on a frontend file,
was blocked and offered to hand the work to the owning team instead. The file was
unchanged.

The point is not that the writes failed. It is that both agents read the reason
and reasoned about it. That is awareness, not an error message.

## Design rules

- **This tool must never be the reason someone's work stopped.** A territory
  violation and a held lease are the only things that block. An unreadable
  payload, a broken config, or no config at all allows the call and says so on
  stderr. Allowing costs a collision the user could already have had. Blocking on
  our own bug costs a session that cannot write to its own repo.
- **Nothing vendor-specific escapes `internal/hookio`.** Everything downstream
  works on one normalised `Event`.
- **Reads never mutate.** `Active` skips expired leases without deleting them, so
  a reader and a writer never contend and the dashboard needs no lock.
- **Zero runtime dependencies.** A hook runs on every tool call, so the artifact
  is one static binary with nothing to resolve at startup. That is also why this
  is Go and not Node: process start is on the hot path.

## Two bugs worth recording

**The pid trap.** Leases first reclaimed any lease whose recorded process had
exited, to recover quickly from a crash. A cross-process test found that this
made every lease stealable: the hook is a short-lived process that exits
milliseconds after acquiring, so its pid is always dead. Three of twelve racing
processes were each granted the same file. Liveness is now the time to live
alone, and `Lease.PID` is informational only. The in-process race test passed
throughout, which is why `crossprocess_test.go` exists.

**flock, not a lock file.** A lock file is state that outlives the process that
made it, so a crash leaves a stale lock and the next run deadlocks. An flock is
held by the kernel on behalf of an open descriptor and is released when the
process dies for any reason. It is held for microseconds around the
read-decide-write section of `Acquire`, never across an agent's tool call.

## What is not built yet

Stated plainly, because a coordination tool that implies coverage it does not
have is worse than none.

- **Opaque shell writes are reported, not blocked.** When a command writes
  somewhere that cannot be read off the command line, a variable, a subshell,
  `xargs`, or an interpreter like `python3`, the hook says so on stderr and allows
  it. Blocking every such command would stop normal work. This is the one hole
  left.
- **No dashboard.** `status` is the whole read model today.
- **Claude Code and Codex only.** Cursor and Gemini CLI both have hooks and
  should slot into `hookio` without touching anything else.
- **Unix only.** Leases need an advisory lock the kernel releases on process
  death. Windows would need `LockFileEx`; the build fails loudly rather than
  degrading into a racing no-op.
- **`init` does not edit agent settings.** It prints what to add. Merging into
  files the user owns is not something to do behind their back.

## Layout

```
cmd/agentcoord/       CLI: hook, status, init
internal/hookio/      payload normalisation, the only vendor-aware code
internal/territory/   path globs to team ownership
internal/lease/       one holder at a time on shared files
internal/shellscan/   paths a shell command is likely to write
internal/config/      per-repo .agentcoord.json
```

`internal/hookio/testdata` holds payloads captured from real Claude Code and
Codex sessions. If a vendor changes its envelope, those tests are how we find
out.
