import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";
import { Worker, WorkerPool, resolveInstanceID } from "../src/worker.js";

test("worker: resolveInstanceID honors explicit value", async () => {
  const resolved = await resolveInstanceID("worker-1");
  assert.deepEqual(resolved, { id: "worker-1", source: "configured" });
});

test("worker: registers action functions fluently", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "default",
  });
  const worker = new Worker(client, { logger: null });
  assert.equal(
    worker.register("demo.action", async () => ({ ok: true })),
    worker,
  );
});

test("worker pool: registers shared action functions fluently", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "default",
  });
  const pool = new WorkerPool(client, { logger: null, count: 2 });
  assert.equal(pool.register("demo.action", async () => ({ ok: true })), pool);
});
