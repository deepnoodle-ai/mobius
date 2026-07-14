import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  AuthRevokedError,
  Client,
  MobiusAPIError,
  StreamHTTPError,
} from "../src/client.js";
import {
  SessionTranscript,
  isTerminalTurnStatus,
  normalizeToolUse,
  textOf,
  toolResultText,
} from "../src/transcript.js";
import type {
  Interaction,
  SessionToolUseBlock,
  SessionTranscriptFrame,
  SessionTranscriptSnapshot,
} from "../src/api/index.js";
import type { TranscriptDiagnostics } from "../src/index.js";

const AT = "2026-07-11T17:03:21Z";

function interaction(status: Interaction["status"]): Interaction {
  return {
    id: "iact_1",
    kind: "request_information",
    status,
    title: "Which region?",
    target_user_ids: ["user_1"],
    created_at: AT,
    updated_at: AT,
  };
}

function frame(f: Record<string, unknown>): SessionTranscriptFrame {
  return f as unknown as SessionTranscriptFrame;
}

// apply folds one frame given as a plain object, with an optional SSE id.
function apply(
  t: SessionTranscript,
  f: Record<string, unknown>,
  id?: string,
): void {
  t.apply({ frame: frame(f), id });
}

function newClient(): Client {
  return new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
}

test("transcript: upsert/block/delta/block-complete converge on a row", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_a",
    session_id: "s1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    content: [],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.block",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    block: { type: "text", text: "" },
  });
  apply(t, {
    event_type: "message.delta",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    text: "hel",
  });
  apply(t, {
    event_type: "message.delta",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    text: "lo",
  });
  assert.equal(
    (t.message("m_a")!.content[0] as { text: string }).text,
    "hello",
  );
  // Completing block replaces whatever deltas built.
  apply(t, {
    event_type: "message.block",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    block: { type: "text", text: "hello world" },
  });
  assert.equal(
    (t.message("m_a")!.content[0] as { text: string }).text,
    "hello world",
  );
});

test("transcript: block at gap index pads content (no sparse holes)", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_a",
    session_id: "s1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    content: [],
    created_at: AT,
  });
  // Opening index 2 on empty content pads indexes 0 and 1 with empty blocks.
  apply(t, {
    event_type: "message.block",
    session_id: "s1",
    message_id: "m_a",
    content_index: 2,
    block: { type: "text", text: "third" },
  });
  const content = t.message("m_a")!.content;
  assert.equal(content.length, 3);
  assert.notEqual(content[0], undefined); // padded, not a sparse hole
  assert.notEqual(content[1], undefined);
  assert.equal((content[2] as { text: string }).text, "third");
  // Deltas target the padded blocks, not a hole.
  apply(t, {
    event_type: "message.delta",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    text: "pad",
  });
  assert.equal((t.message("m_a")!.content[0] as { text: string }).text, "pad");
  // A late message.block fills a padded index in place.
  apply(t, {
    event_type: "message.block",
    session_id: "s1",
    message_id: "m_a",
    content_index: 1,
    block: { type: "text", text: "second" },
  });
  assert.equal(
    (t.message("m_a")!.content[1] as { text: string }).text,
    "second",
  );
});

test("transcript: block.patch merges tool status/progress; null clears", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_a",
    session_id: "s1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    content: [
      {
        type: "tool_use",
        id: "toolu_1",
        name: "fetch",
        input: {},
        status: "pending",
      },
    ],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.block.patch",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    status: "running",
    progress: { display: "scanned 1400 lines" },
  });
  let block = t.message("m_a")!.content[0] as {
    status: string;
    progress?: unknown;
  };
  assert.equal(block.status, "running");
  assert.deepEqual(block.progress, { display: "scanned 1400 lines" });
  // progress: null clears; omitted status preserves.
  apply(t, {
    event_type: "message.block.patch",
    session_id: "s1",
    message_id: "m_a",
    content_index: 0,
    status: "ok",
    progress: null,
  });
  block = t.message("m_a")!.content[0] as {
    status: string;
    progress?: unknown;
  };
  assert.equal(block.status, "ok");
  assert.equal(block.progress, undefined);
});

