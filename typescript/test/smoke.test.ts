import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  Client,
  ConfigError,
  DEFAULT_BASE_URL,
  isTerminalRunStatus,
} from "../src/client.js";
import {
  WEBHOOK_EVENT_TYPE_HEADER,
  buildSyntheticWebhookPayload,
  deliverSyntheticWebhook,
} from "../src/webhook.js";
import {
  MOBIUS_DELIVERY_ID_HEADER,
  MOBIUS_SECRET_REF_HEADER,
  MOBIUS_SECRET_VERSION_HEADER,
  MOBIUS_SIGNATURE_HEADER,
  MOBIUS_SIGNATURE_VERSION_HEADER,
  MOBIUS_TIMESTAMP_HEADER,
  parseWebhookDelivery,
  signDelivery,
  verifySignedDelivery,
} from "../src/signing.js";

function installFakeFetch(reply: {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
  capture?: (input: RequestInfo | URL, init?: RequestInit) => void;
}): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    reply.capture?.(input, init);
    return new Response(
      reply.body != null ? JSON.stringify(reply.body) : null,
      {
        status: reply.status,
        headers: {
          "Content-Type": "application/json",
          ...reply.headers,
        },
      },
    );
  }) as typeof fetch;
  return () => {
    globalThis.fetch = original;
  };
}

test("client: defaults to the production API host", async () => {
  let requestedURL = "";
  const restore = installFakeFetch({
    status: 200,
    body: { items: [], has_more: false },
    capture: (input) => {
      requestedURL = typeof input === "string" ? input : input.toString();
    },
  });
  try {
    const client = new Client({ apiKey: "mbx_test", project: "test-project" });
    await client.listLoops();
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    `${DEFAULT_BASE_URL}/v1/projects/test-project/loops`,
  );
});

test("client: extracts project handle from project-pinned API key", () => {
  const client = new Client({ apiKey: "mbx_secret.prod" });
  assert.equal(client.project, "prod");
});

test("client: rejects conflicting explicit project", () => {
  assert.throws(
    () => new Client({ apiKey: "mbx_secret.prod", project: "staging" }),
    ConfigError,
  );
});

test("client: workerSocketURL uses websocket scheme", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "default",
  });
  assert.equal(
    client.workerSocketURL(),
    "ws://localhost:8080/v1/projects/default/workers/socket",
  );
});

test("client: startRun posts the new request shape", async () => {
  let requestedURL = "";
  let requestBody = "";
  const restore = installFakeFetch({
    status: 202,
    body: loopRun("run_1", "running"),
    capture: (input, init) => {
      requestedURL = typeof input === "string" ? input : input.toString();
      requestBody = String(init?.body ?? "");
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      project: "test-project",
    });
    const run = await client.startRun("loop_1", {
      external_id: "external-1",
      event: { topic: "sdk" },
    });
    assert.equal(run.id, "run_1");
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/projects/test-project/loops/loop_1/runs",
  );
  assert.match(requestBody, /"idempotency_key":"external-1"/);
  assert.match(requestBody, /"event":\{"topic":"sdk"\}/);
});

test("client: run control helpers use loop run endpoints", async () => {
  const seen: string[] = [];
  const restore = installFakeFetch({
    status: 200,
    body: loopRun("run_1", "cancelled"),
    capture: (input) => {
      seen.push(typeof input === "string" ? input : input.toString());
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      project: "test-project",
    });
    await client.getRun("run_1");
    await client.cancelRun("run_1", "user requested");
    await client.signalRun("run_1", "approval", { ok: true });
  } finally {
    restore();
  }
  assert.ok(seen.some((url) => url.endsWith("/v1/projects/test-project/runs/run_1")));
  assert.ok(
    seen.some((url) =>
      url.endsWith("/v1/projects/test-project/runs/run_1/cancel"),
    ),
  );
  assert.ok(
    seen.some((url) =>
      url.endsWith("/v1/projects/test-project/runs/run_1/signals"),
    ),
  );
});

