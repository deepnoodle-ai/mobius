import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  Client,
  ConfigError,
  DEFAULT_BASE_URL,
} from "../src/client.js";
import type { SessionTranscriptFrame } from "../src/api/index.js";
import {
  WEBHOOK_EVENT_TYPE_HEADER,
  buildSyntheticWebhookPayload,
  deliverSyntheticWebhook,
} from "../src/webhook.js";
import {
  MOBIUS_DELIVERY_ID_HEADER,
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
    const client = new Client({ apiKey: "mbx_test" });
    await client.listAgents();
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    `${DEFAULT_BASE_URL}/v1/agents`,
  );
});

test("client: org resource list helpers", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/blueprints/bindings")) {
      assert.equal(url.searchParams.get("namespace"), "starter");
      assert.equal(url.searchParams.get("blueprint_key"), "support");
      return Response.json({ items: [] });
    }
    if (url.pathname.endsWith("/interactions")) {
      assert.equal(url.searchParams.get("session_id"), "sess_1");
      assert.equal(url.searchParams.get("status"), "pending");
      assert.equal(url.searchParams.get("inbox"), "true");
      return Response.json({ items: [], has_more: false });
    }
    if (url.pathname.endsWith("/permissions")) {
      return Response.json({ items: [], presets: [], action_groups: [] });
    }
    if (url.pathname.endsWith("/principals")) {
      assert.equal(url.searchParams.get("kind"), "service");
      assert.equal(url.searchParams.get("include_disabled"), "true");
      return Response.json({ items: [] });
    }
    if (url.pathname.endsWith("/roles")) {
      assert.equal(url.searchParams.get("cursor"), "role_cursor");
      return Response.json({ items: [], has_more: false });
    }
    if (url.pathname.endsWith("/role-assignments")) {
      assert.equal(url.searchParams.get("principal_id"), "principal_1");
      return Response.json({ items: [] });
    }
    return new Response(null, { status: 404 });
  }) as typeof fetch;
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    assert.deepEqual(
      (
        await client.listBlueprintBindings({
          namespace: "starter",
          blueprintKey: "support",
        })
      ).items,
      [],
    );
    assert.deepEqual(
      (
        await client.listInteractions({
          status: "pending",
          sessionId: "sess_1",
          inbox: true,
        })
      ).items,
      [],
    );
    assert.deepEqual((await client.listOrgPermissions()).items, []);
    assert.deepEqual(
      (
        await client.listPrincipals({
          kind: "service",
          includeDisabled: true,
        })
      ).items,
      [],
    );
    assert.deepEqual(
      (await client.listRoles({ cursor: "role_cursor" })).items,
      [],
    );
    assert.deepEqual(
      (
        await client.listRoleAssignments({ principalId: "principal_1" })
      ).items,
      [],
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("client: workerSocketURL uses websocket scheme", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
  });
  assert.equal(
    client.workerSocketURL(),
    "ws://localhost:8080/v1/workers/socket",
  );
});

