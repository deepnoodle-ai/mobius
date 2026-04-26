# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/). Mobius is pre-1.0; pin your version.

## [Unreleased]

### Changed

- Go SDK: rename `RunEventTypeStepProgress` to `RunEventTypeJobUpdated` so
  the constant matches the wire value (`job_updated`) emitted by the server
  and documented in the spec. Source-incompatible for callers using the old
  name.

### Fixed

- Go SDK: `WatchRun` and `WatchProjectRuns` now actually stream. The
  previous implementation called the generated `*WithResponse` wrappers,
  which buffered the body via `io.ReadAll` and closed it before returning,
  so the goroutine read from a closed body and emitted no events. Switched
  to the unbuffered `StreamRunEvents` / `StreamProjectRunEvents` and
  delegated frame parsing to `wonton/sse`, which handles multi-line `data:`
  fields, `\r\n` line endings, comment frames, and an 8 MiB line buffer for
  larger envelopes.
- Go SDK: `WatchRun(ctx, id, 0)` no longer sends `?since=0`; `since=0` now
  means live-only. Pass a positive seq cursor to replay durable events.

### Added

- CLI output formatting: `--output / -o {auto,pretty,json,yaml,text}`,
  `--fields / -F` projection, `--quiet / -q`, `--var KEY=VALUE` substitution
  in `--file` and `@-` flag contents, `--dry-run`, `@path` / `@-` support,
  YAML auto-detect on JSON-typed body flags, and repeatable `--tag KEY=VALUE`
  on tag-bearing resources. Distinct exit codes for 4xx vs. 5xx.
- `mobius workflows apply` for idempotent upsert by handle, and
  `mobius workflows init` to scaffold a starter spec.
- Per-command pretty renderers via `RegisterResponseRenderer` (only fire on
  pretty + no `--fields`); `getRun` now renders a status header and a
  per-execution-path table.
- Auth login URL overrides: `--api-url` and a separate web-URL override are
  honored when constructing the device-auth flow.

### Changed

- Trim admin/billing/org/role/integration/service-account schemas out of the
  public client surface and rename permission strings to the new
  `mobius.work.execute`, `mobius.project.view`, `mobius.access.manage` set.
- Add `widget_id` path parameter and `WorkflowRunJobCounts` schema; sync
  workflow run lifecycle/status contract.
- Tighten `TagMap` cap from 50 to 8 per resource and enforce key/value
  length and `propertyNames` in schema; drop redundant tag descriptions.
- Python codegen now uses `--strict-nullable`, so required-and-nullable
  fields produce `Optional` types and fields with non-null defaults stop
  accepting `None`.
- Regenerate Go, TypeScript, and Python SDKs from the updated OpenAPI
  contract.

### Fixed

- Device auth stays on a custom API origin instead of falling back to the
  production host.

## [0.0.7] - 2026-04-24

### Added

- Cascade-aware CLI configuration flags: `mobius runs start --config`,
  `--config-file`, `mobius runs get --show`, and project config helpers for
  setting and clearing inherited configuration.
- Named CLI credential profiles with default selection, `--profile` /
  `MOBIUS_PROFILE`, `MOBIUS_CREDENTIALS_FILE`, `auth use`, and `auth remove`.

### Changed

- Regenerated the Go, TypeScript, and Python SDKs from the updated OpenAPI
  contract.
- CLI credentials now record explicit project association and use the project
  suffix format for `mbx_` and `mbc_` credentials.

### Fixed

- `mobius auth status` now verifies saved credentials against the API and no
  longer misreports injected saved credentials as shell `MOBIUS_API_KEY`
  values.

## Earlier

See git tags for history before `v0.0.8`.