test("smoke: waitRun fetches after stream closes before terminal", async () => {
  let getCalls = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    const path = new URL(url).pathname;
    if (path.endsWith("/events.stream")) {
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
      JSON.stringify(loopRun("run_1", getCalls === 1 ? "running" : "completed")),
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

test("smoke: signing helpers verify and parse webhook deliveries", async () => {
  const body = `{"type":"run.completed","data":{"id":"run_1"}}`;
  const key = Buffer.from("01234567890123456789012345678901");
  const signature = signDelivery(key, Buffer.from(body), {
    deliveryId: "delivery_1",
    timestamp: 1710000000,
  }).signature;

  const req = new Request("https://example.invalid/webhooks/mobius", {
    method: "POST",
    body,
    headers: {
      [MOBIUS_SIGNATURE_HEADER]: signature,
      [MOBIUS_SIGNATURE_VERSION_HEADER]: "v1",
      [MOBIUS_TIMESTAMP_HEADER]: "1710000000",
      [MOBIUS_DELIVERY_ID_HEADER]: "delivery_1",
      [MOBIUS_SECRET_REF_HEADER]: "mobius/webhook/test",
      [MOBIUS_SECRET_VERSION_HEADER]: "2",
    },
  });
  const signed = await verifySignedDelivery(req, {
    key,
    now: () => 1710000005,
  });
  const parsed = parseWebhookDelivery<{ id: string }>(signed);
  assert.equal(parsed.type, "run.completed");
  assert.equal(parsed.data.id, "run_1");
  assert.equal(Buffer.from(signed.body).toString("utf8"), body);

  const badReq = new Request("https://example.invalid/webhooks/mobius", {
    method: "POST",
    body,
    headers: {
      [MOBIUS_SIGNATURE_HEADER]: "sha256=00",
      [MOBIUS_SIGNATURE_VERSION_HEADER]: "v1",
      [MOBIUS_TIMESTAMP_HEADER]: "1710000000",
      [MOBIUS_DELIVERY_ID_HEADER]: "delivery_1",
      [MOBIUS_SECRET_REF_HEADER]: "mobius/webhook/test",
      [MOBIUS_SECRET_VERSION_HEADER]: "2",
    },
  });
  await assert.rejects(() =>
    verifySignedDelivery(badReq, { key, now: () => 1710000005 }),
  );
});

test("smoke: synthetic webhook delivery posts signed Mobius envelope", async () => {
  let requestedURL = "";
  let requestBody = "";
  let eventType = "";
  let signature = "";
  let version = "";
  let deliveryID = "";

  const fetchFn = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestBody = String(init?.body ?? "");
    const headers = new Headers(init?.headers);
    eventType = headers.get(WEBHOOK_EVENT_TYPE_HEADER) ?? "";
    signature = headers.get(MOBIUS_SIGNATURE_HEADER) ?? "";
    version = headers.get(MOBIUS_SIGNATURE_VERSION_HEADER) ?? "";
    deliveryID = headers.get(MOBIUS_DELIVERY_ID_HEADER) ?? "";
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  const key = Buffer.from("01234567890123456789012345678901");
  await deliverSyntheticWebhook({
    url: "https://example.invalid/webhooks/mobius",
    key,
    secretRef: "mobius/webhook/test",
    secretVersion: 2,
    deliveryId: "delivery_2",
    timestamp: 1710000000,
    eventType: "run.completed",
    data: { id: "run_1" },
    fetch: fetchFn,
  });

  assert.equal(requestedURL, "https://example.invalid/webhooks/mobius");
  assert.equal(eventType, "run.completed");
  assert.equal(version, "v1");
  assert.equal(deliveryID, "delivery_2");
  assert.equal(
    signature,
    signDelivery(key, Buffer.from(requestBody), {
      deliveryId: "delivery_2",
      timestamp: 1710000000,
    }).signature,
  );
  assert.equal(
    requestBody,
    buildSyntheticWebhookPayload("run.completed", { id: "run_1" }),
  );
});

test("client: terminal run status helper includes cancelled", () => {
  assert.equal(isTerminalRunStatus("completed"), true);
  assert.equal(isTerminalRunStatus("failed"), true);
  assert.equal(isTerminalRunStatus("cancelled"), true);
  assert.equal(isTerminalRunStatus("running"), false);
});

test("client: invokeAgent posts the compound invoke request shape", async () => {
  let requestedURL = "";
  let requestBody = "";
  const restore = installFakeFetch({
    status: 202,
    body: turnAck("sess_1", "turn_1", 7),
    capture: (input, init) => {
      requestedURL = typeof input === "string" ? input : input.toString();
      requestBody = String(init?.body ?? "");
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      project: "test-project",
    });
    const ack = await client.invokeAgent({
      agentId: "agent_1",
      content: [{ type: "text", text: "hi" }],
      idempotencyKey: "evt_1",
      session: { session_key: "app:acct_1:user_2" },
      config: {
        instructions: "Be concise.",
        model: "claude-sonnet-4-6",
        effort: "medium",
        toolkits: [{ name: "tickets", actions: ["tickets.search"] }],
      },
    });
    assert.equal(ack.after_sequence, 7);
    assert.equal(ack.session.id, "sess_1");
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/projects/test-project/agents/invoke",
  );
  assert.match(requestBody, /"agent_ref":\{"id":"agent_1"\}/);
  assert.match(requestBody, /"idempotency_key":"evt_1"/);
  assert.match(requestBody, /"session_key":"app:acct_1:user_2"/);
  assert.match(requestBody, /"config":\{/);
  assert.match(requestBody, /"instructions":"Be concise\."/);
  assert.match(requestBody, /"model":"claude-sonnet-4-6"/);
  assert.match(requestBody, /"effort":"medium"/);
  assert.match(requestBody, /"toolkits":\[\{"name":"tickets","actions":\["tickets\.search"\]\}\]/);
});

test("client: invokeAgent requires agent ref and content", async () => {
  const client = new Client({ apiKey: "mbx_test", project: "test-project" });
  await assert.rejects(() =>
    client.invokeAgent({ content: [{ type: "text", text: "hi" }] }),
  );
  await assert.rejects(() => client.invokeAgent({ agentId: "agent_1", content: [] }));
});

test("client: invokeAgentStream streams session frames inline", async () => {
  let acceptHeader: string | null = null;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    acceptHeader = new Headers(init?.headers).get("Accept");
    return new Response(
      'event: turn.completed\ndata: {"usage":{"input_tokens":42}}\n\n',
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });

  const events = [];
  for await (const ev of client.invokeAgentStream({
    agentName: "support",
    content: [{ type: "text", text: "hi" }],
  })) {
    events.push(ev);
  }

  assert.equal(acceptHeader, "text/event-stream");
  assert.equal(events.length, 1);
  assert.equal(events[0].eventType, "turn.completed");
  assert.equal(
    (events[0].data as { usage: { input_tokens: number } }).usage.input_tokens,
    42,
  );
});

function loopRun(id: string, status: string) {
  return {
    id,
    org_id: "org_1",
    project_id: "proj_1",
    loop_id: "loop_1",
    loop_version_id: "lver_1",
    loop_version: 1,
    status,
    created_at: "2026-05-27T00:00:00Z",
    updated_at: "2026-05-27T00:00:00Z",
  };
}

function turnAck(sessionId: string, turnId: string, afterSequence: number) {
  return {
    after_sequence: afterSequence,
    session: {
      id: sessionId,
      agent_id: "agent_1",
      origin: "api",
      scope: "agent",
      scope_name: "app:acct_1:user_2",
      scope_ref_id: "agent_1",
      session_key: "app:acct_1:user_2",
      status: "active",
      title: "",
      visibility: "private",
      version: 1,
      message_count: 1,
      token_input_total: 0,
      token_output_total: 0,
      created_at: "2026-05-27T00:00:00Z",
      updated_at: "2026-05-27T00:00:00Z",
    },
    turn: {
      id: turnId,
      agent_id: "agent_1",
      session_id: sessionId,
      attempt: 1,
      status: "running",
      created_at: "2026-05-27T00:00:00Z",
      updated_at: "2026-05-27T00:00:00Z",
    },
  };
}
