# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/). Mobius is pre-1.0; pin your version.

## [Unreleased]

## [0.0.56] - 2026-07-17

### Added

- Curated create-or-adopt in all three SDKs: Go `CreateAgent`/`CreateProject`
  (plus thin agent Get/List/Update/Delete), Python `create_agent`/
  `create_project`, TS `createAgent`/`createProject`, with adopt exposed as
  `AdoptExisting`/`ExternalRef` and a client-side fail-fast. See `docs/sdk-helpers.md`.
- Adopt-mode creates now ride the replay-safe retry path in every SDK —
  transient failures are retried because `external_ref` makes the create
  idempotent; plain creates still get exactly one attempt.
- Adopt conflict codes (`external_identity_conflict`, `project_archived`,
  `project_capacity_reached`) as documented constants on the existing API
  error types in Go, Python, and TypeScript.
- Curated OAuth return-origin allowlist methods in all three SDKs
  (`GetOAuthReturnOrigins`/`ReplaceOAuthReturnOrigins` and equivalents);
  thin wrappers, server-side validation only.

### Fixed

- `mobius organizations replace-oauth-return-origins` gains `--clear` to send
  an empty list — the documented way to disable embedded return, previously
  unreachable from the CLI.

## [0.0.55] - 2026-07-17

### Added

- Go `CreateArtifact` and Python `create_artifact` project-authorized upload
  helpers matching the v0.0.53 multipart contract, plus a hand-written
  `mobius artifacts upload` command. See `docs/sdk-helpers.md`.
