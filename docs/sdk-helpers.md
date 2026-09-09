# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- creating (or safely re-creating) agents with create-or-adopt semantics
  keyed on `external_ref`
- managing org blueprints, principals, roles, and role assignments
- managing the organization OAuth return-origin allowlist
- managing organization Actions and their signing-secret lifecycle
- publishing org artifacts with metadata and safe retry keys
- reading, searching, and synchronizing agent memory
- managing org and organization-shared skills and agent skill assignments
- listing action invocation audit records with provenance filters
- listing interactions with run, session, target, inbox, and status filters
- invoking agents and following a session's live transcript (message rows,
  turns, and human-input interactions) as it streams
- running workers that execute action jobs and LLM generation jobs over
  WebSockets

## Webhooks

Verify the exact request body before parsing it:

```go
verified, err := mobius.VerifySignedDelivery(r, mobius.VerifySignedDeliveryOptions{
	Key: signingKey,
})
event, err := mobius.ParseWebhookDelivery(verified)
```

```python
verified = mobius.verify_signed_delivery(body, headers, key=signing_key)
event = mobius.parse_webhook_delivery(verified)
```

```ts
const verified = await verifySignedDelivery(request, { key: signingKey });
const event = parseWebhookDelivery(verified);
```

When the key must be selected from the delivery headers, use the resolver form:
`ResolveKey` in Go, `resolve_key` in Python, or `resolveKey` in TypeScript.

## Signed Action Invocations

HTTP actions configured with `invocation_format: signed_context_v1` receive a
typed identity envelope. Verify the exact raw request bytes before parsing them:

```go
body, err := io.ReadAll(r.Body)
if err != nil { return err }
verified, err := mobius.VerifyActionInvocationV1(
	body,
	r.Header,
	mobius.VerifySignedDeliveryOptions{Key: signingKey},
)
if err != nil { return err }
invocation := verified.Invocation
```

```python
verified = mobius.verify_action_invocation_v1(
    body,
    headers,
    key=signing_key,
)
invocation = verified.invocation
```

```ts
const body = new Uint8Array(await request.arrayBuffer());
const verified = await verifyActionInvocationV1(body, request.headers, {
  key: signingKey,
});
const invocation = verified.invocation;
```

The typed helpers ignore unknown fields but require every v1 scope, action,
actor, origin, and parameters field. Agent actors must include `agent_id`, and
non-agent actors must not. They report invalid signatures, stale deliveries,
unsupported schemas, and malformed envelopes as distinct errors. Generic
`ParseActionInvocation` / `parse_action_invocation` /
`parseActionInvocation` remains available for legacy bodies.

The formal wire contract is `ActionInvocationV1` in `openapi.yaml`. In Go,
stale deliveries match both `ErrInvalidSignedDelivery` and
`ErrStaleSignedDelivery`; schema and structure errors match
`ErrUnsupportedActionInvocationSchema` and `ErrMalformedActionInvocation`.
Python and TypeScript expose the corresponding `InvalidSignatureError`,
`StaleDeliveryError`, `UnsupportedActionInvocationSchemaError`, and
`MalformedActionInvocationError` classes.

After verification, compare the signed org, action ID, and action name
with the endpoint's configured expectations. Deduplicate by delivery ID within
the action/secret scope before performing side effects. Never choose a user or
tenant from `parameters`; user-mapped agent integrations should resolve by the
signed `(org_id, agent_id)` pair.

During key rotation, select the verification key using
`X-Mobius-Secret-Version`. Keep both the current and previous versions during
the 72-hour overlap; Mobius stops accepting or using the previous version when
that window expires. A delivery always signs the consuming org's `org_id`.
The Action ID is stable across renames and should be the primary
endpoint-side identity check.

## Action signing secrets

Action create and rotate responses reveal a `whsec_` signing secret exactly
once. Store that string immediately. The SDK parsing helpers turn it into the
32 raw bytes expected by the signing and verification helpers:

```go
key, err := mobius.ParseSigningSecret(actionSigningSecret)
```

```python
key = mobius.parse_signing_secret(action_signing_secret)
```

```ts
const key = parseSigningSecret(actionSigningSecret);
```

Rotation activates the new version immediately and leaves the prior version
enabled for the fixed 72-hour overlap. Outbound deliveries use the new
version; verifiers should accept both advertised versions during that window.

