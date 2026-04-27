import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  Client,
  DEFAULT_BASE_URL,
  LeaseLostError,
  PayloadTooLargeError,
  RateLimitError,
  RateLimitedError,
} from "../src/client.js";
import {
  WEBHOOK_EVENT_TYPE_HEADER,
  WEBHOOK_SIGNATURE_HEADER,
  buildSyntheticWebhookPayload,
  deliverSyntheticWebhook,
  parseSignedWebhookRequest,
  parseWebhookEvent,
  signWebhookPayload,
  verifyWebhookSignature,
} from "../src/webhook.js";

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
  await client.claimJob({ worker_instance_id: "worker-1", concurrency_limit: 1 });

  assert.equal(
    requestedURL,
    `${DEFAULT_BASE_URL}/v1/projects/test-project/jobs/claim`,
  );
});

test("smoke: claimJob returns null on 204", async () => {
  const client = clientWithFakeFetch({ status: 204 });
  const job = await client.claimJob({ worker_instance_id: "worker-1", concurrency_limit: 1 });
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
  const job = await client.claimJob({ worker_instance_id: "worker-1", concurrency_limit: 1 });
  assert.ok(job);
  assert.equal(job!.job_id, "job_1");
  assert.equal(job!.action, "print");
});

test("smoke: heartbeatJob 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () => client.heartbeatJob("job_1", { worker_instance_id: "w", attempt: 1 }),
    LeaseLostError,
  );
});

test("smoke: completeJob 409 raises LeaseLostError", async () => {
  const client = clientWithFakeFetch({ status: 409 });
  await assert.rejects(
    () =>
      client.completeJob("job_1", {
        worker_instance_id: "w",
        attempt: 1,
        status: "completed",
      }),
    LeaseLostError,
  );
});

// Session-token is the preferred fence value (the SDK stamps it on
// every claim and presents it on heartbeat / complete / events).
// The tests below mirror the worker_instance_id paths above so any
// drift in the token-bearing wire shape fails the suite.
test("smoke: heartbeatJob with worker_session_token serializes the token", async () => {
  let requestBody = "";
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    requestBody = String(init?.body ?? "");
    return new Response(null, { status: 409 });
  }) as typeof fetch;
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  await assert.rejects(
    () =>
      client.heartbeatJob("job_1", {
        worker_session_token: "tok-abc",
        attempt: 1,
      }),
    LeaseLostError,
  );
  assert.match(requestBody, /"worker_session_token":"tok-abc"/);
});

test("smoke: completeJob with worker_session_token serializes the token", async () => {
  let requestBody = "";
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    requestBody = String(init?.body ?? "");
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  await client.completeJob("job_1", {
    worker_session_token: "tok-abc",
    attempt: 1,
    status: "completed",
  });
  assert.match(requestBody, /"worker_session_token":"tok-abc"/);
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
    worker_instance_id: "worker-1",
    attempt: 1,
    type: "scrape.page_done",
    payload: { url: "https://example.com" },
  });

  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/projects/test-project/jobs/job_1/events",
  );
  assert.match(requestBody, /"type":"scrape\.page_done"/);
});

test("smoke: emitJobEvents 413 raises PayloadTooLargeError", async () => {
  const client = clientWithFakeFetch({ status: 413 });
  await assert.rejects(
    () =>
      client.emitJobEvent("job_1", {
        worker_instance_id: "w",
        attempt: 1,
        type: "oversize",
        payload: { blob: "x" },
      }),
    PayloadTooLargeError,
  );
});

test("smoke: emitJobEvents 429 raises RateLimitError", async () => {
  globalThis.fetch = (async () => {
    return new Response(null, {
      status: 429,
      headers: {
        "Retry-After": "2",
        "X-RateLimit-Scope": "key",
        "X-RateLimit-Limit": "100",
        "X-RateLimit-Remaining": "0",
      },
    });
  }) as typeof fetch;
  const client = new Client({
    baseURL: "https://api.example.invalid",
    apiKey: "mbx_test",
    project: "test-project",
    // POST without Idempotency-Key surfaces RateLimitError immediately
    // regardless of retry budget.
    retry: 3,
  });
  await assert.rejects(
    () =>
      client.emitJobEvent("job_1", {
        worker_instance_id: "w",
        attempt: 1,
        type: "progress",
        payload: { pct: 10 },
      }),
    RateLimitError,
  );
  // RateLimitedError still exported as a backward-compat subclass.
  assert.equal(
    Object.getPrototypeOf(RateLimitedError.prototype).constructor.name,
    "RateLimitError",
  );
});

