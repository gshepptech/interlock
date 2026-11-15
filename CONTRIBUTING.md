# Contributing

Bug reports and pull requests are welcome.

## Before you open a pull request

```bash
gofmt -l .        # should print nothing
go vet ./...
go test ./...
```

All three must pass. Some tests start real subprocesses and race several
processes for the same lease, so run the full suite, not just the package you
changed.

## Guidelines

- Keep vendor-specific code in `internal/hookio`. Everything downstream works on
  the normalised event.
- A hook runs on every tool call. Do not add work to the hook path without a
  reason, and do not add dependencies lightly.
- Interlock must never block a write because of its own failure. Only a real
  conflict blocks; anything else allows the call and reports why.
- Bug fixes come with a test that fails without the fix.
- If you change a hook payload parser, add the real payload to
  `internal/hookio/testdata`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`fix(lease): ...`, `docs: ...`).

## Reporting a bug

Include the output of `interlock doctor`, the agent and its version, and the
command or tool call that behaved unexpectedly.
