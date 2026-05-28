# SDK Helpers

The Go, Python, and TypeScript SDKs expose the same convenience layer around
three common integration tasks:

- verifying and parsing Mobius outgoing webhook deliveries
- delivering Mobius-shaped synthetic webhooks for local/test bridges
- managing saved workflow definitions from code

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
