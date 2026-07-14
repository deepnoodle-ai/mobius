import assert from "node:assert/strict";
import test from "node:test";

import type { Interaction, SessionTranscriptFrame } from "../src/api/index.js";
import { SessionChat } from "../src/chat.js";
import type { Client, TranscriptUpdate } from "../src/client.js";
import { SessionTranscript } from "../src/transcript.js";

const AT = "2026-07-14T12:00:00Z";

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

test("SessionChat exposes pushed interactions, phase changes, and resolution", async () => {
  const transcript = new SessionTranscript();
  const pending = interaction("pending");
  const completed = interaction("completed");
  const waitingTurn = {
    event_type: "turn.upsert",
    id: "turn_1",
    agent_id: "agent_1",
    session_id: "sess_1",
    attempt: 1,
    status: "waiting",
    created_at: AT,
    updated_at: AT,
  } as SessionTranscriptFrame;
  const pendingFrame = {
    event_type: "interaction.upsert",
    ...pending,
  } as SessionTranscriptFrame;
  const completedFrame = {
    event_type: "interaction.upsert",
    ...completed,
  } as SessionTranscriptFrame;

  const frames = [waitingTurn, pendingFrame, completedFrame];
  const client = {
    async *watchSessionTranscriptUpdates(): AsyncGenerator<TranscriptUpdate> {
      for (const frame of frames) {
        transcript.apply({ frame });
        yield {
          frame,
          cursor: transcript.cursor,
          transcript,
          connection: "open",
          reconnectCount: 0,
        };
      }
    },
  } as unknown as Client;

  const phases: Array<string | null> = [];
  const observed: string[] = [];
  const resolved: string[] = [];
  const chat = new SessionChat(client, {
    onPhase: (phase) => phases.push(phase),
    onInteraction: ({ status }) => observed.push(status),
    onInteractionResolved: ({ id }) => resolved.push(id),
  });

  const updates = [];
  for await (const update of chat.watch("sess_1")) updates.push(update);

  assert.deepEqual(phases, ["waiting"]);
  assert.deepEqual(observed, ["pending", "completed"]);
  assert.deepEqual(resolved, ["iact_1"]);
  assert.equal(updates[1].pendingInteractions.length, 1);
  assert.equal(updates[2].pendingInteractions.length, 0);
});

test("SessionChat does not resolve a terminal interaction first seen after reconnect", async () => {
  const transcript = new SessionTranscript();
  const terminalFrame = {
    event_type: "interaction.upsert",
    ...interaction("completed"),
  } as SessionTranscriptFrame;
  const client = {
    async *watchSessionTranscriptUpdates(): AsyncGenerator<TranscriptUpdate> {
      transcript.apply({ frame: terminalFrame });
      yield {
        frame: terminalFrame,
        cursor: transcript.cursor,
        transcript,
        connection: "open",
        reconnectCount: 0,
      };
    },
  } as unknown as Client;

  const observed: string[] = [];
  const resolved: string[] = [];
  const chat = new SessionChat(client, {
    onInteraction: ({ id }) => observed.push(id),
    onInteractionResolved: ({ id }) => resolved.push(id),
  });

  for await (const _ of chat.watch("sess_1")) {
    // Drain the synthetic update.
  }

  assert.deepEqual(observed, ["iact_1"]);
  assert.deepEqual(resolved, []);
});