The CLI's `actions create` and `actions rotate-secret` refuse to run
without an explicit secret sink, so the one-time reveal is never burned by
accident: `--secret-file PATH` writes the key to a `0600` file and masks it
in the printed response; `--show-secret` prints the full response for
existing scripts.

## Organization OAuth Return Origins

`GetOAuthReturnOrigins` / `get_oauth_return_origins` /
`getOAuthReturnOrigins` and the matching `Replace…` methods manage the
organization's allowlist of exact HTTPS origins an embedded partner may name
as an OAuth connect `return_url`. Both require Admin or Owner membership:

```go
origins, err := client.ReplaceOAuthReturnOrigins(ctx, []string{"https://app.partner.example"})
```

```python
origins = client.replace_oauth_return_origins(["https://app.partner.example"])
```

```ts
const origins = await client.replaceOAuthReturnOrigins([
  "https://app.partner.example",
]);
```

The wrappers are deliberately dumb: the server normalizes each entry
(lowercase host, default ports stripped, duplicates collapsed) and rejects
invalid origins — nothing is validated client-side. Replace is a full
replacement over PUT, so it is already idempotent and retried like any other
idempotent request. Passing an empty list disables embedded return for the
organization; from the CLI that state is explicit:
`mobius organizations replace-oauth-return-origins --clear`.

## Action Invocation Audit

`ListActionInvocations` / `list_action_invocations` / `listActionInvocations`
lists the org's invocation audit records with every contract filter:
run, job, environment, action name, immutable action ID, definition scope,
signing-secret version, delivery ID, correlation ID, terminal status, and
pagination.

```go
page, err := client.ListActionInvocations(ctx, &mobius.ListActionInvocationsOptions{
	ActionID:        "act_2m7x9q5v8p3n4r6t",
	DefinitionScope: api.ListActionInvocationsParamsDefinitionScopeOrganization,
	SecretVersion:   2,
})
```

Each entry preserves definition provenance (`action_id`,
`definition_scope`) and, for signed HTTP deliveries, the delivery identity
(`delivery_id`, `correlation_id`, `secret_version`), so an audit can answer
"which definition ran, and which key signed it" without a raw transport. The
catalog entry and invocation entry types are exported publicly in all three
SDKs.

## Artifact Uploads

An action handler or other org-scoped service that produces deterministic
bytes can publish them directly to Mobius with an org-scoped API key holding
the artifacts write permission:

```go
artifact, err := client.CreateArtifact(ctx, mobius.CreateArtifactOptions{
	Name:           "renders/report.html",
	Path:           renderedPath, // or Reader for in-memory bytes
	Metadata:       map[string]any{"renderer": "omni"},
	IdempotencyKey: deliveryID + ":report",
})
```

```python
artifact = client.create_artifact(
    rendered_bytes,
    name="renders/report.html",
    metadata={"renderer": "omni"},
    idempotency_key=f"{delivery_id}:report",
)
```

```ts
const artifact = await client.createArtifact({
  name: "renders/report.html",
  file: renderedBytes,
  mimeType: "text/html",
  metadata: { renderer: "omni" },
  idempotencyKey: `${deliveryId}:report`,
});
```

The API key authorizes the upload; the action envelope is not forwarded to
Mobius. Lease-free artifacts are private to the API key's principal and do not
claim run or step lineage. Use a distinct idempotency key for each artifact
produced by one delivery. Repeating the same key under the same authenticated
principal returns the original artifact without uploading the bytes again.

### Session Attachments

A document or image that belongs to one conversation goes through the session
attachment endpoint instead, which binds the artifact immutably to the session
and returns the content block to append in a message or turn input. The Go SDK
and the CLI expose it today; Python and TypeScript callers use the generated
client:

```go
attachment, err := client.CreateSessionAttachment(ctx, sessionID,
	mobius.CreateSessionAttachmentOptions{
		Path:           briefPath, // or Reader for in-memory bytes
		IdempotencyKey: turnID + ":brief",
	})
// attachment.ContentBlock is the block to append in the next turn input.
```

```bash
mobius sessions attach sess_123 ./brief.md
```

Mobius detects the media type from the bytes rather than the multipart MIME
declaration, so `--mime` (and `Mime`) is only a hint. The retry key is scoped
to the session and caller: reusing it with different bytes, filename, or MIME
hint is rejected with a conflict.

## Synthetic Webhooks

Local development bridges can post a Mobius-shaped webhook to a local app when
hosted Mobius cannot reach localhost:

