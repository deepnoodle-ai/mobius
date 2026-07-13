# @deepnoodle/mobius

TypeScript SDK for [Mobius](https://www.mobiusops.ai/) — a work coordination
platform for mixed teams of humans, systems, and AI agents.

This package contains the runtime client and WebSocket worker used to execute
actions and LLM generations against the Mobius API, plus high-level helpers for
creating loops and starting, observing, and controlling loop runs.
Types are generated from the canonical
[OpenAPI spec](https://github.com/deepnoodle-ai/mobius/blob/main/openapi.yaml)
and round-tripped against the same cross-language contract fixtures as the Go
and Python SDKs.

## Install

```bash
npm install @deepnoodle/mobius
# or
pnpm add @deepnoodle/mobius
```

Requires Node.js 22+ (the worker uses the global `WebSocket`, stable since Node 22).

## Quick start

### Invoke a multi-tool agent

```ts
import {
  Client,
  MobiusAPIError,
  normalizeToolUse,
  textOf,
} from "@deepnoodle/mobius";

const client = new Client({
  apiKey: process.env.MOBIUS_API_KEY!,
  project: process.env.MOBIUS_PROJECT!,
});

try {
  const turn = await client.invokeAgent({
    agentName: "launch-scout",
    session: {
      mode: "continue_or_create",
      session_key: `research:${customerId}`,
    },
    idempotencyKey: inboundMessageId,
    content: [{ type: "text", text: "Check the name and create a shortlist." }],
    config: {
      toolkits: [
        { name: "naming", actions: ["naming.domain.check"] },
        { name: "shortlists", actions: ["shortlists.create"] },
      ],
    },
  });

  for await (const update of turn.updates()) {
    for (const message of update.transcript.renderableMessages()) {
      console.log(textOf(message));
      for (const block of message.content) {
        if (block.type === "tool_use") {
          const tool = normalizeToolUse(block);
          console.log(tool.resolvedAction?.name ?? tool.wireName);
        }
      }
    }
  }
  if (turn.error) throw turn.error;
} catch (error) {
  if (error instanceof MobiusAPIError && error.code === "session_turn_active") {
    // Choose explicitly: wait and re-invoke, or client.nudgeSession(...).
  } else {
    throw error;
  }
}
```

`messages()` is lossless; `renderableMessages()` is the UI projection.
Canonical catalog identity lives in `resolvedAction`, while `wireName` and
`wireInput` preserve what the model saw. See the
[cross-language SDK helper guide](https://github.com/deepnoodle-ai/mobius/blob/main/docs/sdk-helpers.md)
for nudges, reconnection, diagnostics, session management, and the
server-to-browser boundary.

### Worker

```ts
import { Client, Worker } from "@deepnoodle/mobius";

const client = new Client({ apiKey: process.env.MOBIUS_API_KEY! });

const worker = new Worker(client, {
  name: "email-sender",
  version: "1.0.0",
  queues: ["emails"],
  // Hold up to 5 jobs in flight from one process. Surfaces as a
  // single row on the workers page with a saturation bar.
  concurrency: 5,
});

worker.register("send_email", async (params, signal) => {
  // send email...
  return { sent: true };
});

await worker.run();
```

The `worker_instance_id` is auto-detected from the runtime platform
(Cloud Run revision, Kubernetes pod, Fly machine, Railway replica,
Render instance) and falls back to a per-boot UUID. Set
`workerInstanceId` explicitly only for stable singleton workers — two
live processes using the same override in the same project will
collide and the second will throw `WorkerInstanceConflictError`.

For independent presence rows (one row per worker) — e.g. graceful
draining or in-flight isolation — use `WorkerPool` with `count` and
optionally `workerInstanceIdPrefix` instead.

### Loop runs

```ts
import { Client } from "@deepnoodle/mobius";

const client = new Client({ apiKey: process.env.MOBIUS_API_KEY! });

const run = await client.startRun("customer-onboarding", {
  external_id: "customer-run-123",
  event: { customer_id: "cus_123" },
});

const terminal = await client.waitRun(run.id);
console.log(terminal.status, terminal.result, terminal.error_message);
```

### Loops

```ts
const loop = await client.createLoop({
  name: "Customer onboarding",
  spec: { steps: [] },
});

console.log(loop.id, loop.status);
```

### Webhooks

```ts
import {
  parseWebhookDelivery,
  verifySignedDelivery,
} from "@deepnoodle/mobius";

const verified = await verifySignedDelivery(request, {
  key: Buffer.from(process.env.MOBIUS_SIGNING_KEY_B64!, "base64"),
});
const event = parseWebhookDelivery(verified);
console.log(event.type, event.data);
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
  await client.startRun("customer-onboarding", {
    external_id: "customer-run-123",
    event: { customer_id: "cus_123" },
  });
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