test("transcript: null content is normalized at apply, snapshot, and seed", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_apply",
    session_id: "s1",
    agent_id: "a1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    entry_type: "message",
    content: null,
    created_at: AT,
  });
  assert.deepEqual(t.message("m_apply")!.content, []);

  t.applySnapshot({
    messages: [
      {
        id: "m_snapshot",
        session_id: "s1",
        agent_id: "a1",
        role: "assistant",
        status: "streaming",
        turn_id: "t1",
        turn_index: 2,
        sequence: null,
        entry_type: "message",
        content: null,
        created_at: AT,
      },
    ],
    turns: [],
    has_more: true,
    resume_cursor: "1.1",
  } as unknown as SessionTranscriptSnapshot);
  assert.deepEqual(t.message("m_snapshot")!.content, []);

  t.seed({
    user_message: {
      id: "m_seed",
      session_id: "s1",
      agent_id: "a1",
      role: "user",
      status: "final",
      turn_id: "t1",
      turn_index: 0,
      sequence: 1,
      entry_type: "message",
      content: null,
      created_at: AT,
    },
    turn: {
      id: "t1",
      session_id: "s1",
      agent_id: "a1",
      attempt: 1,
      status: "queued",
      created_at: AT,
      updated_at: AT,
    },
  } as never);
  assert.deepEqual(t.message("m_seed")!.content, []);
});

test("transcript: renderable projection and content/tool helpers", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "turn.upsert",
    id: "t1",
    session_id: "s1",
    agent_id: "a1",
    attempt: 1,
    status: "running",
    created_at: AT,
    updated_at: AT,
  });
  for (const [id, turnIndex] of [
    ["empty_1", 1],
    ["empty_2", 2],
  ] as const) {
    apply(t, {
      event_type: "message.upsert",
      id,
      session_id: "s1",
      agent_id: "a1",
      role: "assistant",
      status: "streaming",
      turn_id: "t1",
      turn_index: turnIndex,
      sequence: null,
      entry_type: "message",
      content: [],
      created_at: AT,
    });
  }
  apply(t, {
    event_type: "message.upsert",
    id: "preview",
    session_id: "s1",
    agent_id: "a1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 3,
    sequence: null,
    entry_type: "message",
    content: [
      {
        type: "tool_use",
        id: "call_1",
        name: "catalog",
        input: { command: "check", args: { domain: "x.test" } },
      },
    ],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.upsert",
    id: "final",
    session_id: "s1",
    agent_id: "a1",
    role: "assistant",
    status: "final",
    turn_id: "t1",
    turn_index: 3,
    sequence: 4,
    entry_type: "message",
    content: [
      {
        type: "tool_use",
        id: "call_1",
        name: "catalog",
        input: { command: "check", args: { domain: "x.test" } },
        resolved_action: {
          name: "naming.domain.check",
          input: { domain: "x.test" },
        },
      },
      { type: "text", text: "done" },
    ],
    created_at: AT,
  });

  const visible = t.renderableMessages();
  assert.deepEqual(
    visible.map((message) => message.id),
    ["final", "empty_2"],
  );
  const normalized = normalizeToolUse(visible[0]!.content[0] as never);
  assert.equal(normalized.wireName, "catalog");
  assert.equal(normalized.resolvedAction?.name, "naming.domain.check");
  assert.equal(normalized.wireInput.command, "check");
  assert.equal(textOf(visible[0]!), "done");
  assert.equal(toolResultText({ content: "plain" } as never), "plain");
  assert.equal(
    toolResultText({ content: [{ type: "text", text: "blocks" }] } as never),
    "blocks",
  );
  assert.equal(
    toolResultText({
      content: [
        { type: "text", text: "first" },
        { type: "image", source: {} },
        { type: "text", text: "second" },
      ],
    } as never),
    "firstsecond",
  );
});

test("transcript: exports shared diagnostics naming", () => {
  const diagnostics: TranscriptDiagnostics = {
    status: "running",
    cursor: null,
    ready: true,
    reconnectCount: 0,
    connection: "open",
  };
  assert.equal(diagnostics.status, "running");
});

