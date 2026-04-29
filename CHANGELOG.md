# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/). Mobius is pre-1.0; pin your version.

## [Unreleased]

## [0.0.18] - 2026-04-29

### Added

- Go SDK: typed `Context` helpers for the remaining worker-callable
  endpoints. `Context.RunServerAction(name, params, opts)` and
  `Context.RequestInteraction(req)` mirror the existing `EmitEvent`
  shape and auto-thread project handle, job ID, and step name, so
  callers no longer have to drop down to `client.RawClient()` and
  pointer-wrap optional fields. New `RunServerActionOptions`,
  `InteractionRequest`, `InteractionTarget`, and `InteractionKind`
  types.
- Go SDK: action-catalog wrappers `Client.ListActionCatalog` and
  `Client.GetActionCatalogEntry`, exposing the `Available` /
  `Integration` fields so callers can distinguish *missing action*
  from *action exists but the required integration is not configured*.
- Go SDK: `RunEventTypeCustom` and `RunEventTypeRunStepUpdated` SSE
  constants (the server emits both; the SDK previously didn't list
  them). `RunEvent.AsCustom()` unpacks the doubly-nested custom event
  envelope. `RunEvent.JobID` lifted from the wire envelope to the
  struct.
- Go SDK: `Context.ProjectHandle()` and `Client.ProjectHandle()`
  return the project handle (e.g. `"default"`) — what `ProjectID()`
  was actually returning all along.

### Changed

- Go SDK: `Context.ProjectID()` is deprecated in favor of
  `ProjectHandle()` (kept as a source-compatible alias). The internal
  `runtimeJob.ProjectID` field is renamed to `ProjectHandle` for
  consistency.
- Go SDK: `RunEventTypeActionAppended` is marked deprecated — the
  server does not actually emit this event.

## [0.0.17] - 2026-04-29

### Fixed

- SDKs (Go, Python, TypeScript): the `system_hostname` rung of
  `worker_instance_id` auto-detect now appends a per-boot 8-char random
  suffix so two workers booted on the same host never collide and trip
  the server's 60s instance-takeover window. Platform-aware rungs above
  it (Cloud Run, K8s `HOSTNAME`, Fly machine, Railway/Render replica
  IDs) already produced unique-per-replica IDs and are unchanged.

### Changed

- `WorkerInstanceID` / `workerInstanceId` config is documented as
  opt-in for stable identity across restarts (named singleton workers);
  the auto-detected default is now unique per process.
- `InstanceConflictError` message now points at the two concrete
  remediations (set an explicit instance ID, or wait for the existing
  registration to age out).

## [0.0.16] - 2026-04-29

### Added

- `RunStep` now carries required `transition_seq` (per-run monotonic
  ledger order) and `kind` (`worker_action` / `server_action` /
  `control`) fields. New `RunStepKind` enum surfaced in all SDKs.
- `WorkflowRun` gains structured run-source fields: `source_type`
  (`api` / `trigger` / `slack` / `fork` / `tool`), `source_id`, and
  `source_label`.
- `GET /v1/projects/{project}/runs` filters: `source_type` and
  `source_id`.
- List run steps is documented as ordered by `transition_seq`
  ascending so paginated reads can replay or reconstruct the ledger.

### Changed

- Spec rename: `Job.last_error` → `Job.error_message`. Generated
  Go/Python/TypeScript types follow.
- SDKs (Go, Python, TypeScript): `ListRunsOptions.initiated_by` removed
  in favor of `source_type` + `source_id`; `forked_from` is also
  exposed alongside.

### Removed

- `GET /v1/projects/{project}/runs/{id}/action-log` and the
  `ActionLogEntry` / `ActionLogListResponse` schemas. Run history is
  served by the durable run-step endpoints added in v0.0.15.
- The `action_appended` SSE event on the run-events stream.
- `WorkflowRun.initiated_by` (replaced by `source_type` /
  `source_id` / `source_label`) and `WorkflowRun.cancel_requested`
  (use `cancel_requested_at` and `status` instead).

## [0.0.15] - 2026-04-28

### Added

- SDKs (Go, Python, TypeScript): new run-history endpoints —
  `GET /v1/projects/{project}/runs/{id}/steps`,
  `GET .../steps/{step_id}`, and `POST .../runs/{id}/forks` — for
  inspecting durable per-step execution history and forking a terminal
  run from a chosen step.
- `WorkflowRun` and `WorkflowRunDetail` now carry a required
  `step_counts` field grouping run steps by status.

### Changed

- Spec rename: `default_job_config` → `default_step_config` on run
  detail, and the `job_failed` run-error type → `step_failed`. Generated
  Go/Python/TypeScript types follow.
- CLI: `runs get --show` enum updated to accept `default_step_config`
  in place of the renamed `default_job_config`.

### Fixed

- CLI: `--profile <name>` now overrides the default profile instead of
  silently using the default. Credential resolution moved out of
  pre-parse env mutation into an `authMiddleware` that runs after flag
  parsing, exposed via `authFor(ctx)`. Generated commands and `worker`
  use a new `requireAuth()` middleware in place of
  `cli.RequireFlags("api-key")`, so a saved browser-login session
  satisfies the auth gate without a redundant `--api-key`.

