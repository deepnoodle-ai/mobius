# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- managing loops and loop runs from code
- streaming an agent session's live transcript (message rows and turns) into a
  reducer
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
immutable id) plus the turns that produced them. `SessionTranscriptReducer`
folds the stream frames into that view for you — the whole merge is set-by-id,
so state frames overwrite and nothing is an increment except streamed text.
Start a turn and render it as it streams, closing at the turn's terminal state:

```go
ack, events, err := client.InvokeAgentTranscript(ctx, mobius.InvokeAgentOptions{
	AgentName: "support",
	Content:   []map[string]any{{"type": "text", "text": "check the deploy"}},
})
if err != nil { return err }

r := mobius.NewSessionTranscriptReducer()
if ack.UserMessage != nil { r.Rows[ack.UserMessage.Id] = ack.UserMessage }
for ev := range events { // channel closes at the turn's terminal turn.upsert
	r.Apply(ev.Frame, ev.ID)
	render(r.MessagesForTurn(ack.Turn.Id))
}
```

```python
ack, stream = client.invoke_agent_transcript(mobius.InvokeAgentOptions(
    agent_name="support",
    content=[{"type": "text", "text": "check the deploy"}],
))
r = mobius.SessionTranscriptReducer()
if ack.user_message:
    r.rows[ack.user_message.id] = ack.user_message.model_dump(mode="json")
for event in stream:  # generator ends at the turn's terminal turn.upsert
    r.apply(event.frame, event.id)
    render(r.messages_for_turn(ack.turn.id))
```

```ts
// TS drives the reducer for you and yields it after every change.
for await (const transcript of client.invokeAgentTranscript({
  agentName: "support",
  content: [{ type: "text", text: "check the deploy" }],
})) {
  render(transcript.messages());
}
```

For an existing session, `WatchSessionTranscript` / `watch_session_transcript`
/ `watchSessionTranscript` follows the live transcript across reconnects
(reconnecting on a `rotate` close, stopping on `idle`). Go and Python yield the
raw `TranscriptStreamEvent` frames — apply them to your own reducer, as above —
while TypeScript yields the reducer directly. Drop to
`StreamSessionTranscript` / `stream_session_transcript` /
`streamSessionTranscript` for a single connection, or poll
`GetSessionTranscript` / `get_session_transcript` / `getSessionTranscript` and
fold each page in with `ApplySnapshot` / `apply_snapshot` / `applySnapshot` —
the snapshot is the same shape the stream is a view of, so a poller and a
subscriber converge on identical state. Read the ordered rows with
`Messages` / `messages` / `messages()` and the resume position from the
reducer's `Cursor` / `cursor`.

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