test("smoke: startRun posts a correlated run request", async () => {
  let requestedURL = "";
  let requestBody = "";
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestBody = String(init?.body ?? "");
    return new Response(JSON.stringify(runBody("run_1", "active")), {
      status: 202,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  const run = await client.startRun(
    { name: "demo", steps: [] },
    {
      queue: "research",
      external_id: "external-1",
      metadata: { org_id: "org_1" },
      inputs: { topic: "sdk" },
    },
  );

  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/projects/test-project/runs",
  );
  assert.equal(run.id, "run_1");
  const body = JSON.parse(requestBody);
  assert.equal(body.mode, "inline");
  assert.equal(body.queue, "research");
  assert.equal(body.external_id, "external-1");
});

test("smoke: startWorkflowRun uses the workflow-bound route", async () => {
  let requestedURL = "";
  let requestBody = "";
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestBody = String(init?.body ?? "");
    return new Response(JSON.stringify(runBody("run_1", "active")), {
      status: 202,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  const run = await client.startWorkflowRun("wf_1", {
    external_id: "external-1",
    inputs: { topic: "sdk" },
  });

  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/projects/test-project/workflows/wf_1/runs",
  );
  assert.equal(run.id, "run_1");
  const body = JSON.parse(requestBody);
  assert.equal(body.external_id, "external-1");
  assert.equal(body.mode, undefined);
  assert.equal(body.definition_id, undefined);
});

test("smoke: run control helpers use project-scoped paths", async () => {
  const seen: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    seen.push(url);
    const path = new URL(url).pathname;
    if (path.endsWith("/signals")) {
      return new Response(
        JSON.stringify({ id: "sig_1", run_id: "run_1", name: "approval" }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      );
    }
    if (path.endsWith("/cancellations") || path.endsWith("/resumptions")) {
      return new Response(null, { status: 204 });
    }
    if (path.endsWith("/runs/run_1")) {
      return new Response(JSON.stringify(runDetailBody("run_1", "completed")), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response(
      JSON.stringify({ items: [runBody("run_1", "completed")], has_more: false }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });

  assert.equal((await client.getRun("run_1")).status, "completed");
  assert.equal(
    (await client.listRuns({ status: "completed", external_id: "external-1" }))
      .items.length,
    1,
  );
  await client.cancelRun("run_1");
  await client.resumeRun("run_1");
  assert.equal(
    (await client.sendRunSignal("run_1", { name: "approval" })).id,
    "sig_1",
  );

  assert.ok(seen.some((url) => url.includes("/runs?status=completed")));
  assert.ok(seen.some((url) => url.endsWith("/runs/run_1/cancellations")));
  assert.ok(seen.some((url) => url.endsWith("/runs/run_1/resumptions")));
  assert.ok(seen.some((url) => url.endsWith("/runs/run_1/signals")));
});

test("smoke: waitRun fetches after stream closes before terminal", async () => {
  let getCalls = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    const path = new URL(url).pathname;
    if (path.endsWith("/events")) {
      return new Response(
        `event: run_updated
id: 7
data: {"type":"run_updated","run_id":"run_1","seq":7,"timestamp":"2026-04-27T00:00:00Z","data":{"status":"active"}}

`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    getCalls += 1;
    return new Response(
      JSON.stringify(runDetailBody("run_1", getCalls === 1 ? "active" : "completed")),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
  const run = await client.waitRun("run_1", { reconnectDelayMs: 1 });

  assert.equal(run.status, "completed");
  assert.equal(getCalls, 2);
});

test("smoke: webhook helpers verify and parse signed requests", async () => {
  const body = `{"type":"run.completed","data":{"id":"run_1"}}`;
  const signature = signWebhookPayload("secret", body);

  verifyWebhookSignature("secret", body, signature);
  const parsed = parseWebhookEvent<{ id: string }>(body);
  assert.equal(parsed.type, "run.completed");
  assert.equal(parsed.data.id, "run_1");

  const req = new Request("https://example.invalid/webhooks/mobius", {
    method: "POST",
    body,
    headers: { [WEBHOOK_SIGNATURE_HEADER]: signature },
  });
  const signed = await parseSignedWebhookRequest<{ id: string }>(req, "secret");
  assert.equal(signed.event.data.id, "run_1");
  assert.equal(Buffer.from(signed.body).toString("utf8"), body);

  assert.throws(() =>
    verifyWebhookSignature(
      "secret",
      body,
      "sha256=0000000000000000000000000000000000000000000000000000000000000000",
    ),
  );
});

test("smoke: synthetic webhook delivery posts signed Mobius envelope", async () => {
  let requestedURL = "";
  let requestBody = "";
  let eventType = "";
  let signature = "";

  const fetchFn = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestBody = String(init?.body ?? "");
    const headers = new Headers(init?.headers);
    eventType = headers.get(WEBHOOK_EVENT_TYPE_HEADER) ?? "";
    signature = headers.get(WEBHOOK_SIGNATURE_HEADER) ?? "";
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  await deliverSyntheticWebhook({
    url: "https://example.invalid/webhooks/mobius",
    secret: "secret",
    eventType: "run.completed",
    data: { id: "run_1" },
    fetch: fetchFn,
  });

  assert.equal(requestedURL, "https://example.invalid/webhooks/mobius");
  assert.equal(eventType, "run.completed");
  assert.equal(signature, signWebhookPayload("secret", requestBody));
  assert.equal(
    requestBody,
    buildSyntheticWebhookPayload("run.completed", { id: "run_1" }),
  );
});

test("smoke: workflow helpers create, update, and ensure definitions", async () => {
  const requests: Array<{ method: string; path: string; body: unknown }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const path = new URL(url).pathname;
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    requests.push({ method: init?.method ?? "GET", path, body });

    if (path === "/v1/projects/test-project/workflows" && init?.method === "POST") {
      return jsonResponse(
        workflowDefinitionBody("wf_1", body.name, body.handle, body.spec),
      );
    }
    if (
      path === "/v1/projects/test-project/workflows/wf_1" &&
      init?.method === "PATCH"
    ) {
      return jsonResponse(
        workflowDefinitionBody(
          "wf_1",
          body.name ?? "research",
          "research",
          body.spec,
        ),
      );
    }
    if (path === "/v1/projects/test-project/workflows/wf_1") {
      return jsonResponse(
        workflowDefinitionBody("wf_1", "research", "research", {
          name: "old",
          steps: [],
        }),
      );
    }
    return jsonResponse({
      items: [workflowDefinitionSummaryBody("wf_1", "research", "research")],
      has_more: false,
    });
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });

  assert.equal(
    (
      await client.createWorkflow(
        { name: "research", steps: [] },
        { handle: "research" },
      )
    ).id,
    "wf_1",
  );
  assert.equal(
    (
      await client.updateWorkflow("wf_1", {
        name: "research v2",
        spec: { name: "research v2", steps: [] },
      })
    ).name,
    "research v2",
  );
  const result = await client.ensureWorkflow(
    { name: "research", steps: [] },
    { handle: "research" },
  );

  assert.equal(result.created, false);
  assert.equal(result.updated, true);
  assert.ok(requests.some((req) => req.method === "PATCH"));
});

function runBody(id: string, status: "active" | "completed" | "failed") {
  return {
    id,
    ephemeral: true,
    workflow_name: "demo",
    status,
    path_counts: {
      total: 1,
      active: status === "active" ? 1 : 0,
      working: status === "active" ? 1 : 0,
      waiting: 0,
      completed: status === "completed" ? 1 : 0,
      failed: status === "failed" ? 1 : 0,
    },
    job_counts: { ready: 0, scheduled: 0, claimed: 0 },
    wait_summary: {
      waiting_paths: 0,
      kind_counts: {},
      next_wake_at: null,
      waiting_on_signal_names: [],
      interaction_ids: [],
    },
    errors: [],
    attempt: 1,
    created_at: "2026-04-27T00:00:00Z",
    updated_at: "2026-04-27T00:00:00Z",
  };
}

function runDetailBody(id: string, status: "active" | "completed" | "failed") {
  return { ...runBody(id, status), paths: [] };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function workflowDefinitionSummaryBody(id: string, name: string, handle: string) {
  return {
    id,
    name,
    handle,
    latest_version: 1,
    created_by: "user_1",
    created_at: "2026-04-27T00:00:00Z",
    updated_at: "2026-04-27T00:00:00Z",
  };
}

function workflowDefinitionBody(
  id: string,
  name: string,
  handle: string,
  spec: unknown,
) {
  return { ...workflowDefinitionSummaryBody(id, name, handle), spec };
}
