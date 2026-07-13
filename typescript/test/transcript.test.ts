import { strict as assert } from "node:assert";
import { test } from "node:test";

import { AuthRevokedError, Client, StreamHTTPError } from "../src/client.js";
import {
  SessionTranscript,
  isTerminalTurnStatus,
} from "../src/transcript.js";
import type {
  SessionTranscriptFrame,
  SessionTranscriptSnapshot,
} from "../src/api/index.js";

const AT = "2026-07-11T17:03:21Z";

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
  assert.equal((t.message("m_a")!.content[0] as { text: string }).text, "hello");
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
      { type: "tool_use", id: "toolu_1", name: "fetch", input: {}, status: "pending" },
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
  block = t.message("m_a")!.content[0] as { status: string; progress?: unknown };
  assert.equal(block.status, "ok");
  assert.equal(block.progress, undefined);
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
  apply(t, { event_type: "stream.ready", session_id: "s1", resume_cursor: "99.9" });
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
    has_more: false,
    resume_cursor: "42.6",
  };
  t.applySnapshot(snap);
  assert.equal(t.message("m_stale"), undefined); // dropped: absent from final page
  assert.notEqual(t.message("m1"), undefined);
  assert.equal(t.cursor, "42.6");
});

test("client: getSessionTranscript builds the snapshot URL with query", async () => {
  let requestedURL = "";
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = typeof input === "string" ? input : input.toString();
    const body: SessionTranscriptSnapshot = {
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
  assert.equal((events[0].frame as { event_type: string }).event_type, "turn.upsert");
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
        user_message: undefined,
        turn: { ...invokeAck.turn, status: "completed" },
      };
      return new Response(JSON.stringify(ack), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.pathname.endsWith("/transcript")) {
      const snap: SessionTranscriptSnapshot = {
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
        has_more: false,
        resume_cursor: "43.9",
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
