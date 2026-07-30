# interlock

Lets coding agents from different vendors work the same repo at the same time
without overwriting each other.

You start Claude Code in one terminal and Codex in another, yourself, in one
shared checkout. They see each other. Whoever writes a file first has it for a
minute or two, and the other is told who is in there so it works on something
else and comes back.

**There is nothing to configure.** Run `interlock init` once and open your
agents.

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

**Awareness** is the half that needs no rules at all, and it does most of the
work. Each agent is told, as it starts and as it goes, who else is live in the
repo and which directories they are working in. An agent that knows where its
peer is will usually stay out of the way without ever being refused anything.

**Claims** are the backstop. Writing a file claims it for ninety seconds,
renewed on every touch and released when the session ends. A second agent that
tries the same file is refused and told who has it, in those words: a timing
conflict, not a permission one, so work on something else and come back.

**Territory** is optional, and only worth declaring for a rule the automatic
behaviour cannot infer, such as one team owning `migrations/` outright. Most
repos never need it. `interlock init --with-territory` writes a starter file.

One property shapes the lease design: **an agent cannot wait.** There is no way
to park a tool call and resume it when a lock frees, so a held file produces a
redirect ("someone else has this, go and do something else") and never a queue.

**Shell scanning** closes the obvious bypass. Territory enforced only at the edit
tool would be sidestepped by `sed -i`, so shell commands are scanned for the
paths they are likely to write.

**After-the-fact verification** covers the case the guard cannot decide in
advance. `python3 build.py` writes somewhere and nothing before it runs says
where, so the tree is snapshotted either side of the command and anything that
landed outside the agent's territory is reported back to it. That report also
grants the agent permission to revert exactly those paths, for fifteen minutes,
because a tool that demands a fix and then blocks the fix is worse than one that
says nothing.

**The dashboard** is how a person sees all of it at once: who is here, what each
agent last touched, which shared files are held, and what the tool has actually prevented. That
part matters more than it sounds: a coordination tool is invisible when it works,
and without a count of what it stopped there is no way to tell whether it is
earning its place. Misses are counted separately from saves, so the page cannot
flatter itself.

The page binds to loopback only,
has no flag to change that, and refuses a non-loopback address. It ships no
JavaScript and fetches nothing, because it renders paths and model names that came
out of another tool's session files.

## Try it

```bash
go build -o bin/interlock ./cmd/interlock
cd /your/repo
/path/to/bin/interlock init --dry-run   # see exactly what would change
/path/to/bin/interlock init             # wire the hooks into both agents
/path/to/bin/interlock status           # territory, plus who holds what
/path/to/bin/interlock dashboard        # live view on 127.0.0.1:7717
```

`init` merges into `.claude/settings.json` and `.codex/hooks.json` rather than
replacing them, keeps a copy of anything it modifies, is safe to run twice, and
refuses to touch settings it cannot parse. `init --uninstall` takes its hooks
back out and leaves everything else alone.

Territory is declared per repo in `.interlock.json`:

```json
{
  "teams": {
    "backend":  { "owns": ["src/api/**", "migrations/**"], "shared": ["src/types/**"], "agents": ["claude"] },
    "frontend": { "owns": ["src/components/**"], "agents": ["codex"] }
  }
}
```

A team can span vendors. Two Claude sessions and a Codex session can all be
`backend`. Set `INTERLOCK_TEAM` to put one session on a team other than its
agent's default.

Add `.interlock/` to `.gitignore`. Leases are local runtime state.

### Codex needs the repo trusted

Codex loads project-local `.codex/hooks.json` only for a trusted project. Add a
`projects."<path>".trust_level = "trusted"` entry to `~/.codex/config.toml`, or
the hooks are skipped without saying so. Codex also exposes no session-end event,
so its leases are freed by the time to live rather than on exit.

## The verification that matters

All four run against real Claude Code and Codex sessions in one shared checkout.

**Two agents, running at the same time, with no configuration at all.** Claude
was told to make three separate edits to `src/shared.ts`. While it worked, Codex
was told to add a comment to the same file. Codex reported:

> The first write was blocked because `claude` held `src/shared.ts`. I waited,
> re-read the file, saw another comment had been added, then applied the
> requested comment without removing that change.

All four edits survived. Neither agent lost the other's work, and neither was
told anything by a human.

**Territory, when it is declared, both directions:**

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

**`init` against real settings.** Given a `.claude/settings.json` holding a model
choice, a permissions block and the user's own `PreToolUse` guard, it added its
four hooks, left every one of those intact, kept the user's guard first in the
list, wrote a backup, appended to a `.gitignore` that had no trailing newline
without joining the entries, and reported "already wired up" on a second run.

