import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";
import {
  SessionTranscriptReducer,
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

function newClient(): Client {
  return new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    project: "test-project",
  });
}

test("reducer: upsert/block/delta/block-complete converge on a row", () => {
  const r = new SessionTranscriptReducer();
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
      event_type: "message.block",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      block: { type: "text", text: "" },
    }),
  );
  r.apply(
    frame({
      event_type: "message.delta",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      text: "hel",
    }),
  );
  r.apply(
    frame({
      event_type: "message.delta",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      text: "lo",
    }),
  );
  assert.equal((r.rows.get("m_a")!.content[0] as { text: string }).text, "hello");
  // Completing block replaces whatever deltas built.
  r.apply(
    frame({
      event_type: "message.block",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      block: { type: "text", text: "hello world" },
    }),
  );
  assert.equal(
    (r.rows.get("m_a")!.content[0] as { text: string }).text,
    "hello world",
  );
});

test("reducer: block.patch merges tool status/progress; null clears", () => {
  const r = new SessionTranscriptReducer();
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
      event_type: "message.block.patch",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      status: "running",
      progress: { display: "scanned 1400 lines" },
    }),
  );
  let block = r.rows.get("m_a")!.content[0] as {
    status: string;
    progress?: unknown;
  };
  assert.equal(block.status, "running");
  assert.deepEqual(block.progress, { display: "scanned 1400 lines" });
  // progress: null clears; omitted status preserves.
  r.apply(
    frame({
      event_type: "message.block.patch",
      session_id: "s1",
      message_id: "m_a",
      content_index: 0,
      status: "ok",
      progress: null,
    }),
  );
  block = r.rows.get("m_a")!.content[0] as { status: string; progress?: unknown };
  assert.equal(block.status, "ok");
  assert.equal(block.progress, undefined);
});

test("reducer: terminal turn prunes its leftover streaming rows", () => {
  const r = new SessionTranscriptReducer();
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
      event_type: "turn.upsert",
      id: "t1",
      session_id: "s1",
      agent_id: "a1",
      attempt: 1,
      status: "cancelled",
      created_at: AT,
      updated_at: AT,
    }),
  );
  assert.equal(r.rows.has("m_live"), false); // pruned
  assert.equal(r.rows.has("m_final"), true); // durable row survives
});

test("reducer: cursor tracks SSE id; stream.ready adopts unconditionally", () => {
  const r = new SessionTranscriptReducer();
  r.apply(
    frame({
      event_type: "turn.upsert",
      id: "t1",
      session_id: "s1",
      agent_id: "a1",
      attempt: 1,
      status: "running",
      created_at: AT,
      updated_at: AT,
    }),
    "42.7",
  );
  assert.equal(r.cursor, "42.7");
  assert.equal(r.ready, false);
  r.apply(
    frame({ event_type: "stream.ready", session_id: "s1", resume_cursor: "99.9" }),
  );
  assert.equal(r.cursor, "99.9");
  assert.equal(r.ready, true);
});

test("reducer: ordering — final by sequence, streaming after, unknown frames ignored", () => {
  const r = new SessionTranscriptReducer();
  r.apply(frame({ event_type: "future.frame", whatever: true })); // ignored
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
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
    }),
  );
  r.apply(
    frame({
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
    }),
  );
  assert.deepEqual(
    r.messages().map((m) => m.id),
    ["m1", "m2", "m3"],
  );
});

test("reducer: applySnapshot prunes streaming rows absent from a final page", () => {
  const r = new SessionTranscriptReducer();
  r.rows.set("m_stale", {
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
  r.applySnapshot(snap);
  assert.equal(r.rows.has("m_stale"), false); // dropped: absent from final page
  assert.equal(r.rows.has("m1"), true);
  assert.equal(r.cursor, "42.6");
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

  let last: SessionTranscriptReducer | undefined;
  try {
    for await (const r of newClient().watchSessionTranscript("sess_1")) {
      last = r;
    }
  } finally {
    globalThis.fetch = original;
  }
  assert.equal(call, 2); // reconnected once on rotate, stopped on idle
  assert.equal(cursors[0], null); // first connect: no cursor
  assert.equal(cursors[1], "42.7"); // reconnect carries the advanced cursor
  assert.equal(last!.turns.get("t1")!.status, "completed");
});

test("client: invokeAgentTranscript streams a turn to its terminal upsert", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    if (url.pathname.endsWith("/agents/invoke")) {
      const ack = {
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
      return new Response(JSON.stringify(ack), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response(
      `event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_a","session_id":"s1","role":"assistant","status":"final","sequence":43,"turn_index":1,"turn_id":"t1","content":[{"type":"text","text":"done"}],"created_at":"${AT}"}\n\n` +
        `id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"${AT}","updated_at":"${AT}"}\n\n`,
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  }) as typeof fetch;

  let last: SessionTranscriptReducer | undefined;
  try {
    for await (const r of newClient().invokeAgentTranscript({
      agentName: "support",
      content: [{ type: "text", text: "hi" }],
    })) {
      last = r;
    }
  } finally {
    globalThis.fetch = original;
  }
  // Seeded caller row + assistant reply are both present; turn is terminal.
  assert.equal(last!.turns.get("t1")!.status, "completed");
  assert.deepEqual(
    last!.messagesForTurn("t1").map((m) => m.id),
    ["m_user", "m_a"],
  );
});

test("isTerminalTurnStatus covers the three terminal states", () => {
  assert.equal(isTerminalTurnStatus("completed"), true);
  assert.equal(isTerminalTurnStatus("failed"), true);
  assert.equal(isTerminalTurnStatus("cancelled"), true);
  assert.equal(isTerminalTurnStatus("running"), false);
  assert.equal(isTerminalTurnStatus("queued"), false);
});
