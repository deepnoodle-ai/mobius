import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  AuthRevokedError,
  Client,
  WorkerInstanceConflictError,
} from "../src/client.js";
import { Worker, WorkerPool, resolveInstanceID } from "../src/worker.js";

function testClient(): Client {
  return new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "test-project",
  });
}

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

test("worker: classifies terminal protocol error codes", () => {
  const worker = new Worker(testClient(), {
    workerInstanceId: "dup",
    logger: null,
  });
  const classify = (error: { code?: string; message?: string }) =>
    (worker as unknown as {
      terminalProtocolError(e: unknown): Error | undefined;
    }).terminalProtocolError(error);

  const conflict = classify({
    code: "worker_instance_conflict",
    message: "already registered",
  });
  assert.ok(conflict instanceof WorkerInstanceConflictError);
  assert.equal(conflict.workerInstanceId, "dup");
  assert.equal(conflict.projectHandle, "test-project");
  assert.equal(conflict.message, "already registered");

  assert.ok(classify({ code: "invalid_actor" }) instanceof AuthRevokedError);
  assert.equal(classify({ code: "register_failed" }), undefined);
});

test("worker: run rethrows instance conflict without reconnecting", async () => {
  const worker = new Worker(testClient(), {
    workerInstanceId: "dup",
    reconnectDelayMs: 1,
    logger: null,
  });
  let calls = 0;
  (worker as unknown as { runSocket(): Promise<void> }).runSocket = async () => {
    calls += 1;
    if (calls > 1) {
      // Regression guard: a reconnect means the fix is broken. Stop the loop
      // so the test fails fast (run resolves) instead of hanging.
      worker.stop();
      return;
    }
    throw new WorkerInstanceConflictError("dup", "test-project");
  };

  await assert.rejects(worker.run(), WorkerInstanceConflictError);
  assert.equal(calls, 1);
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
