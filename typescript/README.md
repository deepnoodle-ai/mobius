# @deepnoodle/mobius

TypeScript SDK for [Mobius](https://www.mobiusops.ai/) — a work coordination
platform for mixed teams of humans, systems, and AI agents.

This package contains the runtime client and worker used to claim and execute
tasks against the Mobius API. Types are generated from the canonical
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
import { Client, Worker } from "@deepnoodle/mobius";

const client = new Client({ apiKey: process.env.MOBIUS_API_KEY! });

const worker = new Worker(client, {
  workerId: "my-worker-1",
  name: "email-sender",
  version: "1.0.0",
  queues: ["emails"],
  concurrency: 5,
});

worker.register("send_email", async (params, signal) => {
  // send email...
  return { sent: true };
});

await worker.run();
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

## Documentation

- [Mobius docs](https://docs.mobiusops.ai/)
- [Repository](https://github.com/deepnoodle-ai/mobius)

## License

Apache-2.0