test("client: resolves agents by name and sessions by agent name plus key", async () => {
  const seen: string[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    seen.push(url.toString());
    if (url.pathname.endsWith("/agents/agent_1")) {
      return new Response(JSON.stringify({ id: "agent_1", name: "Scout" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/agents")) {
      assert.equal(url.searchParams.get("name"), "Scout");
      return new Response(
        JSON.stringify({ items: [{ id: "agent_1", name: "Scout" }] }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    assert.ok(url.pathname.endsWith("/sessions"));
    assert.equal(url.searchParams.get("agent_name"), "Scout");
    assert.equal(url.searchParams.get("session_key"), "conversation-1");
    return new Response(
      JSON.stringify({
        items: [{ id: "session_1", agent_id: "agent_1", session_key: "conversation-1" }],
        has_more: false,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }) as typeof fetch;

  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    assert.equal((await client.resolveAgent("Scout")).id, "agent_1");
    assert.equal((await client.resolveAgent("Scout")).id, "agent_1");
    assert.equal((await client.getAgent("agent_1")).name, "Scout");
    assert.equal(
      (await client.getSessionByKey("conversation-1", { agentName: "Scout" }))!.id,
      "session_1",
    );
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(seen.filter((url) => url.includes("/agents?name=")).length, 1);
});

test("smoke: signing helpers verify and parse webhook deliveries", async () => {
  const body = `{"type":"ping","data":{"id":"run_1"}}`;
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
      [MOBIUS_SECRET_VERSION_HEADER]: "2",
    },
  });
  const signed = await verifySignedDelivery(req, {
    key,
    now: () => 1710000005,
  });
  const parsed = parseWebhookDelivery<{ id: string }>(signed);
  assert.equal(parsed.type, "ping");
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
    secretVersion: 2,
    deliveryId: "delivery_2",
    timestamp: 1710000000,
    eventType: "ping",
    data: { id: "run_1" },
    fetch: fetchFn,
  });

  assert.equal(requestedURL, "https://example.invalid/webhooks/mobius");
  assert.equal(eventType, "ping");
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
    buildSyntheticWebhookPayload("ping", { id: "run_1" }),
  );
});

test("client: createSession posts the resolve-or-create request", async () => {
  let requestedURL = "";
  let requestMethod = "";
  let requestBody = "";
  const restore = installFakeFetch({
    status: 200,
    body: inputAck("sess_1", "turn_1", 7).session,
    capture: (input, init) => {
      requestedURL = typeof input === "string" ? input : input.toString();
      requestMethod = init?.method ?? "GET";
      requestBody = String(init?.body ?? "");
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    const session = await client.createSession({
      agent_id: "agent_1",
      session_key: "app:acct_1:user_2",
      model_override: "claude-sonnet-5",
    });
    assert.equal(session.id, "sess_1");
  } finally {
    restore();
  }
  assert.equal(requestMethod, "POST");
  assert.equal(requestedURL, "https://api.example.invalid/v1/sessions");
  assert.deepEqual(JSON.parse(requestBody), {
    agent_id: "agent_1",
    session_key: "app:acct_1:user_2",
    model_override: "claude-sonnet-5",
  });
});

test("client: sendSessionInput starts a turn and returns its transcript", async () => {
  let requestedURL = "";
  let requestBody = "";
  let idempotencyHeader: string | null = null;
  const restore = installFakeFetch({
    status: 202,
    body: inputAck("sess_1", "turn_1", 7),
    capture: (input, init) => {
      requestedURL = typeof input === "string" ? input : input.toString();
      requestBody = String(init?.body ?? "");
      idempotencyHeader = new Headers(init?.headers).get("Idempotency-Key");
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    const result = await client.sendSessionInput("sess_1", {
      content: [{ type: "text", text: "hi" }],
      context: [{ name: "naming-board", content: "Chosen: none" }],
      idempotencyKey: "evt_1",
      operation: { timeout_seconds: 90 },
      output: { schema: { type: "object" } },
      metadata: { source: "slack" },
    });
    assert.equal(result.delivery, "new_turn");
    assert.equal(result.sessionId, "sess_1");
    assert.equal(result.turnId, "turn_1");
    assert.equal(result.deduped, false);
    assert.equal(result.wokeTurn, false);
    assert.equal(result.nudge, undefined);
    const turn = result.turn;
    assert.ok(turn);
    assert.equal(turn.afterSequence, 7);
    assert.equal(turn.sessionId, "sess_1");
    assert.equal(turn.id, "turn_1");
    assert.equal(turn.output, undefined);
    assert.equal(turn.outputSource, undefined);
    turn.transcript.apply({
      frame: {
        event_type: "turn.upsert",
        id: "turn_1",
        session_id: "sess_1",
        agent_id: "agent_1",
        attempt: 1,
        status: "completed",
        output: { answer: "42" },
        output_source: "tool",
        created_at: "2026-05-27T00:00:00Z",
        updated_at: "2026-05-27T00:00:01Z",
      } as unknown as SessionTranscriptFrame,
    });
    assert.equal(turn.status, "completed");
    assert.deepEqual(turn.output, { answer: "42" });
    assert.equal(turn.outputSource, "tool");
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/sessions/sess_1/inputs",
  );
  assert.deepEqual(JSON.parse(requestBody), {
    content: [{ type: "text", text: "hi" }],
    idempotency_key: "evt_1",
    operation: { timeout_seconds: 90 },
    context: [{ name: "naming-board", content: "Chosen: none" }],
    output: { schema: { type: "object" } },
    metadata: { source: "slack" },
  });
  assert.equal(idempotencyHeader, "evt_1");
});

test("client: sendSessionInput reports direction for a running turn", async () => {
  let requestBody = "";
  let idempotencyHeader: string | null = null;
  const nudge = {
    id: "nudge_1",
    status: "pending",
    delivery: "current_turn",
    content: "Use the shorter name",
    turn: { id: "turn_1", status: "waiting" },
    sender_principal_id: "principal_1",
    created_at: "2026-07-14T12:00:00Z",
  };
  const ack = inputAck("sess_1", "turn_1", 7);
  const restore = installFakeFetch({
    status: 202,
    body: {
      ...ack,
      delivery: "current_turn",
      user_message: undefined,
      resume_cursor: undefined,
      woke_turn: true,
      nudge,
    },
    capture: (_input, init) => {
      requestBody = String(init?.body ?? "");
      idempotencyHeader = new Headers(init?.headers).get("Idempotency-Key");
    },
  });
  try {
    const client = new Client({ apiKey: "mbx_test" });
    const result = await client.sendSessionInput("sess_1", {
      content: [{ type: "text", text: "Use the shorter name" }],
      delivery: "current_turn",
      idempotencyKey: "  event_2  ",
      wake: true,
    });
    assert.equal(result.delivery, "current_turn");
    assert.equal(result.turnId, "turn_1");
    assert.equal(result.wokeTurn, true);
    assert.equal(result.turn, undefined);
    assert.equal(result.nudge?.id, "nudge_1");
    assert.equal(result.nudge?.status, "pending");
  } finally {
    restore();
  }

  assert.deepEqual(JSON.parse(requestBody), {
    content: [{ type: "text", text: "Use the shorter name" }],
    delivery: "current_turn",
    idempotency_key: "event_2",
    wake: true,
  });
  assert.equal(idempotencyHeader, "event_2");
});

test("client: sendSessionInput follows the new turn when direction finds no running turn", async () => {
  let requestBody = "";
  // No turn could absorb the direction, so the server started one.
  const restore = installFakeFetch({
    status: 202,
    body: inputAck("sess_1", "turn_1", 7),
    capture: (_input, init) => {
      requestBody = String(init?.body ?? "");
    },
  });
  try {
    const client = new Client({ apiKey: "mbx_test" });
    const result = await client.sendSessionInput("sess_1", {
      content: [{ type: "text", text: "Use the shorter name" }],
      delivery: "current_turn",
    });
    assert.equal(result.delivery, "new_turn");
    assert.ok(result.turn);
    assert.equal(result.turn.id, "turn_1");
    assert.equal(result.turn.sessionId, "sess_1");
  } finally {
    restore();
  }
  assert.equal(JSON.parse(requestBody).delivery, "current_turn");
});

test("client: sendSessionInput omits a whitespace-only idempotency key", async () => {
  let idempotencyHeader: string | null = null;
  let requestBody = "";
  const restore = installFakeFetch({
    status: 202,
    body: inputAck("sess_1", "turn_1", 7),
    capture: (_input, init) => {
      requestBody = String(init?.body ?? "");
      idempotencyHeader = new Headers(init?.headers).get("Idempotency-Key");
    },
  });
  try {
    const client = new Client({ apiKey: "mbx_test" });
    await client.sendSessionInput("sess_1", {
      content: [{ type: "text", text: "hi" }],
      idempotencyKey: "  \t  ",
    });
  } finally {
    restore();
  }

  assert.equal("idempotency_key" in JSON.parse(requestBody), false);
  assert.equal(idempotencyHeader, null);
});

test("client: sendSessionInput retries only when it carries an idempotency key", async () => {
  async function callsFor(idempotencyKey?: string): Promise<number> {
    let calls = 0;
    const original = globalThis.fetch;
    globalThis.fetch = (async () => {
      calls += 1;
      if (calls === 1) {
        return new Response(null, {
          status: 503,
          headers: { "Retry-After": "0" },
        });
      }
      return Response.json(inputAck("sess_1", "turn_1", 7), { status: 202 });
    }) as typeof fetch;
    try {
      const client = new Client({ apiKey: "mbx_test", retry: 1 });
      await client
        .sendSessionInput("sess_1", {
          content: [{ type: "text", text: "hi" }],
          idempotencyKey,
        })
        .catch(() => undefined);
    } finally {
      globalThis.fetch = original;
    }
    return calls;
  }

  assert.equal(await callsFor("evt_1"), 2);
  assert.equal(await callsFor(undefined), 1);
});

test("client: listSessionMessages can include runtime context", async () => {
  let requestedURL = "";
  const restore = installFakeFetch({
    status: 200,
    body: {
      items: [
        {
          id: "msg_1",
          session_id: "sess_1",
          agent_id: "agent_1",
          role: "system",
          content: [
            {
              type: "reminder",
              name: "app-board",
              tier: "contextual",
              content: "Chosen: none",
            },
          ],
          entry_type: "message",
          sequence: 1,
          created_at: "2026-07-14T12:00:00Z",
        },
      ],
    },
    capture: (input) => {
      requestedURL = typeof input === "string" ? input : input.toString();
    },
  });
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    const messages = await client.listSessionMessages("sess_1", { include: "context" });
    assert.equal(messages.items[0]?.content[0]?.type, "reminder");
    assert.equal(messages.items[0]?.content[0]?.content, "Chosen: none");
  } finally {
    restore();
  }
  assert.equal(
    requestedURL,
    "https://api.example.invalid/v1/sessions/sess_1/messages?include=context",
  );
});

test("client: session nudge lifecycle routes", async () => {
  const requests: string[] = [];
  const queued = {
    id: "nudge_1",
    status: "pending",
    delivery: "current_turn",
    content: "Use the shorter name",
    turn: { id: "turn_1", status: "waiting" },
    sender_principal_id: "principal_1",
    created_at: "2026-07-14T12:00:00Z",
  };
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    requests.push(`${init?.method ?? "GET"} ${url.pathname}`);
    if (url.pathname.endsWith("/nudges")) {
      assert.equal(url.searchParams.get("status"), "pending");
      assert.equal(url.searchParams.get("order"), "desc");
      return Response.json({ items: [queued], has_more: false });
    }
    if (url.pathname.endsWith("/cancel")) {
      return Response.json({
        ...queued,
        status: "cancelled",
        cancelled_at: "2026-07-14T12:01:00Z",
      });
    }
    return Response.json(queued);
  }) as typeof fetch;
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
    });
    const page = await client.listNudges("s1", {
      status: ["pending"],
      order: "desc",
    });
    assert.equal(page.items[0]?.id, "nudge_1");
    assert.equal((await client.getNudge("s1", "nudge_1")).status, "pending");
    assert.equal(
      (await client.cancelNudge("s1", "nudge_1")).status,
      "cancelled",
    );
  } finally {
    globalThis.fetch = original;
  }
  assert.deepEqual(requests, [
    "GET /v1/sessions/s1/nudges",
    "GET /v1/sessions/s1/nudges/nudge_1",
    "POST /v1/sessions/s1/nudges/nudge_1/cancel",
  ]);
});

test("client: sendSessionInput requires content", async () => {
  const client = new Client({ apiKey: "mbx_test" });
  await assert.rejects(
    () => client.sendSessionInput("sess_1", { content: [] }),
    ConfigError,
  );
  await assert.rejects(
    () =>
      client.sendSessionInput("sess_1", {
        content: undefined as unknown as Record<string, unknown>[],
      }),
    ConfigError,
  );
});

function inputAck(sessionId: string, turnId: string, afterSequence: number) {
  return {
    delivery: "new_turn",
    deduped: false,
    woke_turn: false,
    after_sequence: afterSequence,
    resume_cursor: "41.6",
    session: {
      id: sessionId,
      owner: { kind: "person", id: "user_1" },
      posture: "only_you",
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