## [0.0.14] - 2026-04-27

### Fixed

- Go worker: claim loop now honors the server's `Retry-After` after a
  `RateLimitError` (429) instead of sleeping a hardcoded 2s, clamped to
  `MaxClaimRateLimitSleep` (5 min). Prevents fleets from extending an
  active rate-limit window by polling through it. Python and TypeScript
  workers will mirror this behaviour in a follow-up.

### Documentation

- `docs/retries.md`: add a "Worker claim loop" section documenting how
  workers react to `RateLimitError` from claim, so SDKs in other
  languages can match the Go behaviour.

## [0.0.13] - 2026-04-27

### Added

- Worker SDKs (Go, Python, TypeScript): add `concurrency` knob to
  `WorkerConfig` (default 1) with semaphore-gated claim loop, so a single
  worker process can hold N jobs in flight under one presence row.
- Worker SDKs: generate a per-boot `worker_session_token` UUID at `Worker`
  construction and send it as the lease fence on every claim, heartbeat,
  complete, and events call.
- Worker SDKs: auto-detect `worker_instance_id` from the runtime platform
  (Cloud Run revision + metadata, `HOSTNAME`, `FLY_MACHINE_ID`,
  `RAILWAY_REPLICA_ID`, `RENDER_INSTANCE_ID`), falling back to a generated
  UUID; the source is logged once at startup.
- CLI: add `--concurrency N` (primary, default 1) and `--instance-id`
  (advanced override) flags to the `worker` subcommand. `--workers` is
  retained for the multi-instance pool case and is mutually exclusive with
  `--concurrency`.

### Changed

- Worker protocol: `worker_id` is renamed to `worker_instance_id` on the
  wire. SDKs keep a deprecated alias on `WorkerConfig` (Go `WorkerID`,
  Python `worker_id`, TypeScript `workerId`) that logs a one-time
  deprecation warning.
- Worker SDKs: a 409 `worker_instance_conflict` from claim now surfaces as
  a hard `WorkerInstanceConflictError` out of `Worker.run` with a
  remediation message, replacing the previous log-and-keep-polling
  behaviour.

## [0.0.12] - 2026-04-27

### Added

- Go SDK: add outgoing webhook helpers for HMAC signing and verification,
  generic event-envelope parsing with raw JSON event data, and signed request
  parsing.
- Go SDK: add synthetic webhook envelope and delivery helpers for local/test
  bridges that need to post Mobius-shaped signed webhook requests.
- Go SDK: add saved workflow-definition helpers for list/get/create/update,
  plus `EnsureWorkflow` and `SyncWorkflows` reconciliation helpers.
- TypeScript and Python SDKs: add parity for webhook signature verification,
  event parsing, signed request helpers, synthetic webhook delivery helpers,
  and saved workflow-definition list/get/create/update/ensure/sync helpers.
- Docs: add cross-language SDK helper documentation and TypeScript README
  examples for the new webhook and workflow helper surfaces.

## [0.0.11] - 2026-04-27

### Added

- Go, TypeScript, and Python SDKs: add high-level run-control helpers for
  starting runs, starting workflow-definition runs, getting/listing runs,
  cancelling/resuming runs, sending signals, watching run events, and waiting
  for terminal completion.
- TypeScript and Python SDKs: add run-event watching and `wait` recovery logic
  on top of the generated OpenAPI bindings.

## [0.0.10] - 2026-04-27

### Added

- Go, TypeScript, and Python SDKs: add `WorkerPool` helpers for running
  multiple single-job workers in one process, with distinct worker identities
  and shared action registration.

### Changed

- Breaking: each SDK `Worker` now executes at most one active job lease at a
  time. Scale throughput with `WorkerPool`, multiple worker instances, or the
  CLI `--workers` option instead of per-worker job concurrency.
- CLI: replace the worker `--concurrency` option with `--workers` and update
  examples to show the new pool-oriented execution model.

## [0.0.9] - 2026-04-26

### Added

- Spec: `WorkerSession` now carries an embedded `user` profile (id, email,
  first/last name, avatar URL, timestamps) for CLI-backed human worker
  sessions, so callers don't need a second lookup to display attribution.
  Absent on service-account-backed sessions or when the user record is
  unavailable.

### Changed

- Spec docs: enumerate the full SSE run-events vocabulary
  (`run_updated`, `job_updated`, `action_appended`,
  `interaction_created` / `interaction_completed` /
  `interaction_group_claimed` / `interaction_group_released`, `custom`) on
  both project-wide and per-run streams, and instruct clients to ignore
  unknown future types rather than treat the stream as malformed.
- Go SDK: rename `RunEventTypeStepProgress` to `RunEventTypeJobUpdated` so
  the constant matches the wire value (`job_updated`) emitted by the server
  and documented in the spec. Source-incompatible for callers using the old
  name.
- Regenerate Go, TypeScript, and Python SDKs from the updated OpenAPI
  contract.

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

## [0.0.8] - 2026-04-26

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
