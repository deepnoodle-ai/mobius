# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## What this is

Public client side of Mobius (workflows / runs / tasks / workers). Three SDKs
(Go, Python, TypeScript) plus the `mobius` CLI, all generated from `openapi.yaml`.

## Commands

```bash
make generate        # regen Go client + CLI, TS schema, Python models
make generate-check  # CI gate — fails if generated files drift from spec
make test            # test-go + test-ts + test-py
```

Single-test: `go test -run TestX ./mobius/...`,
`cd python && uv run pytest -k name`, `cd typescript && pnpm test` (no per-test
filter without editing the node command).

## Non-obvious architecture

**`openapi.yaml` is the source of truth.** Never hand-edit `*.gen.go`,
`typescript/src/api/schema.ts`, or `python/deepnoodle/mobius/_api/models.py`.
Change the spec, run `make generate`.

**Go import path is `github.com/deepnoodle-ai/mobius/mobius`** — the nested
`mobius/mobius` is intentional (module root vs. library package). `mobius/api/`
is generated, `mobius/action/` is the stock-action library the `worker`
subcommand registers.

**CLI customization goes through `internal/cligen/overrides.go`**, keyed by
OpenAPI `operationId`. Every `cmd/mobius/commands_*.gen.go` is emitted from the
spec + the oapi-codegen client; only `app.go`, `main.go`, `worker.go` are
hand-written. If a new operation needs a non-default group/command name, add
an override in the same change as the spec edit.

**`internal/testdata/contract/` is the cross-language wire-format contract.**
Each SDK round-trips every fixture through its generated types and asserts
byte-equivalence. If a fixture fails in one SDK, fix the SDK or the spec —
never edit the fixture to match current SDK behavior. The runtime job
endpoints (claim / heartbeat / complete) are hand-written in each SDK and
validated *only* by these fixtures, so contract-test failures are real wire
bugs.

When adding a schema to `manifest.json`, also add a case to `newForSchema` in
`mobius/api/contract_test.go` — a missing binding fails the test by design.

## Commit & PR convention

Conventional Commits: `type: short imperative description`, lowercase after
the colon, no scope. Types in use: `feat`, `build`, `docs`, `chore`, plus
`openapi:` for spec-only changes. Use the same format for PR titles (squash
merges make them the commit subject).