- Curated agent memory clients in Go, Python, and TypeScript: entry CRUD,
  keyword/semantic/hybrid search with preserved coverage, and bounded
  change-feed sync with explicit cursor-expiry recovery (#178 follow-up).
- Organization Action admin and signing-secret lifecycle in all three SDKs;
  create/rotate return one-time decoded secret material, and the CLI
  requires an explicit sink (`--secret-file` or `--show-secret`) (#179).
- Project and organization Skill lifecycle, usage, and ordered agent skill
  assignments in all three SDKs; `skills import` and `org-skills import`
  now take a document path or `-` for stdin (#181 follow-up).
- `ListActionInvocations` with every audit filter in all three SDKs, plus
  public exports of the catalog and invocation provenance types.
- `if_exists: adopt` on agent and project creation makes retries safe: a
  duplicate `external_ref` returns the existing resource (`200`) instead of
  `409`, in all three SDKs and the CLI.
- Agents gained an assign-once `external_ref` identity key, so the
  organization definition resolver can address an agent as
  `agent/<external_ref>` instead of by display name.
- Organization OAuth return-origin allowlist (`organizations
  get-oauth-return-origins` / `replace-oauth-return-origins`) in all three
  SDKs and the CLI, gating embedded partner OAuth connect flows.

### Changed

- **Breaking (Go):** deprecated `CreateArtifactFromFile` now returns a
  migration error directing callers to `CreateArtifact`; its lineage and
  visibility fields were already rejected by the v0.0.53 contract.

## [0.0.54] - 2026-07-17

### Added

- Organization-scoped Skill CRUD and usage contracts across Go, Python,
  TypeScript, plus explicit `mobius org-skills` CLI commands.

- Organization-scoped signed HTTP Action administration, catalog provenance,
  rotation-safe secret versions, and invocation audit metadata in generated
  Go, Python, TypeScript, and CLI contracts.

## [0.0.53] - 2026-07-17

### Added

- Project-authorized artifact uploads plus a TypeScript `createArtifact`
  helper for multipart bytes, metadata, and optional durable idempotency keys.
- Agent memory configuration now supports `index`, `full`, or `off` automatic
  context; keyword, semantic, and hybrid search with coverage; and a
  cursor-based, content-free mutation feed.

### Changed

- **Breaking:** Catalog entries now require `definition_scope` so clients can
  distinguish project-owned definitions from organization fallback Actions.
- **Breaking:** Agent create/update requests, responses, Blueprint inputs, and
  generated CLI commands no longer accept or expose the freeform `kind` field.
  Remove it from hand-written clients and stored Blueprint JSON before
  upgrading. Landed in the #175 spec sync.
- Session-scoped interaction filters now include every interaction opened by
  agent tool calls, rather than only `request_human_input` interactions.

## [0.0.52] - 2026-07-15

### Added

- Typed Go, Python, and TypeScript verify-then-parse helpers for signed action
  invocation v1 envelopes, with explicit signature, freshness, schema, and
  structure errors. See `docs/sdk-helpers.md`.

### Fixed

- Go, Python, and TypeScript clients now forward idempotency keys and retry
  replay-safe admissions after transient transport, response-body, `429`, and
  `5xx` failures without replaying unsafe new-session or streaming requests.

## [0.0.51] - 2026-07-14

### Added

- Turns can declare a structured-output contract via `output.schema`
  (`TurnOutputSpec`); completed turns and terminal `turn.upsert` frames expose
  the validated `output` and its `output_source` (`tool` or `text`).
- Go, Python, and TypeScript invoke/start-turn helpers now take an `output`
  option, and `TurnTranscript` exposes `output`/`output_source` (Go `Output()`/
  `OutputSource()`) read live from the terminal turn.
- Sessions accept a `retention` policy (`standard` or `bounded` with
  `ttl_seconds`) applied at creation; session reads expose the resolved
  `retention` and computed `expires_at`.
- OpenAPI spec now documents the `Idempotency-Key` request header parameter.

### Fixed

- Python request bodies now serialize alias-mapped fields under their wire
  names (e.g. `output.schema`, condition `if`) instead of the python-safe
  field name.

## [0.0.50] - 2026-07-14

### Added

- Agent invoke and existing-session turn helpers now accept a one-shot
  `operation.timeout_seconds`; turn cancellation is cooperative and permanent,
  with live loop-owned turns returning `409 turn_owned_by_run`.
- Session transcript snapshots and streams now expose human-input wait context
  and interaction lifecycle upserts across the generated Go, Python, and
  TypeScript contracts and transcript accumulators.
- TypeScript `SessionChat` wraps invoke, transcript following, pushed human-input
  interactions, and responses without polling or manual cursor handling.
- Curated Go, Python, and TypeScript clients now expose blueprint lifecycle,
  project permissions/RBAC, and filtered interaction-list operations, with
  route coverage for the complete session nudge lifecycle.

### Changed

- **Breaking:** model catalog `ModelOption.context_window_tokens` is now required;
  compaction supports model-relative `threshold` percentages plus an optional
  absolute `threshold_tokens` override.
- Reusing an invocation idempotency key returns the existing turn and never
  restarts a terminal turn, including one that was cancelled.
- TypeScript API index generation now exports composed OpenAPI schemas as well
  as object and enum schemas.

## [0.0.49] - 2026-07-13

### Added

- Agent discovery, session lookup by `agent_name` plus `session_key`, and
  opt-in transcript following across the Go, Python, and TypeScript SDKs.
- CLI surfaces for permissions, principals, and roles, including one-command
  principal creation with a role and first API key.

### Changed

- **Behavior change:** project API-key creation now rejects principals with no
  role assignments. Assign a role before minting the key, use `mobius
  principals create --role ... --with-key`, or explicitly pass
  `--allow-unassigned-principal` for a deliberately dormant credential.
- `idempotency_key` is the canonical run deduplication term. The high-level
  `external_id` aliases remain compatible but are deprecated.
- TypeScript now exposes edge-safe `./client`, `./transcript`, and `./worker`
  entrypoints that avoid importing Node-only worker dependencies.

## [0.0.48] - 2026-07-13

### Added

- Runtime and action-response context across generated clients and high-level
  session helpers, including typed existing-session turns and opt-in context
  transcript reads. See `docs/sdk-helpers.md`.

### Fixed

- `resume_cursor` is now a required stable invocation boundary for fresh and
  deduplicated turns, so terminal settlement no longer falls back to a bounded
  session tail when an idempotent invocation is resumed.

## [0.0.47] - 2026-07-13

### Fixed

- Terminal turn reconciliation now redrains from the immutable pre-turn cursor,
  recovering durable tool calls after the moving live cursor has passed them.
  See `docs/development/terminal-transcript-reconciliation-cursor.md`.

## [0.0.46] - 2026-07-13

### Fixed

- Turn transcript iterators now reconcile the durable snapshot before exposing
  their terminal update, so final tool artifacts are present when iteration ends.

## [0.0.45] - 2026-07-13

### Added

- Cross-language session SDK usability: structured errors and wait-or-nudge recovery,
  typed session/nudge/message/turn helpers, lossless and renderable transcript views,
  canonical tool normalization, stream diagnostics/logging, and shared contract fixtures.
  See `docs/sdk-helpers.md`.

## [0.0.44] - 2026-07-12

### Changed

- **Breaking:** session-transcript helpers redesigned: `SessionTranscriptReducer`
  is now `SessionTranscript`, `InvokeAgent` always returns a lazy `TurnTranscript`
  (`InvokeAgentTranscript` deleted), and watching yields a `TranscriptWatcher`.
  See `docs/development/session-transcript-api-redesign.md`.

## [0.0.43] - 2026-07-12

### Added

- Session transcripts: `getSessionTranscript` (snapshot) and `streamSessionTranscript` (SSE)
  endpoints with their transcript schemas and frames, plus generated Go/TypeScript/Python clients.
- Session transcript v2 SDK helpers: a `SessionTranscriptReducer` and the `getSessionTranscript` /
  `streamSessionTranscript` / `watchSessionTranscript` / `invokeAgentTranscript` client helpers.
  See `docs/sdk-helpers.md`.

## [0.0.42] - 2026-07-11

### Added

- Session nudge queue: a durable queue for injecting user direction into a running
  session. New endpoints and generated Go/TypeScript/Python clients, plus CLI commands
  `mobius sessions nudge`, `sessions list-nudges`, `sessions get-nudge`, and
  `sessions cancel-nudge`. A nudge lands on the newest nudgeable turn
  (`delivery: current_turn`); if no turn is nudgeable, Mobius queues a direct-session
  turn (`delivery: new_turn`) so accepted input is not lost. Queue entries have a
  `pending` | `delivered` | `cancelled` lifecycle, and reusing an `idempotency_key`
  with identical content returns the original acknowledgement (different content
  conflicts).
- Interactions `session_id` filter (`mobius interactions list --session-id`): returns
  pending interactions raised by agent tool calls
  (`consumer.kind=agent_tool`) whose invocation is a turn of the given chat session, so
  a chat surface can show which interactions its own turns are waiting on.

## [0.0.41] - 2026-07-09

### Changed

- Loop run recovery fields renamed for consistency: `StepRetriedPayload.recovery_mode`
  is now `recovery_action` (matching `RunResumedPayload.recovery_action`), and
  `retry_scope` is now a typed enum (`run_recovery` | `step_policy` | `transient`).
  `RecoverLoopRunRequest.wall_clock_timeout_seconds` is now `wall_clock_extend_seconds`
  — surfaced as the `mobius runs resume` / `runs retry` flag `--wall-clock-extend-seconds`
  and required when a run stopped on `wall_clock_exceeded`.
- Bumped the Dive dependency (CLI Ollama bridge) to v1.12.0.

### Added

- `CancelSession` accepts a `force` query param to also cancel loop-owned turns and
  unlock a wedged session (`mobius sessions cancel --force`). Use for recovery only;
  the owning run may be left inconsistent.
- `GenerationDeltaFrame` documents summarized thinking deltas, which arrive as
  `{ "type": "thinking", "thinking": "..." }` alongside normal `{ "text": "..." }` deltas.

## [0.0.40] - 2026-07-06

### Fixed

- `environment.git.clone` no longer dies when a PR branch is deleted between the
  trigger event and checkout. When the clone action receives a commit sha and/or
  fetch ref (e.g. `refs/pull/<n>/head`), it now performs an immutable checkout —
  init an empty repo, fetch the exact target, check out that commit, and recreate
  the local branch — instead of `git clone --branch <name>`, which failed on
  short-lived bot and release-automation branches. A missing branch/ref is now
  classified as a permanent error so the retry loop fails fast instead of
  widening the deletion window.

## [0.0.39] - 2026-07-06

### Added

- Projects now expose `external_ref` on create, update, and read models so
  clients can carry an assign-once tenant/workspace correlation key through the
  Go, Python, and TypeScript SDKs. The CLI adds `--external-ref` to project
  create/update commands.

## [0.0.38] - 2026-07-05

### Added

- Organization definition-resolver config: `GET`/`PUT
  /v1/organization/definition-resolver` with the `DefinitionResolverConfig`,
  `PutDefinitionResolverRequest`, and `DefinitionResolverAuth` schemas. CLI
  `organizations get-definition-resolver` / `replace-definition-resolver`.

### Changed

- Organization API key endpoints moved from `/v1/organization/api-keys` to
  `/v1/api-keys` (request/response schemas unchanged). The CLI keeps the
  dedicated `org-api-keys` group.
- Inline agent config docs clarify that a session holds one config at a time;
  concurrent definitions need separate sessions.

## [0.0.37] - 2026-07-05

### Added

- ThinkingEffort (`inherit`/`low`/`medium`/`high`/`xhigh`/`max`) as an agent
  default and an override on sessions, invoke sessions, and loop steps.
- Inline agent config on `invoke-agent`: send `instructions`, `model`,
  `effort`, `timeout`, `toolkits`, and `skills` with the request instead of
  using the stored agent definition. Surfaced as a `config` option on the
  ergonomic `InvokeAgent` helpers in all three SDKs, not just the raw client.
- `getProjectCapabilities` endpoint (CLI `projects get-capabilities`) and the
  `ProjectCapabilities` schema.
- `environment_id` filter on list-invocations and an `environment_id` field on
  the invocation record.

### Removed

- Dropped the `Worker` role from the org API key role enum.

## [0.0.36] - 2026-07-03

### Added

- Organization API keys: create/list/get/delete org-wide keys with a chosen
  role (defaults to Admin).
- Loop repositories gain an opt-in `push` flag for write-capable clones.

### Changed

- Agent memory: `putAgentMemoryEntry` renamed to `saveAgentMemoryEntry` (CLI
  `agents save-memory-entry`); `content` is now required (1..16384).
- Action signing: `secret_ref`/`secret_version` replaced by a single
  `signing_secret`, returned only on create/rotate.
- Interactions: `target_user_ids` is now required (min 1 item).
- `WorkerModelCatalogResponse` renamed to `WorkerModelCatalogListResponse`;
  `ArtifactThumbnailSummary` and several internal-only fields removed.

## [0.0.35] - 2026-07-02

### Fixed

- Environments: `environment.git.clone` now surfaces a non-zero git exit as an
  error (with bounded retries to absorb a freshly minted repo token that
  briefly 404s) instead of returning an empty workspace. A broken
  `prepare_repository` clone now fails the run at the git error rather than
  letting later steps run against an empty checkout.

## [0.0.34] - 2026-07-02

### Changed

- Environments: the bot git identity used for agent commits is now
  `Mobius Agent <noreply@mobiusops.ai>` (was `agent@mobiusops.ai`). The
  `noreply@` address follows the standard convention for an automated,
  non-deliverable committer.

## [0.0.33] - 2026-07-02

### Fixed

- Environments: `environment.git.clone` now force-sets the Mobius bot git
  identity (`Mobius Agent <agent@mobiusops.ai>`) instead of only setting it when
  unset. The base image ships a default global identity, so the old "if unset"
  guard never applied — commits made by ad-hoc git in a managed environment were
  misattributed to that default. Forcing the global identity is safe: a
  repo-local identity or an explicit `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env var
  still takes precedence.

## [0.0.32] - 2026-07-02

### Added

- CLI: `mobius git-credential-helper`, a hidden broker-backed credential helper
  so ordinary git commands (`fetch` / `pull` / `push` via `environment.bash`)
  inside a managed environment can authenticate. No token is persisted; push is
  a server-enforced opt-in. `environment.git.clone` now wires
  `credential.helper` to it plus a bot identity.
- Worker (Go): `--keep-warm=on-demand|<duration>|forever`,
  `--keep-warm-required`, and SIGINT / SIGTERM graceful shutdown.
- `job_report_cancelled` contract fixture for the worker `cancelled` status.

### Changed

- Worker (Go): client-side hardening from the architecture review —
  deadline-bounded writes, ping/pong liveness, ack-driven report delivery,
  resume-from-suspend detection, per-instance keep-warm scoping, jittered
  reconnect backoff, and a `KeepWarmWindow` / `KeepWarmRequired` config
  replacing the old release grace.

### Deprecated

- Worker (Go): `WorkerConfig.KeepWarmForLifetime` — use
  `KeepWarmWindow = KeepWarmForever`.

## [0.0.31] - 2026-07-01

Publishes the project blueprint API and reworks how the worker's environment
tools truncate oversized output.

### Added

- SDKs / CLI: a project **blueprint** API. `POST
  …/projects/{project_handle}/blueprints/apply` (operation `applyBlueprint`)
  applies a desired blueprint — a declarative set of project resources plus an
  apply mode — to a project, and `GET
  …/projects/{project_handle}/blueprints/bindings`
  (`listBlueprintBindings`) returns the mapping from each blueprint-defined
  resource key to the concrete resource it resolved to. New CLI group `mobius
  blueprints` (`apply` plus bindings listing), with generated
  request/response types across the Go, TypeScript, and Python SDKs.

### Changed

- Worker (Go): the built-in environment tools now truncate oversized output
  with per-tool strategies that keep the highest-signal content and
  self-describe the truncation in-band, replacing the previous one-size
  head-cut and its parallel structured-truncation fields. `bash` / `git
  clone` / `git fetch` keep head+tail so a trailing error survives; `git
  diff` cuts on whole-file (`diff --git`) boundaries; `git status` cuts on
  whole lines; `logs.tail` keeps the newest bytes behind a prepended notice;
  `files.read` paginates by offset with a `next_offset` trailer; `files.list`
  returns mtime-ranked, byte-budgeted entries with an accurate total. New tool
  parameters: `offset` on `files.read`, `max_output_bytes` on `git status` /
  `git diff`, and `max_bytes` on `logs.tail`.

## [0.0.30] - 2026-06-30

First-class SDK wrappers for the compound agent-invoke endpoint that shipped
generated-only in 0.0.29.

### Added

- SDKs: a hand-written `InvokeAgent` / `invokeAgent` / `invoke_agent`
  convenience method on each client wraps the compound `POST …/agents/invoke`
  call (resolve-or-create session + append the caller's input message + start
  a turn), mirroring the existing `StartRun` request-building and
  error-handling pattern. It returns the `202 Accepted` turn ack carrying a
  durable `after_sequence` stream cursor. A companion `InvokeAgentStream` /
  `invokeAgentStream` / `invoke_agent_stream` variant opens the same call with
  `Accept: text/event-stream` and surfaces the turn's activity inline as
  decoded session stream frames, mirroring `WatchRun`'s SSE plumbing. Provide
  exactly one of agent id or agent name plus the input content.

## [0.0.29] - 2026-06-30

Session transcript rework, synced from the mobius-cloud spec.
`session_messages` is now the single durable conversation log; live streaming
is ephemeral (never persisted) — the stream is a view over the durable log
spliced with a live channel, keyed by one cursor.

### Added

- SDKs / CLI: a new compound `invokeAgent` endpoint
  (`POST …/agents/invoke`) resolves or creates a session, appends the
  caller's input message, and starts a turn in a single retryable call —
  collapsing the create-or-resolve-session + start-turn sequence for a
  product backend (an embedded app, a Slack handler, a Telegram bot) calling
  per inbound message. Returns `202 Accepted` with a durable
  `after_sequence` stream cursor by default; sending `Accept:
  text/event-stream` streams the turn's activity inline instead, framed
  like `GET …/sessions/{id}/stream`. A repeated call with the same
  `input.idempotency_key` resolves the same session and resumes the
  existing turn rather than starting a second one. New CLI command:
  `mobius sessions invoke-agent`.
- SDKs / CLI: `listSessions` gains a `session_key` filter for a read-only
  deterministic session lookup that avoids a create-or-resolve round trip
  (requires `agent_id`, since session keys are scoped to an agent).
  `mobius sessions list --session-key`.
- SDKs: loop run records expose `queue_reason` and `plan_concurrency_limit`.
  `queue_reason` (new `LoopRunQueueReason` enum: `plan_concurrency`,
  `loop_policy`, `trigger_concurrency`) is present only while `status` is
  `queued` and says which gate placed the run in the durable queue;
  `plan_concurrency_limit` is the org-wide concurrent-run ceiling stamped at
  run start, present when the run was evaluated against a plan limit.

### Changed

- SDKs / CLI (BREAKING): the session events endpoint
  `GET …/sessions/{id}/events` is renamed to `GET …/sessions/{id}/stream`
  (operation `streamSession`) and is SSE-only — the JSON (non-streaming)
  response branch and its `limit` parameter are gone. The cursor is
  `after_sequence` (query) / `Last-Event-ID` (header), both in message
  sequence space; one primitive covers initial join, forward streaming, and
  resume. Read `…/messages` first and feed its tail sequence into `…/stream`
  to resume without overlap or gap. The `mobius sessions list-events` CLI
  command is now `mobius sessions stream` (flags `--after-sequence` /
  `--last-event-id`).
- SDKs (BREAKING): session message content is now a frozen typed
  `SessionContentBlock` union (`SessionTextBlock`, `SessionThinkingBlock`,
  `SessionToolUseBlock`, `SessionToolResultBlock`, `SessionImageBlock`),
  typing the content of `SessionMessage`, `SessionMessagePreviewFrame`,
  `SessionUserMessagePayload`, `AgentMessagePayload`, and
  `CompactionCreatedPayload`. Each variant keeps an open
  (`additionalProperties`) map so unknown fields round-trip losslessly.
  Request bodies stay lenient — only response/frame content is frozen. The
  live `tool.result` frame (`ToolResultPayload.content`) now carries the same
  typed blocks as the durable record, and message-role fields are tightened
  (`AgentMessagePayload.role` is the `assistant` literal;
  `SessionUserMessagePayload.role` is `SessionMessageRole`, never `assistant`).
- SDKs: the session SSE stream-frame union (`SessionStreamFrame`) is
  reference-only — several payloads are structurally identical
  (`user.message` vs `agent.message`) or open objects, so the `data:` body
  cannot be shape-matched to one variant. Consumers MUST dispatch on the SSE
  `event:` name and decode the body as the corresponding payload.
- SDKs (BREAKING): the hand-written `Automation*` convenience methods never
  finished migrating to the `loops` rename from 0.0.24 (MB-455, #101) — they
  were the only working names, not aliases over a `Loop*` primary surface as
  previously documented. Completed the migration instead of carrying the
  inconsistency further: Go's `CreateAutomation`/`ListAutomations`/
  `GetAutomation`/`UpdateAutomation`/`DeleteAutomation`/`StartAutomationRun`
  are now `CreateLoop`/`ListLoops`/`GetLoop`/`UpdateLoop`/`DeleteLoop`/
  `StartRun` (`mobius/automations.go` is now `mobius/loops.go`); Python's
  `create_automation`/`list_automations`/`get_automation`/
  `update_automation`/`delete_automation`/`start_automation_run` are now
  `create_loop`/`list_loops`/`get_loop`/`update_loop`/`delete_loop`/
  `start_run`; TypeScript's `createAutomation`/`listAutomations`/
  `getAutomation`/`updateAutomation`/`deleteAutomation`/`startAutomationRun`
  are now `createLoop`/`listLoops`/`getLoop`/`updateLoop`/`deleteLoop`/
  `startRun`. `AutomationOptions` / `UpdateAutomationOptions` /
  `ListAutomationsOptions` are now `LoopOptions` / `UpdateLoopOptions` /
  `ListLoopsOptions` in all three SDKs. `ListRunsOptions.AutomationID` /
  `automation_id` is gone — use `LoopID` / `loop_id`. No deprecated aliases
  are kept; the SDKs are pre-1.0 and these names were never the documented
  public surface.

### Removed

- SDKs (BREAKING): the durable `session_events` resource is gone. Removed
  schemas: `SessionEvent`, `SessionEventListResponse`, `SessionEventPayload`,
  and `SessionLifecycleFrame`. There is no event-list resource to page
  anymore — lifecycle pulses (`turn.*`, `agent.message`,
  `compaction.created`, tool activity) still arrive as live frames on
  `…/stream`, they just aren't persisted or independently listable.

## [0.0.28] - 2026-06-22

Reliability release for the environment worker, across all three workers (Go
managed worker, TypeScript and Python SDKs).

### Fixed

- Workers (Go / TypeScript / Python): self-heal the job-claim loop instead of
  wedging on a lost claim. An outstanding `jobs.claim` was cleared only by a
  `jobs.claimed` reply, with no timeout — so a single lost claim or lost
  response could silently stop a worker from claiming until its connection
  reached max-age (~5 min). On managed runs this also froze the worker's
  `last_seen`, so the dead-worker reaper failed the run's pending jobs as
  `environment_worker_unavailable`. Workers now bound an unanswered claim with
  a 60s timeout and reconnect, and clear the outstanding claim when a matching
  nonterminal `error` frame answers it.

### Changed

- TypeScript SDK: the minimum supported Node version is now 22. The worker
  uses the global `WebSocket`, stable since Node 22; Node 18 and 20 are
  end-of-life. `engines` and the README are updated to match.

## [0.0.27] - 2026-06-22

Worker/CLI release (no SDK API changes; the npm and PyPI packages are
unchanged from 0.0.26).

### Changed

- CLI / Worker: a `go install github.com/deepnoodle-ai/mobius/cmd/mobius@<tag>`
  build — how Mobius Cloud installs the managed environment worker — now
  reports its real module version instead of `"dev"`, via a
  `debug.ReadBuildInfo()` fallback when release ldflags are absent. The
  worker session's reported version is now trustworthy.

### Added

- Worker: managed-environment workers advertise their keep-warm posture as
  session capabilities (`keep-warm:lifetime` when configured for the
  worker's lifetime, `keep-warm:established` once the hold is pinned), so the
  control plane can confirm a run-scoped worker actually kept its Sprite warm.

## [0.0.26] - 2026-06-22

### Added

- CLI / SDKs: sessions are now a top-level `sessions` resource and CLI
  group, synced from the mobius-cloud spec. Agent-scoped session lookups
  were removed from the generated surface.

### Changed

- SDKs: loop `schema_version` is now pinned to `"1"`, matching the
  currently accepted loop authoring schema.
- SDKs: retry policy now retries transport-level failures for replayable,
  idempotent requests, sharing the same retry budget and backoff used for
  `429` / `503` responses.

### Fixed

- Worker: Sprite environments now maintain a Sprites Tasks API hold while
  jobs are running so the VM does not hibernate during long outbound
  operations such as repository clones.
- Worker: run-scoped Sprite workers can pin the keep-warm hold for the
  worker lifetime via `MOBIUS_WORKER_KEEP_WARM=1`, keeping the Sprite awake
  across the idle gaps between an agent loop's tool-call jobs.
- Worker: Sprite keep-warm holds are established synchronously before a
  lifetime-pinned worker starts claiming work, stay alive briefly across
  per-job idle gaps, and warn when refreshes degrade.
- Worker: `MOBIUS_WORKER_KEEP_WARM=1` is now fail-closed: startup or
  refresh failures for the required Sprite hold cause the worker to exit so
  the Sprite Service can restart it instead of claiming work while the VM is
  free to hibernate.

## [0.0.25] - 2026-06-16

### Changed

- SDKs / CLI: runs now carry a structured `event` / `config` / `meta`
  envelope instead of a single `inputs` map, synced from the mobius-cloud
  spec. Starting a run takes `event` (the exact event object, reachable in
  templates at `event.*`), optional `config` (`config.*`), and optional
  `meta`; the loop run record exposes the same fields. The high-level
  `StartRunOptions` gains `Event` / `Config` / `Meta` (Go) and
  `event` / `config` / `meta` (Python, TypeScript) in place of `Inputs` /
  `inputs`, and the loop create/update `default_inputs` option is now
  `default_config`. The `mobius runs start` CLI flag `--inputs` is now
  `--event`, plus `--config` and `--meta`.
- SDKs: the agent `tool_presentation` default is now `meta` (grouped command
  routers) rather than `flat` (one tool per action).
- SDKs: deleting a toolkit or skill no longer returns `409 Conflict` when it
  is still assigned to agents — it is automatically detached, so deletion is
  never blocked by existing assignments.

## [0.0.24] - 2026-06-12

### Added

- SDKs: loop spec `schema_version: "2"`, synced from the mobius-cloud
  spec. v2 specs use one expression language everywhere — expr
  `${{ ... }}` templates and bare expr predicates — over one namespace
  (`inputs`, `event`, `meta`, `steps.<key>.output`). Every step variant
  gains an `if` predicate field, and the loop spec gains a top-level
  `output` block that declares the run result. `save_as` and step-level
  `input` are schema_version 1 only (v2 outputs are always at
  `steps.<key>.output`; reference namespaces directly in config
  fields). v1 specs keep working unchanged. (#102)

### Changed

- CLI / SDKs: the generated `automations` command group and resources
  are now `loops`, synced from the mobius-cloud spec. Operations route
  through loop ID endpoints with handle lookup; the high-level
  `Automation*` SDK APIs are preserved as compatibility aliases so
  existing callers keep working. (MB-455, #101)

### Fixed

- Worker: in-flight jobs now survive WebSocket reconnects. Each claimed
  job previously ran under a per-connection context and reported over
  the captured socket, so any disconnect aborted the job and dropped its
  terminal report, leaving it `claimed` until the reaper requeued it —
  most visibly wedging `environment.artifacts.publish`. Job lifecycle is
  now decoupled from the socket: jobs run under the worker lifetime,
  heartbeats and generation deltas are best-effort while disconnected,
  and the terminal report is parked and delivered idempotently over the
  reconnected connection. (MB-459, #103)

## [0.0.23] - 2026-06-05

### Added

- CLI / SDKs: automation runs are now a top-level `runs` group
  (`get` / `list` / `cancel` / `signal` / `start` /
  `stream` / `list-events` / `list-steps`), synced from the
  mobius-cloud spec. (#95)
- CLI / SDKs: custom actions can now be worker-backed. A new
  `ActionEndpointKind` (`http` | `worker`) selects the dispatch mode;
  `endpoint_url` is required only for `http` actions, while `worker`
  actions are routed through jobs to connected workers advertising the
  registered name. `actions create` gains `--endpoint-kind` and no
  longer hard-requires `--endpoint-url`. (#99)
- SDKs: `invokeAction` is now job-backed — `ActionInvocationResult`
  carries a `job_id`, the endpoint may return `409 Conflict`, and a
  new `direct_action_invoke` timeline/job kind is emitted. (#99)
- CLI: `tables update` gains `--name` to rename a table (alongside
  ownership changes). (#99)
- CLI: `environments start-worker ENV -- ...` now appends command
  argv, including values that begin with `-`, and the
  `--command=<value>` help clarifies how to pass dash-prefixed
  argv. (#98)

### Changed

- CLI: `mobius auth login` now adopts the default profile whenever no
  profile currently holds it (first login, or after the former default
  was removed), instead of only when the credential store is empty. A
  deliberately-set default is never silently replaced, and login now
  prints which profile is active and how to switch. (#94)
- Worker: a `worker_instance_conflict` protocol error received over
  the worker WebSocket is now terminal. The worker (and worker pool)
  exits non-zero instead of reconnecting, so a duplicate
  `--instance-id` fails fast under a process supervisor rather than
  losing the registration race on every reconnect. `invalid_actor` is
  likewise treated as terminal if it arrives mid-session, not only at
  registration. (MB-402, #96)
- SDKs: re-synced from the mobius-cloud spec. The `signalRun` /
  `SignalAutomationRunRequest` descriptions now frame the operation as
  resuming a suspended step, and the worker-socket protocol-error
  `code` field documents the terminal `worker_instance_conflict`. (#96)
- SDKs: regenerated from the mobius-cloud spec. The generated
  automation-run operations were renamed (`*AutomationRun*` ->
  `*Run*`); the hand-written `mobius` client wrappers (`StartRun`,
  `ListRuns`, `GetRun`, `CancelRun`, `SignalRun`, `WatchRun`,
  `WaitRun`) keep their existing signatures. (#95)
- SDKs: session message `content` is now an array of content blocks;
  the separate `content_blocks` field is removed. This is a
  wire-format change. (#99)
- SDKs: table `searchRows` switched to token-prefix search
  (punctuation and hyphens split terms); `CreateTableRequest.name`
  now requires the pattern `^[a-z][a-z0-9_]*$` (max 64 chars); and
  `create` / `update` / `searchRows` gained `409 Conflict`
  responses. (#99)
- Worker: the session staleness threshold dropped from 2 minutes to
  90 seconds. (#99)
- SDKs: `AutomationRetryPolicy.max_attempts` is now capped at 10, with
  documented worker re-queue vs. in-process delay semantics. (#99)

### Removed

- CLI: the `projects set-config` / `clear-config` commands were
  removed with the upstream project-config endpoints. (#95)
- CLI / SDKs: the `agent-tools`, `audit-logs`, `interactions`,
  `permissions`, `principals`, `roles`, and `secrets` command
  groups are no longer generated; the spec no longer exposes them.
  (#95)
- Python: the `INTERACTION_KIND_*` constants and the `InteractionKind`
  export were removed with the upstream interactions API. The generic
  `parse_interaction_callback` delivery helper is retained. (#95)
- SDKs: `ArtifactStorageProvider` drops `s3`; `mobius` is now the only
  provider. (#99)

### Fixed

- Python / TypeScript workers: a `worker_instance_conflict` protocol
  error received over the worker WebSocket is now terminal, matching
  the Go worker (#96). Both SDKs already defined
  `WorkerInstanceConflictError` but never raised it from the socket
  loop, so a duplicate `worker_instance_id` reconnected forever instead
  of exiting. They now raise it (and `AuthRevokedError` on
  `invalid_actor`) and stop without reconnecting. (MB-402, #97)

## [0.0.22] - 2026-06-04

### Added

- CLI / SDKs: new `catalog` group (`list-actions`,
  `list-events`, `get-action`) and `principals` group
  (`create` / `get` / `list` / `update` / `delete`), plus agent
  messaging-binding commands, `agents list-models`,
  `environments attach-worker`, and
  `automations deliver-http-trigger`. (#92)
- SDKs: unified signed-delivery helpers for constructing and
  verifying synthetic webhook / action deliveries, with
  validation that rejects bad overrides (negative or zero
  timestamps, empty delivery IDs) and malformed envelopes. Also
  adds `updated_by` and aligns `created_by` descriptions across
  the Agent, Channel, Workflow, Environment, Secret, Trigger,
  Webhook, Toolkit, Skill, and Table schemas. (#88)
- Worker: publish run output artifacts via job leases. (#90)
- CLI: every command now has argument and flag help. The
  generator emits positional-argument descriptions (resolving
  `$ref` path params) and matches header-parameter descriptions
  (e.g. `X-Idempotency-Key`), so no command shows blank
  arguments or flag help that echoes the field name. (#92)

### Changed

- SDKs: automation and worker surfaces updated;
  `AutomationTriggerInput` renamed to `AutomationTrigger` to
  match the spec, and the worker `--actions` flag honors
  `MOBIUS_WORKER_ACTION_NAMES`. (#89)
- CLI: the `service-accounts` group is removed (replaced by
  `principals` upstream) and audit-log filters use
  `principal_id` instead of `user_id`. (#92)
- Go SDK: `ActionCatalogEntry.Available` is replaced by
  `Readiness` / `ReadinessReason`, catalog actions moved to
  `/catalog/actions`, and `ListAutomationsOptions.Status` is now
  typed `ListAutomationsParamsStatus` (the list filter gained an
  `all` value distinct from `AutomationStatus`). (#92)

## [0.0.21] - 2026-05-22

### Added

- CLI / SDKs: new command groups regenerated from the upstream
  OpenAPI contract — `api-keys`, `permissions`, `roles`,
  `service-accounts`, `environments`, `worker-sessions`,
  `agent-tools` (Skills + Toolkits), `audit-logs` (project-scoped
  `list` and org-scoped `list-org`), plus channel drafts /
  scheduled messages / reactions / custom emoji and
  `channels block-catalog` + `channels dm`. (#81, #84, #86)
- SDKs: workflow updates now require an `expected_version` for
  optimistic concurrency. `mobius workflows apply` resolves the
  current `latest_version` automatically before sending the
  update. `UpdateWorkflowOptions` in all three SDKs threads the
  new field. (#81)
- Go SDK: `Channel` carries new required fields
  `agent_instructions`, `agent_instructions_version`, and
  `default_notification_level`. (#86)

### Changed

- CLI: top-level `mobius --help` now shows a one-line
  description for every command group. Many leaf names were
  simplified to drop redundant resource tokens
  (`webhooks ping-webhook` → `webhooks ping`,
  `team list-team` → `team list`,
  `runs fork-run` → `runs fork`,
  `interactions submit-interaction-handoff` →
  `interactions submit-handoff`, etc.). Project-scoped vs
  nested-resource list ops are disambiguated with
  `list-for-<scope>` (`runs list-for-workflow`,
  `artifacts list-for-run`, `groups list-for-member`). Catalog
  vs project actions are split into `actions invoke`,
  `actions get-catalog`, `actions list-catalog`. (#83)
- CLI: `agent-tools` no longer emits collision-suffixed leaves
  (`get-2`, `delete-2`); Skills and Toolkits each get explicit
  `<verb>-skill` / `<verb>-toolkit` leaves
  (`create-skill`, `create-toolkit`, `list-skill-assignments`,
  `list-toolkit-assignments`, …) and
  `get-agent-tool-manifest` is now `get-manifest`. (#83)
- CLI / SDKs: `Interaction.message` is renamed to
  `Interaction.title`, with a new optional `description`. The
  removed field `Interaction.accepted_submission_id` is gone.
  `send_run_signal` now returns the narrower
  `RunSignalAccepted` (`source_event_id`). `RunWaitSummary`
  reshuffles its fields into `wake_counts` /
  `subject_counts`. (#86)
- SDKs: `InteractionTarget` is now `{ user_ids, group_id }`;
  `CreateStandaloneInteractionRequest` and `Interaction` carry
  flat `target_user_ids` / `target_group_id` instead of a
  nested target object. (#81)
- SDKs: event-stream `since` is now an opaque string cursor on
  both `streamRunEvents` and `streamProjectEvents`. (#81)
- Build: `oapi-codegen` bumped to v2.7.0. Security-scheme scope
  constants in the generated Go client (`BearerAuthScopes`,
  `XApiKeyAuthScopes`) now use typed context keys, and
  nullable `omitempty` schemas are skipped (instead of emitting
  `null`) when nil. (#82)

### Removed

- CLI: dropped duplicate `projects delete-config` — use
  `projects clear-config` (which calls DELETE when
  `--key-prefix` is unset). (#83)
- CLI: `artifacts`, `observables`, `references`, `metrics`,
  `user-state`, `generate`, and `integration-providers` groups,
  plus `actions get` / `actions list`, were removed or replaced
  by the renamed surfaces above. (#84)

## [0.0.20] - 2026-05-06

### Changed

- CLI / SDKs: the `data-tables` command group is renamed to `tables`
  (alias `table`), and `DataTable` / `DataTableRow` schemas are
  renamed to `Table` / `TableRow` in all three regenerated clients.
  Operation IDs follow (`listDataTables` → `listTables`, etc.). URL
  paths were already `/v1/projects/{project}/tables/...`. The
  `TriggerKind` wire enum value `"data_table_row"` is now
  `"table_row"`. (#71)
- CLI: row commands inside the `tables` group spell their leaves
  consistently — `query-rows`, `search-rows`, `insert-row`,
  `upsert-row`, `bulk-insert-rows` — instead of the awkward
  auto-derived forms (`bulk-table-rows`, `query-table-rows`, etc.).

## [0.0.19] - 2026-05-06

### Added

- CLI: `mobius auth status` now reports the credential scope
  (`Scope: org`, `Scope: project:<handle>`, or `Scope: user`) so
  403s on a project-pinned key can be diagnosed without guessing
  whether the key targets the wrong project.
- CLI / SDKs: new command groups and operations regenerated from
  the upstream OpenAPI contract — `agent_invocations`, `data_tables`,
  `events`, `generate`, `integration_catalog`, `logs`, `messages`,
  `references`, and `spans`. The `tools` group is removed (operations
  consolidated/renamed upstream).
- SDKs: workflow data-flow surface — `bind` and `set` step types,
  declared `outputs` on workflow steps, run-step `skipped` status with
  `reason`, and binding provenance fields on run steps.

### Changed

- CLI: `ConfigEntry` is now a flat `{key, value}` pair with dotted
  keys (e.g. `runs.timeouts.execution`) instead of
  `{category, key, value}`. `projects clear-config` takes an optional
  `--key-prefix` instead of the previous required `--category`.
- Go SDK: `streamProjectRunEvents` is renamed to `streamProjectEvents`.
  The server stream now also carries `message_created`,
  `interaction_*`, and `span_appended` events plus
  `run`/`channel`/`interaction` filter params; `WatchProjectRuns`
  continues to subscribe with no filters and ignores non-run events
  (filter knobs are not yet plumbed through the SDK).
- Build: `datamodel-code-generator` bumped from 0.28.5 to 0.56.1 to
  handle the new `ContentBlock` ↔ `ToolResultContentBlock` circular
  reference introduced by the LLM `generate` API.

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