**The dashboard.** Rendered both agents with the files they touched, the lease
held by backend, and per-team active counts, in light and dark. Zero script tags,
zero external URLs. It refused `--addr 0.0.0.0:7720`.

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

## Three bugs worth recording

**The pid trap.** Leases first reclaimed any lease whose recorded process had
exited, to recover quickly from a crash. A cross-process test found that this
made every lease stealable: the hook is a short-lived process that exits
milliseconds after acquiring, so its pid is always dead. Three of twelve racing
processes were each granted the same file. Liveness is now the time to live
alone, and `Lease.PID` is informational only. The in-process race test passed
throughout, which is why `crossprocess_test.go` exists.

**The guard that blocked its own fix.** After-the-fact verification told an agent
it had written outside its territory and to revert it. The agent tried to delete
the file and territory blocked that too, because the file still was not its to
touch. Found by running it, not by a test: the tool demanded a fix and then
prevented it. A flagged path is now writable by the session that was told to fix
it, for that path only and for fifteen minutes.

**flock, not a lock file.** A lock file is state that outlives the process that
made it, so a crash leaves a stale lock and the next run deadlocks. An flock is
held by the kernel on behalf of an open descriptor and is released when the
process dies for any reason. It is held for microseconds around the
read-decide-write section of `Acquire`, never across an agent's tool call.

## What is not built yet

Stated plainly, because a coordination tool that implies coverage it does not
have is worse than none.

- **Opaque shell writes are caught after the fact, not before.** A command whose
  target cannot be read off the command line is allowed, then checked. Outside a
  git repo it cannot even be checked, and `interlock doctor` says so.
- **Claude Code and Codex only.** Cursor and Gemini CLI both have hooks and
  should slot into `hookio` without touching anything else.
- **Unix only.** Leases need an advisory lock the kernel releases on process
  death. Windows would need `LockFileEx`; the build fails loudly rather than
  degrading into a racing no-op.
- **`init` cannot trust the repo for Codex.** That means editing
  `~/.codex/config.toml`, a global file, and TOML without a parser is how you
  corrupt someone's setup. It prints the two lines to add instead. Until they are
  added, Codex skips its hooks silently and only Claude Code is coordinated.
- **No test drives the two real agents against each other.** The wiring is now
  covered by integration tests that run the actual binary, but the cross-vendor
  claims in this README were checked by hand against live Claude Code and Codex
  sessions. That is fine for a handful of runs and not fine for a product.

## Layout

```
cmd/interlock/       CLI: hook, status, init
internal/hookio/      payload normalisation, the only vendor-aware code
internal/territory/   path globs to team ownership
internal/lease/       one holder at a time on shared files
internal/shellscan/   paths a shell command is likely to write
internal/session/     who is here and what they last touched
internal/journal/     what the tool actually prevented, capped
internal/gitstate/    what a command changed, for after-the-fact checks
internal/remedy/      narrow permission to undo a flagged write
internal/lockfile/    the one advisory lock, released by the kernel on death
internal/dashboard/   the live view, server-rendered
internal/install/     merging hooks into each agent's own settings
internal/config/      per-repo .interlock.json
```

`internal/hookio/testdata` holds payloads captured from real Claude Code and
Codex sessions. If a vendor changes its envelope, those tests are how we find
out.

`cmd/interlock/main_test.go` runs the built binary against real payloads in real
git repositories. Both of the bugs recorded above lived in the wiring between
packages, where no unit test could see them, and both were reintroduced
deliberately to confirm these tests fail on them. One of the two is caught only
by `TestInstalledConfigActuallyFiresForEveryWritingTool`, which reads the
settings `init` wrote and asks whether they would fire at all: every other test
calls the hook with an event already chosen, which skips the matcher and hides
exactly that class of fault.

## The failure this tool is most likely to have

Installed but not enforcing, with nothing to tell you. Codex skips project hooks
in a repo it does not trust and says nothing about it, so half a setup looks
exactly like a working one until two agents collide.

`interlock doctor` exists for that:

```
ok    config                  2 teams, root /your/repo
ok    claude hooks            4 events in .claude/settings.json
ok    codex hooks             3 events in .codex/hooks.json
FAIL  codex repo trust        this repo is NOT trusted, so Codex silently
                              skips its hooks
ok    after-the-fact checks   git available, opaque writes are verified
ok    leases                  available
```

Run it after `init`, and any time the dashboard looks quieter than it should.