test("transcript: resolved_action patches enrich live tool blocks", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_tool",
    session_id: "s1",
    agent_id: "a1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    entry_type: "message",
    content: [{ type: "tool_use", id: "call_1", name: "wire", input: {} }],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.block.patch",
    session_id: "s1",
    message_id: "m_tool",
    content_index: 0,
    resolved_action: {
      name: "naming.domain.check",
      input: { domain: "x.test" },
    },
  });
  assert.deepEqual(
    (t.message("m_tool")!.content[0] as { resolved_action: unknown })
      .resolved_action,
    { name: "naming.domain.check", input: { domain: "x.test" } },
  );
});

test("transcript: terminal turn prunes its leftover streaming rows", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_live",
    session_id: "s1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 1,
    sequence: null,
    content: [],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.upsert",
    id: "m_final",
    session_id: "s1",
    role: "user",
    status: "final",
    turn_id: "t1",
    turn_index: 0,
    sequence: 42,
    content: [],
    created_at: AT,
  });
  apply(t, {
    event_type: "turn.upsert",
    id: "t1",
    session_id: "s1",
    agent_id: "a1",
    attempt: 1,
    status: "cancelled",
    created_at: AT,
    updated_at: AT,
  });
  assert.equal(t.message("m_live"), undefined); // pruned
  assert.notEqual(t.message("m_final"), undefined); // durable row survives
});

test("transcript: cursor tracks SSE id; stream.ready adopts unconditionally", () => {
  const t = new SessionTranscript();
  apply(
    t,
    {
      event_type: "turn.upsert",
      id: "t1",
      session_id: "s1",
      agent_id: "a1",
      attempt: 1,
      status: "running",
      created_at: AT,
      updated_at: AT,
    },
    "42.7",
  );
  assert.equal(t.cursor, "42.7");
  assert.equal(t.ready, false);
  apply(t, {
    event_type: "stream.ready",
    session_id: "s1",
    resume_cursor: "99.9",
  });
  assert.equal(t.cursor, "99.9");
  assert.equal(t.ready, true);
});

test("transcript: ordering — final by sequence, streaming after, unknown frames ignored", () => {
  const t = new SessionTranscript();
  apply(t, { event_type: "future.frame", whatever: true }); // ignored
  apply(t, {
    event_type: "message.upsert",
    id: "m2",
    session_id: "s1",
    role: "assistant",
    status: "final",
    turn_id: "t1",
    turn_index: 1,
    sequence: 43,
    content: [],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.upsert",
    id: "m1",
    session_id: "s1",
    role: "user",
    status: "final",
    turn_id: "t1",
    turn_index: 0,
    sequence: 42,
    content: [],
    created_at: AT,
  });
  apply(t, {
    event_type: "message.upsert",
    id: "m3",
    session_id: "s1",
    role: "assistant",
    status: "streaming",
    turn_id: "t1",
    turn_index: 2,
    sequence: null,
    content: [],
    created_at: AT,
  });
  assert.deepEqual(
    t.messages().map((m) => m.id),
    ["m1", "m2", "m3"],
  );
});

test("transcript: applySnapshot prunes streaming rows absent from a final page", () => {
  const t = new SessionTranscript();
  apply(t, {
    event_type: "message.upsert",
    id: "m_stale",
    session_id: "s1",
    agent_id: "a1",
    role: "assistant",
    content: [],
    entry_type: "message",
    status: "streaming",
    turn_index: 9,
    sequence: null,
    turn_id: "t0",
    created_at: AT,
  });
  const snap: SessionTranscriptSnapshot = {
    messages: [
      {
        id: "m1",
        session_id: "s1",
        agent_id: "a1",
        role: "user",
        content: [],
        entry_type: "message",
        status: "final",
        turn_index: 0,
        sequence: 42,
        turn_id: "t1",
        created_at: AT,
      },
    ],
    turns: [],
    interactions: [],
    has_more: false,
    resume_cursor: "42.6",
  };
  t.applySnapshot(snap);
  assert.equal(t.message("m_stale"), undefined); // dropped: absent from final page
  assert.notEqual(t.message("m1"), undefined);
  assert.equal(t.cursor, "42.6");
});

