# agentcoord

Lets coding agents from different vendors work the same repo at the same time
without overwriting each other.

You start Claude Code in one terminal and Codex in another, yourself, in one
shared checkout. Each one is told what it owns and who else is here. If either
tries to write outside its territory, the write is blocked and the agent is told
why, so it redirects instead of failing.

No orchestrator. Neither agent is launched or owned by this tool.

## Status

Working and verified end to end against Claude Code and Codex on one machine.
Not finished. See [what is not built yet](#what-is-not-built-yet) before relying
on it.

## Why this exists

Every other approach to two agents in one repo gives each agent its own git
worktree, so they cannot collide because they cannot see each other's files.
That works, and it means you are no longer working in one checkout.

Claude Code's own agent teams share a checkout and have no file conflict
enforcement at all. The documented answer is "break the work so each teammate
owns a different set of files", which is an instruction to the human. This is
that instruction, enforced.

## How it works

Three mechanisms, both agents, verified:

1. **Hooks fire deterministically.** Unlike an MCP tool, a hook does not depend
   on the model choosing to call it.
2. **`SessionStart` injects context.** Each agent is told its team, the
   territory map, and that other agents may be live.
3. **`PreToolUse` blocks a write** with exit code 2, and the agent reads the
   reason and adapts.

## Try it

```bash
go build -o bin/agentcoord ./cmd/agentcoord
cd /your/repo
/path/to/bin/agentcoord init      # writes .agentcoord.json, prints hook wiring
/path/to/bin/agentcoord status    # shows the territory map
```

Territory is declared per repo in `.agentcoord.json`:

```json
{
  "teams": {
    "backend":  { "owns": ["src/api/**", "migrations/**"], "agents": ["claude"] },
    "frontend": { "owns": ["src/components/**"], "shared": ["src/types/**"], "agents": ["codex"] }
  }
}
```

A team can span vendors. Two Claude sessions and a Codex session can all be
`backend`. Set `AGENTCOORD_TEAM` to place one session on a team other than its
agent's default.

### Codex needs the repo trusted

Codex loads project-local `.codex/hooks.json` only for a trusted project. Add a
`projects."<path>".trust_level = "trusted"` entry to `~/.codex/config.toml`, or
the hooks are skipped without saying so.

## The verification that matters

Both agents were told to create one file in their own territory and one in the
other team's, in the same checkout.

| Agent | Team | Own territory | Other team's territory |
| --- | --- | --- | --- |
| Claude Code | backend | `src/api/users.go` created | `src/components/Button.tsx` blocked |
| Codex | frontend | `src/components/Card.tsx` created | `src/api/orders.go` blocked |

Codex, in its own words: "`src/api/orders.go`: not created because `src/api/**`
is backend-owned and outside my frontend territory."

## Design rules

- **This tool must never be the reason someone's work stopped.** A territory
  violation is the only thing that blocks. An unreadable payload, an unparseable
  config, or no config at all allows the call and says so on stderr. Allowing
  costs a collision the user could already have had. Blocking on our own bug
  costs a session that cannot write to its own repo.
- **Nothing vendor-specific escapes `internal/hookio`.** Everything downstream
  works on one normalised `Event`.
- **Zero runtime dependencies.** A hook runs on every tool call, so the artifact
  is one static binary with nothing to resolve at startup. That is also why this
  is Go and not Node: process start is on the hot path.

## What is not built yet

Stated plainly, because a coordination tool that implies coverage it does not
have is worse than none.

- **Leases on shared files.** `shared` paths are declared and reported, but
  nothing yet serialises access to them. Two agents can both enter
  `src/types/**` right now. This is the next piece.
- **Shell edits are not covered.** An agent can edit any file with `sed` and
  bypass territory entirely. The hook says so on stderr when it sees an
  unchecked shell command rather than staying quiet about the hole.
- **No presence.** `SessionStart` injects the territory map but not who is
  actually live, because there is no lease store to read yet.
- **No dashboard.** The read model comes after there is state worth reading.
- **Claude Code and Codex only.** Cursor and Gemini CLI both have hooks and
  should slot into `hookio` without touching anything else.
- **`init` does not edit agent settings.** It prints what to add. Merging into
  files the user owns is not something to do behind their back.

## Layout

```
cmd/agentcoord/       CLI: hook, status, init
internal/hookio/      payload normalisation, the only vendor-aware code
internal/territory/   path globs to team ownership
internal/config/      per-repo .agentcoord.json
```

`internal/hookio/testdata` holds payloads captured from real Claude Code and
Codex sessions. If a vendor changes its envelope, those tests are how we find
out.