```go
err := mobius.DeliverSyntheticWebhook(ctx, mobius.SyntheticWebhookDelivery{
	URL:           "http://127.0.0.1:8080/webhooks/mobius",
	Key:           signingKey,
	SecretVersion: 1,
	EventType:     string(mobius.WebhookEventRunCompleted),
	Data:          run,
})
```

```python
mobius.deliver_synthetic_webhook(mobius.SyntheticWebhookDelivery(
    url="http://127.0.0.1:8080/webhooks/mobius",
    key=signing_key,
    secret_version=1,
    event_type=mobius.WEBHOOK_EVENT_RUN_COMPLETED,
    data=run,
))
```

```ts
await deliverSyntheticWebhook({
  url: "http://127.0.0.1:8080/webhooks/mobius",
  key: signingKey,
  secretVersion: 1,
  eventType: WEBHOOK_EVENT_RUN_COMPLETED,
  data: run,
});
```

## Create-or-Adopt Provisioning

`CreateAgent` (Go), `create_agent` (Python), and `createAgent` (TypeScript)
create a resource — or, in adopt mode, return the one that already carries
the same `external_ref`. Adopt mode is how provision-per-tenant code becomes
safely retryable: run the same call again after a crash or timeout and it
converges on the same resource instead of failing with 409.

```go
agent, err := client.CreateAgent(ctx, mobius.CreateAgentOptions{
	Agent:         api.CreateAgentRequest{Name: "PR reviewer"},
	AdoptExisting: true,
	ExternalRef:   "tenant-42/pr-reviewer",
})
```

```python
agent = client.create_agent(
    CreateAgentRequest(name="PR reviewer"),
    adopt_existing=True,
    external_ref="tenant-42/pr-reviewer",
)
```

```ts
const agent = await client.createAgent(
  { name: "PR reviewer" },
  { adoptExisting: true, externalRef: "tenant-42/pr-reviewer" },
);
```

Adopt mode requires the external ref: all three SDKs fail fast client-side —
before any HTTP request — when it is missing. A `201` response is a fresh
create; `200` is the existing resource, returned unchanged (mutable fields in
the request are ignored, since no write happens). Go also exposes the thin
agent lifecycle (`GetAgent`, `ListAgents`, `UpdateAgent`, `DeleteAgent`) so
adopted agent IDs never need to come from the raw client.

**Retry semantics — the part to get right.** This endpoint's idempotency
comes from `external_ref`, not an `Idempotency-Key` header. The SDK retry
layers therefore treat *adopt-mode* creates as replay-safe and retry them on
transient failures (429/5xx, connection resets) on the standard backoff
schedule. Plain creates still get exactly one attempt — replaying a
non-adopt create could double-create or spuriously 409. If you need a
retryable create, pass the adopt options; don't wrap a plain create in your
own retry loop.

**Conflict codes.** Adopt refuses to paper over identity conflicts. The
stable code — exported as a constant on each SDK's API error type
(`mobius.ErrCodeExternalIdentityConflict` in Go,
`MobiusAPIError.EXTERNAL_IDENTITY_CONFLICT` in Python and TypeScript):

- `external_identity_conflict` (409): the request names an identity (e.g.
  agent name) that differs from the resource owning the matched
  `external_ref`, or the match is soft-deleted — adopt never resurrects or
  replaces a deleted resource.

## Org Administration

The curated clients expose the same org-administration operations in each
language: apply/list/protect/delete blueprints, read the permission catalog,
manage machine principals and roles, and manage role assignments. Request and
response bodies use the generated OpenAPI models; handwritten option objects
cover query filters and pagination.

`ListInteractions` / `list_interactions` / `listInteractions` accepts every
contract filter, including `session_id` (`SessionID` in Go and `sessionId` in
TypeScript), so a chat surface can fetch pending human-input interactions for
one session without using a raw transport.

## Agent Memory

Agent memory has three surfaces: a summary (`GetAgentMemory`), the entries
themselves, and a content-free change feed for synchronization. Listing
doubles as search:

```go
page, err := client.ListAgentMemoryEntries(ctx, agentID, &mobius.ListAgentMemoryEntriesOptions{
	Query:      "deploy preferences",
	SearchMode: api.MemorySearchModeHybrid,
})
```

Semantic and hybrid searches surface a 503
`memory_semantic_search_unavailable` API error when the index is unavailable
— the SDKs never silently downgrade to keyword search, because that would
change result semantics. The response preserves `search_coverage` so callers
can see when rankings covered only a partially indexed subset.

