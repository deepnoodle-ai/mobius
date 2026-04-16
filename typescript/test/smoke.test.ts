import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client, DEFAULT_BASE_URL, LeaseLostError } from "../src/client.js";

// Smoke tests for the hand-written Client wrapper: verify the error
// translation layer around 409 lease-lost responses and 204 empty claims.
// These are the failure modes most likely to silently drift from the Go
// and Python wrappers, so we assert them explicitly.

function clientWithFakeFetch(
  reply: { status: number; body?: unknown },
): Client {
  globalThis.fetch = (async () => {
    const init: ResponseInit = {
      status: reply.status,
      headers: { "Content-Type": "application/json" },
    };
    return new Response(reply.body != null ? JSON.stringify(reply.body) : null, init);
  }) as typeof fetch;
  return new Client({
    baseURL: "https://api.example.invalid",
    apiKey: "mbx_test",
    namespace: "test-ns",
  });
}

test("smoke: defaults to the production API host", async () => {
  let requestedURL = "";
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  const client = new Client({ apiKey: "mbx_test", namespace: "test-ns" });
  await client.claimTask({ worker_id: "worker-1" });

  assert.equal(requestedURL, `${DEFAULT_BASE_URL}/namespaces/test-ns/jobs/claim`);
});

test("smoke: claimTask returns null on 204", async () => {
  const client = clientWithFakeFetch({ status: 204 });
  const task = await client.claimTask({ worker_id: "worker-1" });
  assert.equal(task, null);
});

test("smoke: claimTask returns task on 200", async () => {
  const client = clientWithFakeFetch({
    status: 200,
    body: {
      data: {
        job_id: "task_1",
        run_id: "run_1",
        namespace_id: "ns_1",
        workflow_name: "hello",
        step_name: "greet",
        action: "print",
        parameters: { msg: "hi" },
        attempt: 1,
        queue: "default",
      },
    },
  });
  const task = await client.claimTask({ worker_id: "worker-1" });
  assert.ok(task);
  assert.equal(task!.job_id, "task_1");
  assert.equal(task!.action, "print");
});

test("smoke: heartbeatTask 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () => client.heartbeatTask("task_1", { worker_id: "w", attempt: 1 }),
    LeaseLostError,
  );
});

test("smoke: completeTask 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () =>
      client.completeTask("task_1", {
        worker_id: "w",
        attempt: 1,
        status: "completed",
      }),
    LeaseLostError,
  );
});
