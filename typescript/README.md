# @deepnoodle/mobius

TypeScript SDK for [Mobius](https://www.mobiusops.ai/) — a work coordination
platform for mixed teams of humans, systems, and AI agents.

This package contains the runtime client and worker used to claim and execute
tasks against the Mobius API, plus high-level helpers for starting, observing,
and controlling workflow runs. Types are generated from the canonical
[OpenAPI spec](https://github.com/deepnoodle-ai/mobius/blob/main/openapi.yaml)
and round-tripped against the same cross-language contract fixtures as the Go
and Python SDKs.

## Install

```bash
npm install @deepnoodle/mobius
# or
pnpm add @deepnoodle/mobius
```

Requires Node.js 18+.

## Quick start

### Worker

```ts
import { Client, WorkerPool } from "@deepnoodle/mobius";

const client = new Client({ apiKey: process.env.MOBIUS_API_KEY! });

const workers = new WorkerPool(client, {
  workerIdPrefix: "email-sender",
  name: "email-sender",
  version: "1.0.0",
  queues: ["emails"],
  count: 5,
});

workers.register("send_email", async (params, signal) => {
  // send email...
  return { sent: true };
});

await workers.run();
```

### Low-level client

```ts
import { Client } from "@deepnoodle/mobius";

const client = new Client({ apiKey: process.env.MOBIUS_API_KEY! });

const claim = await client.claimJob({
  worker_id: "my-worker-1",
  queues: ["default"],
  wait_seconds: 20,
});
```

### Run control

```ts
const run = await client.startRun(
  {
    name: "demo",
    steps: [],
  },
  {
    external_id: "customer-run-123",
    metadata: { org_id: "org_123" },
  },
);

const terminal = await client.waitRun(run.id);
console.log(terminal.status, terminal.result_b64, terminal.error_message);
```

## Rate limiting

The client retries `429 Too Many Requests` and `503 Service Unavailable`
responses automatically, respecting the server's `Retry-After` header and
falling back to exponential backoff (`1s`, `2s`, `4s`, capped at 60s). Non-
idempotent `POST` / `PATCH` requests are retried only when they carry an
`Idempotency-Key` header; otherwise a `429` surfaces as `RateLimitError`
immediately.

```ts
import { Client, RateLimitError } from "@deepnoodle/mobius";

const client = new Client({
  apiKey: process.env.MOBIUS_API_KEY!,
  retry: 3, // default; set to 0 to disable retries entirely
});

try {
  await client.claimJob({ worker_id: "w1", queues: ["default"] });
} catch (err) {
  if (err instanceof RateLimitError) {
    console.warn(
      `rate limited on ${err.scope} scope; retry after ${err.retryAfter}s`,
    );
  }
}
```

The full policy is documented in
[`docs/retries.md`](https://github.com/deepnoodle-ai/mobius/blob/main/docs/retries.md).

## Documentation

- [Mobius docs](https://docs.mobiusops.ai/)
- [Repository](https://github.com/deepnoodle-ai/mobius)

## License

Apache-2.0