`SyncAgentMemory` / `sync_agent_memory` / `syncAgentMemory` advances a
change-feed consumer by one bounded step: it drains every change page after
the supplied cursor and returns the new position to persist. When the cursor
predates retained history (HTTP 410), it recovers explicitly by returning a
full entry snapshot with `reset` set instead of failing or silently
replaying:

```ts
const result = await client.syncAgentMemory(agentId, savedCursor);
if (result.reset) {
  replaceLocalState(result.entries);
} else {
  applyChanges(result.changes);
}
savedCursor = result.nextCursor;
```

Polling cadence and retry policy stay with the caller; the SDK makes no
timing decisions. Entry versions make replayed changes detectable.

## Skills

Skills are reusable instruction bundles assignable to agents. The curated
clients expose the skill lifecycle and ordered agent assignments.
Import takes the Claude Code or Dive-style document as a plain string —
reading files stays a CLI concern:

```python
skill = client.import_skill(Path("SKILL.md").read_text(), name="PR review")
client.replace_agent_skill_assignments(agent_id, [skill.id])
```

Updates (`UpdateSkill` / `update_skill` / `updateSkill`) are explicit full-body
replacements — the SDKs do not fetch-and-merge, which would race concurrent
edits.

The CLI imports documents directly: `mobius skills import ./SKILL.md`, or
`mobius skills import - --name "PR review"` to read stdin with a name override.

## Session Transcripts

An agent session streams as a live transcript: message rows (each keyed by an
immutable id), the turns that produced them, and human-input interactions.
`SessionTranscript` is that view, and the SDKs fold the stream into it for you
— the whole merge is set-by-id, so state frames overwrite and nothing is an
increment except streamed text.

Use `Interactions` / `interactions` to inspect all interactions observed on the
live stream, or `PendingInteractions` / `pending_interactions` /
`pendingInteractions` for the unresolved subset. Snapshot `interactions` are
pending-only: a final snapshot removes stale local pending rows that it omits,
while terminal rows already observed on the stream remain available. A client
that misses the terminal `interaction.upsert` cannot infer whether an omitted
pending interaction completed, was cancelled, or expired.

TypeScript also exposes `SessionChat` for embedded chat surfaces. Its
`onInteraction` callback reports every new or changed interaction, including
terminal upserts. `onInteractionResolved` is narrower: it reports only an
observed pending-to-terminal transition, so a terminal interaction first seen
after reconnect is delivered through `onInteraction` only.

`InvokeAgent` starts a turn and returns a `TurnTranscript`: the turn's
identity (`ID`, `SessionID`, `Status`) immediately, and its live transcript
when iterated. The stream is lazy — a caller that never iterates pays for
nothing beyond the invoke itself. Iteration yields after every state change
and ends at the turn's terminal state:

```go
turn, err := client.InvokeAgent(ctx, mobius.InvokeAgentOptions{
	AgentName: "support",
	Content:   []map[string]any{{"type": "text", "text": "check the deploy"}},
	Context:   []mobius.RuntimeContextItem{{Name: "deploy", Content: "Status: canary"}},
})
if err != nil { return err }
for turn.Next() {
	render(turn.RenderableMessages())
}
if err := turn.Err(); err != nil { return err }
if err := turn.TurnError(); err != nil { return err }
```

```python
turn = client.invoke_agent(mobius.InvokeAgentOptions(
    agent_name="support",
    content=[{"type": "text", "text": "check the deploy"}],
    context=[mobius.RuntimeContextItem(name="deploy", content="Status: canary")],
))
for t in turn:
    render(t.renderable_messages())
if turn.error:
    raise turn.error
# turn.status == "completed"
```

```ts
const turn = await client.invokeAgent({
  agentName: "support",
  content: [{ type: "text", text: "check the deploy" }],
  context: [{ name: "deploy", content: "Status: canary" }],
});
for await (const t of turn) {
  render(t.renderableMessages());
}
if (turn.error) throw turn.error;
// turn.status === "completed"
```

Use `StartTurn` / `start_turn` / `startTurn` with the same `content`, `context`,
and idempotency options when the session already exists:

```go
turn, err := client.StartTurn(ctx, sessionID, mobius.StartTurnOptions{
	Content: []map[string]any{{"type": "text", "text": "check again"}},
	Context: []mobius.RuntimeContextItem{{Name: "deploy", Content: "Status: stable"}},
})
```

