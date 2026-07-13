# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- managing loops and loop runs from code
- invoking agents and following a session's live transcript (message rows and
  turns) as it streams
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

## Synthetic Webhooks

Local development bridges can post a Mobius-shaped webhook to a local app when
hosted Mobius cannot reach localhost:

```go
err := mobius.DeliverSyntheticWebhook(ctx, mobius.SyntheticWebhookDelivery{
	URL:           "http://127.0.0.1:8080/webhooks/mobius",
	Key:           signingKey,
	SecretRef:     "mobius/webhook/local",
	SecretVersion: 1,
	EventType:     string(mobius.WebhookEventRunCompleted),
	Data:          run,
})
```

```python
mobius.deliver_synthetic_webhook(mobius.SyntheticWebhookDelivery(
    url="http://127.0.0.1:8080/webhooks/mobius",
    key=signing_key,
    secret_ref="mobius/webhook/local",
    secret_version=1,
    event_type=mobius.WEBHOOK_EVENT_RUN_COMPLETED,
    data=run,
))
```

```ts
await deliverSyntheticWebhook({
  url: "http://127.0.0.1:8080/webhooks/mobius",
  key: signingKey,
  secretRef: "mobius/webhook/local",
  secretVersion: 1,
  eventType: WEBHOOK_EVENT_RUN_COMPLETED,
  data: run,
});
```

## Loops And Runs

Create a saved, runnable loop with an inline spec, then start a run by loop
ID. The run's `event` object is reachable in step templates at `event.*`;
optional `config` is reachable at `config.*`.

```go
loop, err := client.CreateLoop(ctx, mobius.LoopOptions{
	Name: "Customer onboarding",
	Spec: map[string]any{"steps": []any{ /* ... */ }},
})
if err != nil { return err }
run, err := client.StartRun(ctx, loop.Id, &mobius.StartRunOptions{
	Event:      map[string]any{"customer_id": "cus_123"},
	ExternalID: "customer-run-123",
})
```

```python
loop = client.create_loop(mobius.LoopOptions(
    name="Customer onboarding",
    spec={"steps": []},
))
run = client.start_run(
    loop.id,
    mobius.StartRunOptions(
        event={"customer_id": "cus_123"},
        external_id="customer-run-123",
    ),
)
```

```ts
const loop = await client.createLoop({
  name: "Customer onboarding",
  spec: { steps: [] },
});
const run = await client.startRun(loop.id, {
  event: { customer_id: "cus_123" },
  external_id: "customer-run-123",
});
```

Use `WaitRun` / `wait_run` / `waitRun` when callers need the fresh terminal run
record, or `WatchRun` / `watch_run` / `watchRun` when they need the live event
stream.

## Session Transcripts

An agent session streams as a live transcript: message rows (each keyed by an
immutable id) plus the turns that produced them. `SessionTranscript` is that
view, and the SDKs fold the stream into it for you — the whole merge is
set-by-id, so state frames overwrite and nothing is an increment except
streamed text.

`InvokeAgent` starts a turn and returns a `TurnTranscript`: the turn's
identity (`ID`, `SessionID`, `Status`) immediately, and its live transcript
when iterated. The stream is lazy — a caller that never iterates pays for
nothing beyond the invoke itself. Iteration yields after every state change
and ends at the turn's terminal state:

```go
turn, err := client.InvokeAgent(ctx, mobius.InvokeAgentOptions{
	AgentName: "support",
	Content:   []map[string]any{{"type": "text", "text": "check the deploy"}},
})
if err != nil { return err }
for turn.Next() {
	render(turn.Messages())
}
if err := turn.Err(); err != nil { return err }
// turn.Status() == "completed"
```

```python
turn = client.invoke_agent(mobius.InvokeAgentOptions(
    agent_name="support",
    content=[{"type": "text", "text": "check the deploy"}],
))
for t in turn:
    render(t.messages())
# turn.status == "completed"
```

```ts
const turn = await client.invokeAgent({
  agentName: "support",
  content: [{ type: "text", text: "check the deploy" }],
});
for await (const t of turn) {
  render(t.messages());
}
// turn.status === "completed"
```

`Messages` is scoped to the invoked turn (the caller's message row is seeded
before any streaming); the full session view is on `Transcript()` /
`transcript`. If the invoke dedupes onto an already-finished turn, iteration
hydrates once from the snapshot endpoint instead of streaming, so `Messages`
is complete either way.

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
  render(t.messages());
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