test("transcript: interaction upserts and final snapshots reconcile pending state", () => {
  const transcript = new SessionTranscript();
  apply(transcript, {
    event_type: "interaction.upsert",
    ...interaction("pending"),
  });
  assert.deepEqual(
    transcript.pendingInteractions().map(({ id }) => id),
    ["iact_1"],
  );

  apply(transcript, {
    event_type: "interaction.upsert",
    ...interaction("completed"),
  });
  assert.deepEqual(transcript.pendingInteractions(), []);
  assert.equal(transcript.interaction("iact_1")?.status, "completed");

  transcript.applySnapshot({
    messages: [],
    turns: [],
    interactions: [{ ...interaction("pending"), id: "iact_2" }],
    has_more: false,
    resume_cursor: "2.1",
  });
  assert.deepEqual(
    transcript.interactions().map(({ id }) => id),
    ["iact_1", "iact_2"],
  );

  transcript.applySnapshot({
    messages: [],
    turns: [],
    interactions: [],
    has_more: false,
    resume_cursor: "3.1",
  });
  assert.equal(transcript.interaction("iact_2"), undefined);
  assert.equal(transcript.interaction("iact_1")?.status, "completed");
});

test("client: getSessionTranscript builds the snapshot URL with query", async () => {
  let requestedURL = "";
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    const body = {
      messages: [],
      turns: [],
      has_more: false,
      resume_cursor: "1.1",
    };
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  try {
    const snap = await newClient().getSessionTranscript("sess_1", {
      cursor: "10.2",
      limit: 50,
    });
    assert.equal(snap.resume_cursor, "1.1");
    assert.deepEqual(snap.interactions, []);
  } finally {
    globalThis.fetch = original;
  }
  const url = new URL(requestedURL);
  assert.equal(
    url.pathname,
    "/v1/projects/test-project/sessions/sess_1/transcript",
  );
  assert.equal(url.searchParams.get("cursor"), "10.2");
  assert.equal(url.searchParams.get("limit"), "50");
});