```python
turn = client.start_turn(session_id, mobius.StartTurnOptions(
    content=[{"type": "text", "text": "check again"}],
    context=[mobius.RuntimeContextItem(name="deploy", content="Status: stable")],
))
```

```ts
const turn = await client.startTurn(sessionId, {
  content: [{ type: "text", text: "check again" }],
  context: [{ name: "deploy", content: "Status: stable" }],
});
```

Both invoke and existing-session turn helpers accept a one-shot execution
timeout. Set `Operation: &api.AgentTurnOperationPolicy{TimeoutSeconds: &seconds}`
in Go, `operation=mobius.AgentTurnOperationPolicy(timeout_seconds=90)` in
Python, or `operation: { timeout_seconds: 90 }` in TypeScript. This policy
applies only to the newly admitted turn, overrides the stored agent timeout,
and is not persisted on the session.

Runtime-context names must be unique within a request. Supply names without the
`app-` namespace; a name already beginning with `app-` is deliberately delivered
as `app-app-*`. OpenAPI validates each content value by Unicode code points; the
server additionally enforces 8,192 UTF-8 bytes per item and 16,384 bytes total.
Those name-uniqueness and byte-total rules are server-side.

Runtime context stays hidden from ordinary transcript reads. Request caller-owned
`app-*` reminder rows explicitly when debugging delivery:

```go
page, err := client.ListSessionMessages(ctx, sessionID,
	&mobius.ListSessionMessagesOptions{Include: "context"})
```

```python
page = client.list_session_messages(
    session_id, mobius.ListSessionMessagesOptions(include="context")
)
```

```ts
const page = await client.listSessionMessages(sessionId, { include: "context" });
```

`Messages` is scoped to the invoked turn (the caller's message row is seeded
before any streaming); the full session view is on `Transcript()` /
`transcript`. If the invoke dedupes onto an already-finished turn, iteration
hydrates once from the snapshot endpoint instead of streaming, so `Messages`
is complete either way.

For a live turn, the terminal iteration step is also a durability boundary:
the SDK drains the incremental snapshot from the acknowledgement's stable
`resume_cursor`, then exposes that step. Fresh and deduplicated invocations both
receive a safe lower boundary. Deduplicated retries may replay already-seen
frames, which the transcript merges by identity; the moving transcript cursor
remains dedicated to reconnects. If settlement fails, iteration returns the
transport error instead of presenting an incomplete transcript as finished;
the observed terminal turn status remains available for diagnostics and retry.

### Lossless rows and renderable rows

`Messages` / `messages()` is the lossless protocol view. Use
`RenderableMessages` / `renderable_messages()` / `renderableMessages()` for
product UI. The renderable projection:

- replaces a preview with its final response segment using `turn_id`, role,
  and `turn_index` (with the legacy metadata index as fallback)
- keeps at most the newest empty assistant preview for an active turn
- removes duplicate tool-use/result blocks by tool-call id without collapsing
  genuinely repeated calls
- always returns array content and deterministic durable-then-live ordering

Tool calls keep both identities. The wire name/input are what the model saw;
`resolved_action` is the canonical catalog action Mobius actually dispatched.
Use the helper instead of deriving an action name from underscores:

```go
tool, err := message.Content[0].AsSessionToolUseBlock()
if err == nil {
	normalized := mobius.NormalizeToolUse(tool)
	if normalized.ResolvedAction != nil {
		fmt.Println(normalized.WireName, normalized.ResolvedAction.Name)
	}
}
text := mobius.TextOf(message)
```

```python
tool = mobius.normalize_tool_use(message["content"][0])
print(tool.wire_name, tool.resolved_action["name"])
text = mobius.text_of(message)
```

```ts
const tool = normalizeToolUse(message.content[0]);
console.log(tool.wireName, tool.resolvedAction?.name);
const text = textOf(message);
```

For tool results, `ToolResultText` / `tool_result_text` / `toolResultText`
handles both allowed content forms: a string or an array of typed blocks.
Meta-router `help` and built-in tools legitimately have no resolved action.

### Active-turn conflicts and nudges

Only one direct invocation may be `queued`, `running`, or `waiting` in a
session. A distinct overlapping call fails before its input is appended. It
surfaces as `APIError` / `MobiusAPIError` with status `409`, code
`session_turn_active`, and `details.turn_id` plus `details.status`.

Same-key retries still dedupe. For `running`/`waiting`, explicitly nudge when
the user intends to steer the active response:

