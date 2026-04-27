# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
three common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- managing saved workflow definitions from code

## Webhooks

Verify the exact request body before parsing it:

```go
event, body, err := mobius.ParseSignedWebhookRequest(r, secret)
```

```python
signed = mobius.parse_signed_webhook_request(body, headers, secret)
```

```ts
const signed = await parseSignedWebhookRequest(request, secret);
```

For lower-level framework integration, use `SignWebhookPayload` /
`sign_webhook_payload` / `signWebhookPayload`, `VerifyWebhookSignature` /
`verify_webhook_signature` / `verifyWebhookSignature`, and
`ParseWebhookEvent` / `parse_webhook_event` / `parseWebhookEvent`.

## Synthetic Webhooks

Local development bridges can post a Mobius-shaped webhook to a local app when
hosted Mobius cannot reach localhost:

```go
err := mobius.DeliverSyntheticWebhook(ctx, mobius.SyntheticWebhookDelivery{
	URL:       "http://127.0.0.1:8080/webhooks/mobius",
	Secret:    secret,
	EventType: string(mobius.WebhookEventRunCompleted),
	Data:      run,
})
```

```python
mobius.deliver_synthetic_webhook(mobius.SyntheticWebhookDelivery(
    url="http://127.0.0.1:8080/webhooks/mobius",
    secret=secret,
    event_type=mobius.WEBHOOK_EVENT_RUN_COMPLETED,
    data=run,
))
```

```ts
await deliverSyntheticWebhook({
  url: "http://127.0.0.1:8080/webhooks/mobius",
  secret,
  eventType: WEBHOOK_EVENT_RUN_COMPLETED,
  data: run,
});
```

## Saved Workflows

Use `EnsureWorkflow` / `ensure_workflow` / `ensureWorkflow` when startup code
should create a saved workflow if it does not exist and update it when the spec
or selected metadata changes.

```go
result, err := client.EnsureWorkflow(ctx, spec, &mobius.WorkflowOptions{
	Handle: "customer-onboarding",
})
```

```python
result = client.ensure_workflow(
    spec,
    mobius.WorkflowOptions(handle="customer-onboarding"),
)
```

```ts
const result = await client.ensureWorkflow(spec, {
  handle: "customer-onboarding",
});
```

The helpers intentionally keep reconciliation small: they match by handle when
provided, otherwise by name, fetch the current latest definition, and update
only changed metadata or the latest spec.
