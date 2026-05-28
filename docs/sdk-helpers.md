# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- managing automations and automation runs from code
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

## Automations And Runs

Create a saved automation, add an immutable version, publish it, then start a
run by automation handle.

```go
automation, err := client.CreateAutomation(ctx, mobius.AutomationOptions{
	Handle: "customer-onboarding",
	Name:   "Customer onboarding",
})
if err != nil { return err }
_, err = client.CreateAutomationVersion(ctx, automation.Handle, spec, &mobius.AutomationVersionOptions{
	Publish: true,
})
if err != nil { return err }
run, err := client.StartAutomationRun(ctx, automation.Handle, &mobius.StartRunOptions{
	Inputs:     map[string]any{"customer_id": "cus_123"},
	ExternalID: "customer-run-123",
})
```

```python
automation = client.create_automation(mobius.AutomationOptions(
    handle="customer-onboarding",
    name="Customer onboarding",
))
client.create_automation_version(
    automation.handle,
    spec,
    mobius.AutomationVersionOptions(publish=True),
)
run = client.start_automation_run(
    automation.handle,
    mobius.StartRunOptions(
        inputs={"customer_id": "cus_123"},
        external_id="customer-run-123",
    ),
)
```

```ts
const automation = await client.createAutomation({
  handle: "customer-onboarding",
  name: "Customer onboarding",
});
await client.createAutomationVersion(automation.handle, spec, {
  publish: true,
});
const run = await client.startAutomationRun(automation.handle, {
  inputs: { customer_id: "cus_123" },
  external_id: "customer-run-123",
});
```

Use `WaitRun` / `wait_run` / `waitRun` when callers need the fresh terminal run
record, or `WatchRun` / `watch_run` / `watchRun` when they need the live event
stream.

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