```go
ack, err := client.NudgeSession(ctx, sessionID, mobius.NudgeSessionOptions{
	Content: incomingText, IdempotencyKey: inboundID, Wake: true,
})
```

```python
ack = client.nudge_session(session_id, mobius.NudgeSessionOptions(
    content=incoming_text, idempotency_key=inbound_id, wake=True,
))
```

```ts
const ack = await client.nudgeSession(sessionId, {
  content: incomingText,
  idempotencyKey: inboundId,
  wake: true,
});
```

Inspect `ack.delivery`: `current_turn` means the direction targets the active
turn; `new_turn` means a terminal race promoted it to follow-up work. A queued
turn cannot be steered, so normally wait and invoke again for that status.

For an existing session, `WatchSessionTranscript` / `watch_session_transcript`
/ `watchSessionTranscript` follows the live transcript across reconnects
(reconnecting on a `rotate` close, stopping on `idle`) — a `TranscriptWatcher`
handle in Go and Python, an async generator of the view in TypeScript:

```go
watch := client.WatchSessionTranscript(ctx, sessionID, nil)
for watch.Next() {
	render(watch.Messages())
}
if err := watch.Err(); err != nil { return err }
saveCursor(watch.Cursor())
```

```python
watch = client.watch_session_transcript(session_id)
for t in watch:
    render(t.messages())
save_cursor(watch.transcript.cursor)
```

```ts
for await (const t of client.watchSessionTranscript(sessionId)) {
  render(t.renderableMessages());
}
```

Drop to `StreamSessionTranscript` / `stream_session_transcript` /
`streamSessionTranscript` for raw frames on a single connection (fold them
with the view's `Apply`), or poll `GetSessionTranscript` /
`get_session_transcript` / `getSessionTranscript` and fold each page in with
`ApplySnapshot` / `apply_snapshot` / `applySnapshot` — the snapshot is the
same shape the stream is a view of, so a poller and a subscriber converge on
identical state. Read the ordered rows with `Messages` / `messages` /
`messages()` and the resume position from the view's `Cursor` / `cursor`.

TypeScript exposes every observed frame through `turn.updates()` and reports
cursor/readiness/reconnect/last-frame/connection facts through
`turn.diagnostics()`. Python exposes the same `updates()` and `diagnostics()`
methods. Go keeps its pull shape: after `Next`, read `Update()` and
`Diagnostics()` from the turn or watcher. Optional SDK logging reports request,
retry, stream-open/rotate/reconnect/idle, terminal, and transport facts without
headers, bodies, credentials, or message content.

## Session Management

The curated clients cover the common session lifecycle without dropping to a
raw generated client: list/get sessions, cancel, compact, list durable
messages, nudge list/get/cancel, and turn list/get/cancel. Names follow each
language's conventions (`ListSessions`, `list_sessions`, `listSessions`, and
so on). The generated client remains the escape hatch for less common session
operations.

Turn cancellation is idempotent, cooperative, and non-resumable. It retains
committed transcript rows but cannot roll back model, tool, or external effects;
reusing the invocation idempotency key returns the same cancelled turn.

## Server-to-browser boundary

Keep credentials and the Mobius transcript fold on your server. Send the
browser a product-owned projection containing the persisted cursor and the
result of `renderableMessages`; do not proxy the API key or make browser
components interpret raw provider tool names. Re-projecting from each
`updates()` frame keeps this adapter incremental without making it part of the
SDK contract.

Store the inbound message and its idempotency key before invoke. Store the
Mobius acknowledgement/cursor before acknowledging an upstream webhook. If
only the stream fails, resume it; invoking a distinct second message is not a
reconnect strategy. Hosted callbacks also cannot reach localhost: use a public
tunnel for a real callback test or a signed synthetic-webhook bridge locally.

## Workers

Workers keep one outbound WebSocket open to Mobius Cloud. The agent loop stays
in Mobius Cloud; workers execute the action and LLM generation jobs sent over
that socket.

```go
worker := client.NewWorker(mobius.WorkerConfig{
	Concurrency: 8,
	Queues:      []string{"default"},
})
worker.Register(mobius.ActionFunc("send_email", sendEmail))
err := worker.Run(ctx)
```

```python
worker = mobius.Worker(client, mobius.WorkerConfig(
    concurrency=8,
    queues=["default"],
))
worker.register("send_email", send_email)
await worker.run()
```

```ts
const worker = new Worker(client, {
  concurrency: 8,
  queues: ["default"],
});
worker.register("send_email", sendEmail);
await worker.run();
```