test("client: streamSessionTranscript decodes frames with their SSE id", async () => {
  let acceptHeader: string | null = null;
  const original = globalThis.fetch;
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    acceptHeader = new Headers(init?.headers).get("Accept");
    return new Response(
      `id: 42.7\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"${AT}","updated_at":"${AT}"}\n\n` +
        `event: stream.ready\ndata: {"event_type":"stream.ready","session_id":"s1","resume_cursor":"42.7"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  const events = [];
  try {
    for await (const ev of newClient().streamSessionTranscript("sess_1")) {
      events.push(ev);
    }
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(acceptHeader, "text/event-stream");
  assert.equal(events.length, 2);
  assert.equal(events[0].id, "42.7");
  assert.equal(
    (events[0].frame as { event_type: string }).event_type,
    "turn.upsert",
  );
  // The ready frame has no id: line; last-event-id persists as the cursor.
  assert.equal(events[1].id, "42.7");
});

test("client: watchSessionTranscript reconnects on rotate and stops on idle", async () => {
  const cursors: (string | null)[] = [];
  let call = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    cursors.push(url.searchParams.get("cursor"));
    call += 1;
    if (call === 1) {
      return new Response(
        `id: 42.7\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"${AT}","updated_at":"${AT}"}\n\n` +
          `event: stream.end\ndata: {"event_type":"stream.end","session_id":"s1","reason":"rotate"}\n\n`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    return new Response(
      `id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"${AT}","updated_at":"${AT}"}\n\n` +
        `event: stream.end\ndata: {"event_type":"stream.end","session_id":"s1","reason":"idle"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  let last: SessionTranscript | undefined;
  try {
    for await (const t of newClient().watchSessionTranscript("sess_1")) {
      last = t;
    }
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(call, 2); // reconnected once on rotate, stopped on idle
  assert.equal(cursors[0], null); // first connect: no cursor
  assert.equal(cursors[1], "42.7"); // reconnect carries the advanced cursor
  assert.equal(last!.turn("t1")!.status, "completed");
});

test("client: watchSessionTranscript follow reconnects after idle", async () => {
  let calls = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async () => {
    calls += 1;
    if (calls === 1) {
      return new Response(
        `event: stream.end\ndata: {"event_type":"stream.end","session_id":"s1","reason":"idle"}\n\n`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    return new Response(
      `id: 44.1\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t2","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  try {
    for await (const transcript of newClient().watchSessionTranscript(
      "sess_1",
      {
        follow: true,
        reconnectDelayMs: 0,
      },
    )) {
      assert.equal(transcript.turn("t2")!.status, "running");
      break;
    }
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(calls, 2);
});

test("client: watchSessionTranscript surfaces a permanent error, no reconnect", async () => {
  let calls = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async () => {
    calls += 1;
    return new Response(JSON.stringify({ error: { code: "not_found" } }), {
      status: 404,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;

  try {
    await assert.rejects(
      (async () => {
        for await (const t of newClient().watchSessionTranscript("sess_1")) {
          void t;
        }
      })(),
      (err: unknown) => err instanceof StreamHTTPError && err.status === 404,
    );
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(calls, 1); // surfaced on the first attempt, no reconnect loop
});

test("client: ordinary requests expose structured MobiusAPIError", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(
      JSON.stringify({
        error: {
          code: "session_turn_active",
          message: "another direct turn is active",
          details: { turn_id: "turn_blocking", status: "running" },
        },
      }),
      {
        status: 409,
        headers: {
          "Content-Type": "application/json",
          "X-Request-ID": "req_1",
          "Retry-After": "2",
        },
      },
    )) as typeof fetch;
  try {
    await assert.rejects(
      newClient().invokeAgent({
        agentName: "support",
        content: [{ type: "text", text: "next" }],
      }),
      (error: unknown) => {
        assert.ok(error instanceof MobiusAPIError);
        assert.equal(error.status, 409);
        assert.equal(error.code, "session_turn_active");
        assert.deepEqual(error.details, {
          turn_id: "turn_blocking",
          status: "running",
        });
        assert.equal(error.requestId, "req_1");
        assert.equal(error.retryAfter, 2);
        return true;
      },
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("client: nudgeSession is a typed thin wrapper", async () => {
  let requestedURL = "";
  let requestedBody = "";
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    requestedBody = String(init?.body ?? "");
    return new Response(
      JSON.stringify({
        nudge_id: "nudge_1",
        delivery: "current_turn",
        session: { id: "s1" },
        turn: { id: "t1", status: "running" },
        after_sequence: 2,
        deduped: false,
        woke_turn: true,
      }),
      { status: 202, headers: { "Content-Type": "application/json" } },
    );
  }) as typeof fetch;
  try {
    const ack = await newClient().nudgeSession("s1", {
      content: "Use the shorter name",
      idempotencyKey: "event_2",
      wake: true,
    });
    assert.equal(ack.nudge_id, "nudge_1");
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(
    new URL(requestedURL).pathname,
    "/v1/projects/test-project/sessions/s1/nudges",
  );
  assert.deepEqual(JSON.parse(requestedBody), {
    content: "Use the shorter name",
    idempotency_key: "event_2",
    wake: true,
  });
});

test("client: watchSessionTranscript raises AuthRevokedError on 401", async () => {
  let calls = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async () => {
    calls += 1;
    return new Response("", { status: 401 });
  }) as typeof fetch;

  try {
    await assert.rejects(
      (async () => {
        for await (const t of newClient().watchSessionTranscript("sess_1")) {
          void t;
        }
      })(),
      (err: unknown) => err instanceof AuthRevokedError,
    );
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(calls, 1);
});

const invokeAck = {
  after_sequence: 42,
  resume_cursor: "41.6",
  user_message: {
    id: "m_user",
    session_id: "s1",
    agent_id: "a1",
    role: "user",
    content: [{ type: "text", text: "hi" }],
    entry_type: "message",
    status: "final",
    turn_index: 0,
    sequence: 42,
    turn_id: "t1",
    created_at: AT,
  },
  session: { id: "s1", agent_id: "a1" },
  turn: {
    id: "t1",
    agent_id: "a1",
    session_id: "s1",
    attempt: 1,
    status: "queued",
    created_at: AT,
    updated_at: AT,
  },
};

test("client: invokeAgent streams the turn to its terminal upsert", async () => {
  let streamCalls = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      return new Response(JSON.stringify(invokeAck), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript")) {
      assert.equal(url.searchParams.get("cursor"), "41.6");
      return new Response(
        JSON.stringify({
          messages: [],
          turns: [],
          has_more: false,
          resume_cursor: "43.9",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    streamCalls += 1;
    assert.equal(url.searchParams.get("cursor"), "41.6"); // opened from the seeded cursor
    return new Response(
      `event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_a","session_id":"s1","role":"assistant","status":"final","sequence":43,"turn_index":1,"turn_id":"t1","content":[{"type":"text","text":"done"}],"created_at":"${AT}"}\n\n` +
        `id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
    });
    assert.equal(turn.id, "t1");
    assert.equal(turn.sessionId, "s1");
    assert.equal(turn.status, "queued"); // seeded from the invoke response
    assert.equal(turn.deduped, false);
    assert.equal(streamCalls, 0); // lazy: no stream until iteration
    // The caller's message row is seeded before any streaming.
    assert.deepEqual(
      turn.messages().map((m) => m.id),
      ["m_user"],
    );

    let steps = 0;
    for await (const t of turn) {
      steps += 1;
      void t;
    }
    assert.equal(steps, 2); // message.upsert + terminal turn.upsert
    assert.equal(turn.status, "completed");
    assert.deepEqual(
      turn.messages().map((m) => m.id),
      ["m_user", "m_a"],
    );
    assert.equal(turn.transcript.cursor, "43.9");
    assert.equal(turn.diagnostics().status, "completed");
    assert.equal(turn.diagnostics().lastFrameType, "turn.upsert");
    assert.equal(turn.diagnostics().connection, "ended");
  } finally {
    globalThis.fetch = original;
  }
});

test("client: TurnTranscript redrains from the invocation cursor before its final update", async () => {
  const durableMessages: SessionTranscriptSnapshot["messages"] = [
    invokeAck.user_message as SessionTranscriptSnapshot["messages"][number],
    {
      id: "m_call",
      session_id: "s1",
      agent_id: "a1",
      role: "assistant",
      content: [
        {
          type: "tool_use",
          id: "call_1",
          name: "naming_words_coin",
          input: { count: 2 },
          resolved_action: {
            name: "naming.words.coin",
            input: { count: 2 },
          },
        },
      ],
      entry_type: "message",
      status: "final",
      turn_index: 1,
      sequence: 43,
      turn_id: "t1",
      created_at: AT,
    },
    {
      id: "m_result",
      session_id: "s1",
      agent_id: "a1",
      role: "user",
      content: [{ type: "tool_result", tool_use_id: "call_1", content: "ok" }],
      entry_type: "message",
      status: "final",
      turn_index: 2,
      sequence: 44,
      turn_id: "t1",
      created_at: AT,
    },
    {
      id: "m_done",
      session_id: "s1",
      agent_id: "a1",
      role: "assistant",
      content: [{ type: "text", text: "done" }],
      entry_type: "message",
      status: "final",
      turn_index: 3,
      sequence: 45,
      turn_id: "t1",
      created_at: AT,
    },
  ];
  const terminalTurn: SessionTranscriptSnapshot["turns"][number] = {
    id: "t1",
    session_id: "s1",
    agent_id: "a1",
    attempt: 1,
    status: "completed",
    created_at: AT,
    updated_at: AT,
  };
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      return new Response(JSON.stringify(invokeAck), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript/stream")) {
      return new Response(
        `event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_preview","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","sequence":null,"turn_index":1,"turn_id":"t1","entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"naming_words_coin","input":{"count":2}}],"created_at":"${AT}"}\n\n` +
          `id: 44.3\nevent: message.upsert\ndata: ${JSON.stringify({ event_type: "message.upsert", ...durableMessages[2] })}\n\n` +
          `id: 44.9\nevent: turn.upsert\ndata: ${JSON.stringify({ event_type: "turn.upsert", ...terminalTurn })}\n\n`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    if (url.pathname.endsWith("/transcript")) {
      const cursor = url.searchParams.get("cursor");
      const pageToken = url.searchParams.get("page_token");
      let snap: SessionTranscriptSnapshot;
      if (cursor === "44.9") {
        // Real lower-bound filtering cannot redeliver the call at sequence 43.
        snap = {
          messages: durableMessages.slice(3),
          turns: [terminalTurn],
          interactions: [],
          has_more: false,
          resume_cursor: "45.9",
        };
      } else if (pageToken === "terminal-page-2") {
        snap = {
          messages: durableMessages.slice(2),
          turns: [terminalTurn],
          interactions: [],
          has_more: false,
          resume_cursor: "45.9",
        };
      } else {
        assert.equal(cursor, "41.6");
        snap = {
          messages: durableMessages.slice(0, 2),
          turns: [],
          interactions: [],
          has_more: true,
          next_page_token: "terminal-page-2",
          resume_cursor: "43.3",
        };
      }
      return new Response(JSON.stringify(snap), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    throw new Error(`unexpected request: ${url.pathname}`);
  }) as typeof fetch;

  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
    });
    const updates = [];
    for await (const update of turn.updates()) updates.push(update);

    assert.equal(updates.length, 3);
    const finalUpdate = updates[updates.length - 1];
    assert.equal(finalUpdate.connection, "ended");
    assert.equal(finalUpdate.cursor, "45.9");
    const fullSnapshot = new SessionTranscript();
    fullSnapshot.applySnapshot({
      messages: durableMessages,
      turns: [terminalTurn],
      interactions: [],
      has_more: false,
      resume_cursor: "45.9",
    });
    const finalMessages =
      finalUpdate.transcript.renderableMessagesForTurn("t1");
    assert.deepEqual(
      finalMessages,
      fullSnapshot.renderableMessagesForTurn("t1"),
    );
    const toolUse = finalMessages
      .flatMap((message) => message.content)
      .find((block) => block.type === "tool_use") as SessionToolUseBlock;
    assert.equal(
      normalizeToolUse(toolUse).resolvedAction?.name,
      "naming.words.coin",
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("client: deduped in-flight turn replays from its stable cursor", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      return new Response(
        JSON.stringify({
          ...invokeAck,
          deduped: true,
          user_message: undefined,
          resume_cursor: "41.6",
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      );
    }
    if (url.pathname.endsWith("/transcript/stream")) {
      assert.equal(url.searchParams.get("cursor"), "41.6");
      return new Response(
        `id: 45.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    if (url.pathname.endsWith("/transcript")) {
      assert.equal(url.searchParams.get("cursor"), "41.6");
      const snap: SessionTranscriptSnapshot = {
        messages: [
          {
            id: "m_call",
            session_id: "s1",
            agent_id: "a1",
            role: "assistant",
            content: [
              {
                type: "tool_use",
                id: "call_1",
                name: "naming_words_coin",
                input: { count: 2 },
                resolved_action: {
                  name: "naming.words.coin",
                  input: { count: 2 },
                },
              },
            ],
            entry_type: "message",
            status: "final",
            turn_index: 1,
            sequence: 43,
            turn_id: "t1",
            created_at: AT,
          },
        ],
        turns: [{ ...invokeAck.turn, status: "completed" }],
        interactions: [],
        has_more: false,
        resume_cursor: "45.9",
      };
      return new Response(JSON.stringify(snap), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    throw new Error(`unexpected request: ${url.pathname}`);
  }) as typeof fetch;
  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
      idempotencyKey: "evt_1",
    });
    for await (const _ of turn) {
      // Fold through terminal reconciliation.
    }
    const toolUse = turn
      .renderableMessages()
      .flatMap((message) => message.content)
      .find((block) => block.type === "tool_use") as SessionToolUseBlock;
    assert.equal(
      normalizeToolUse(toolUse).resolvedAction?.name,
      "naming.words.coin",
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("client: TurnTranscript surfaces terminal snapshot reconciliation errors", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      return new Response(JSON.stringify(invokeAck), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript/stream")) {
      return new Response(
        `id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      );
    }
    if (url.pathname.endsWith("/transcript")) {
      return new Response(
        JSON.stringify({ error: { code: "snapshot_unavailable" } }),
        { status: 500, headers: { "Content-Type": "application/json" } },
      );
    }
    throw new Error(`unexpected request: ${url.pathname}`);
  }) as typeof fetch;

  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
    });
    await assert.rejects(
      (async () => {
        for await (const update of turn.updates()) void update;
      })(),
      /snapshot_unavailable/,
    );
    assert.equal(turn.status, "completed");
  } finally {
    globalThis.fetch = original;
  }
});

test("client: TurnTranscript exposes live failed-turn errors", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      return new Response(JSON.stringify(invokeAck), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript")) {
      return new Response(
        JSON.stringify({
          messages: [],
          turns: [],
          has_more: false,
          resume_cursor: "43.9",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    return new Response(
      `id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"failed","error_type":"invalid_conversation_state","error_message":"history ended with assistant content","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;
  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
    });
    for await (const _ of turn) {
      // Fold the terminal update.
    }
    assert.equal(turn.errorType, "invalid_conversation_state");
    assert.equal(turn.errorMessage, "history ended with assistant content");
    assert.equal(turn.error?.name, "invalid_conversation_state");
  } finally {
    globalThis.fetch = original;
  }
});

test("client: invokeAgent rejects a blank resume cursor", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ ...invokeAck, resume_cursor: " " }), {
      status: 202,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;
  try {
    await assert.rejects(
      newClient().invokeAgent({
        agentName: "support",
        content: [{ type: "text", text: "hi" }],
      }),
      /resume_cursor/,
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("client: invokeAgent hydrates an already-terminal turn from the snapshot", async () => {
  let streamCalls = 0;
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      const ack = {
        ...invokeAck,
        deduped: true,
        resume_cursor: "31.6",
        user_message: undefined,
        turn: { ...invokeAck.turn, status: "completed" },
      };
      return new Response(JSON.stringify(ack), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript")) {
      // Two pages: hydration must follow next_page_token until has_more is
      // false so messages() includes the older page.
      const pageToken = url.searchParams.get("page_token");
      if (!pageToken) assert.equal(url.searchParams.get("cursor"), "31.6");
      const snap: SessionTranscriptSnapshot =
        pageToken === "pt_2"
          ? {
              messages: [
                {
                  id: "m_a",
                  session_id: "s1",
                  agent_id: "a1",
                  role: "assistant",
                  content: [{ type: "text", text: "done" }],
                  entry_type: "message",
                  status: "final",
                  turn_index: 1,
                  sequence: 43,
                  turn_id: "t1",
                  created_at: AT,
                },
              ],
              turns: [],
              interactions: [],
              has_more: false,
              resume_cursor: "43.9",
            }
          : {
              messages: [
                {
                  id: "m_user",
                  session_id: "s1",
                  agent_id: "a1",
                  role: "user",
                  content: [],
                  entry_type: "message",
                  status: "final",
                  turn_index: 0,
                  sequence: 42,
                  turn_id: "t1",
                  created_at: AT,
                },
              ],
              turns: [],
              interactions: [],
              has_more: true,
              next_page_token: "pt_2",
              resume_cursor: "42.1",
            };
      return new Response(JSON.stringify(snap), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    streamCalls += 1;
    throw new Error(`unexpected request: ${url.pathname}`);
  }) as typeof fetch;

  try {
    const turn = await newClient().invokeAgent({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
      idempotencyKey: "evt_1",
    });
    assert.equal(turn.deduped, true);
    assert.equal(turn.status, "completed");

    let steps = 0;
    for await (const t of turn) {
      steps += 1;
      void t;
    }
    assert.equal(steps, 1); // one snapshot hydration
    assert.equal(streamCalls, 0); // no SSE connection for a finished turn
    assert.deepEqual(
      turn.messages().map((m) => m.id),
      ["m_user", "m_a"],
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("isTerminalTurnStatus covers the three terminal states", () => {
  assert.equal(isTerminalTurnStatus("completed"), true);
  assert.equal(isTerminalTurnStatus("failed"), true);
  assert.equal(isTerminalTurnStatus("cancelled"), true);
  assert.equal(isTerminalTurnStatus("running"), false);
  assert.equal(isTerminalTurnStatus("queued"), false);
});
