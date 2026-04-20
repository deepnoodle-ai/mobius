import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  Client,
  DEFAULT_BASE_URL,
  LeaseLostError,
  PayloadTooLargeError,
  RateLimitedError,
} from "../src/client.js";

// Smoke tests for the hand-written Client wrapper: verify the error
// translation layer around 409 lease-lost responses and 204 empty claims.
// These are the failure modes most likely to silently drift from the Go
// and Python wrappers, so we assert them explicitly.

function clientWithFakeFetch(reply: {
  status: number;
  body?: unknown;
}): Client {
  globalThis.fetch = (async () => {
    const init: ResponseInit = {
      status: reply.status,
      headers: { "Content-Type": "application/json" },
    };
    return new Response(
      reply.body != null ? JSON.stringify(reply.body) : null,
      init,
    );
  }) as typeof fetch;
  return new Client({
    baseURL: "https://api.example.invalid",
    apiKey: "mbx_test",
    project: "test-project",
  });
}

test("smoke: defaults to the production API host", async () => {
  let requestedURL = "";
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  const client = new Client({ apiKey: "mbx_test", project: "test-project" });
  await client.claimJob({ worker_id: "worker-1" });

  assert.equal(
    requestedURL,
    `${DEFAULT_BASE_URL}/projects/test-project/jobs/claim`,
  );
});

test("smoke: claimJob returns null on 204", async () => {
  const client = clientWithFakeFetch({ status: 204 });
  const job = await client.claimJob({ worker_id: "worker-1" });
  assert.equal(job, null);
});

test("smoke: claimJob returns job on 200", async () => {
  const client = clientWithFakeFetch({
    status: 200,
    body: {
      job_id: "job_1",
      run_id: "run_1",
      workflow_name: "hello",
      step_name: "greet",
      action: "print",
      parameters: { msg: "hi" },
      attempt: 1,
      queue: "default",
    },
  });
  const job = await client.claimJob({ worker_id: "worker-1" });
  assert.ok(job);
  assert.equal(job!.job_id, "job_1");
  assert.equal(job!.action, "print");
});

test("smoke: heartbeatJob 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () => client.heartbeatJob("job_1", { worker_id: "w", attempt: 1 }),
    LeaseLostError,
  );
});

test("smoke: completeJob 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () =>
      client.completeJob("job_1", {
        worker_id: "w",
        attempt: 1,
        status: "completed",
      }),
    LeaseLostError,
  );
});

test("smoke: emitJobEvent posts to project events endpoint", async () => {
  let requestedURL = "";
  let requestBody = "";
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestBody = String(init?.body ?? "");
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  await client.emitJobEvent("job_1", {
    worker_id: "worker-1",
    attempt: 1,
    type: "scrape.page_done",
    payload: { url: "https://example.com" },
  });

  assert.equal(
    requestedURL,
    "https://api.example.invalid/projects/test-project/jobs/job_1/events",
  );
  assert.match(requestBody, /"type":"scrape\.page_done"/);
});

test("smoke: emitJobEvents 413 raises PayloadTooLargeError", async () => {
  const client = clientWithFakeFetch({ status: 413 });
  await assert.rejects(
    () =>
      client.emitJobEvent("job_1", {
        worker_id: "w",
        attempt: 1,
        type: "oversize",
        payload: { blob: "x" },
      }),
    PayloadTooLargeError,
  );
});

test("smoke: emitJobEvents 429 raises RateLimitedError", async () => {
  globalThis.fetch = (async () => {
    return new Response(null, { status: 429, headers: { "Retry-After": "2" } });
  }) as typeof fetch;
  const client = new Client({
    baseURL: "https://api.example.invalid",
    apiKey: "mbx_test",
    project: "test-project",
  });
  await assert.rejects(
    () =>
      client.emitJobEvent("job_1", {
        worker_id: "w",
        attempt: 1,
        type: "progress",
        payload: { pct: 10 },
      }),
    RateLimitedError,
  );
});
