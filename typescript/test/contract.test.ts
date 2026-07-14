import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import type {
  SessionTranscriptFrame,
  SessionTranscriptSnapshot,
  WorkerSocketGenerationDeltaFrame,
  WorkerSocketJobCancelFrame,
  WorkerSocketJobHeartbeatAckFrame,
  WorkerSocketJobHeartbeatFrame,
  WorkerSocketJobReportFrame,
  WorkerSocketJobsClaimFrame,
  WorkerSocketJobsClaimedFrame,
  WorkerSocketRegisterFrame,
  WorkerSocketRegisteredFrame,
} from "../src/api/index.js";
import {
  SessionTranscript,
  normalizeToolUse,
  toolResultText,
} from "../src/transcript.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const contractDir = join(
  __dirname,
  "..",
  "..",
  "..",
  "internal",
  "testdata",
  "contract",
);

interface FixtureEntry {
  file: string;
  schema: string;
  kind: "websocket_frame" | "transcript_frames" | "transcript_snapshot";
  endpoint: string;
}

interface Manifest {
  fixtures: FixtureEntry[];
}

type WorkerFrame =
  | WorkerSocketRegisterFrame
  | WorkerSocketRegisteredFrame
  | WorkerSocketJobsClaimFrame
  | WorkerSocketJobsClaimedFrame
  | WorkerSocketJobHeartbeatFrame
  | WorkerSocketJobHeartbeatAckFrame
  | WorkerSocketJobReportFrame
  | WorkerSocketGenerationDeltaFrame
  | WorkerSocketJobCancelFrame;

const manifest = JSON.parse(
  readFileSync(join(contractDir, "manifest.json"), "utf-8"),
) as Manifest;

assert.ok(manifest.fixtures.length > 0, "manifest has no fixtures");

for (const fixture of manifest.fixtures.filter(
  (entry) => entry.kind === "websocket_frame",
)) {
  test(`contract: ${fixture.file} round-trips as ${fixture.schema}`, () => {
    const raw = readFileSync(join(contractDir, fixture.file), "utf-8");
    const parsed = JSON.parse(raw) as WorkerFrame;
    const roundTripped = JSON.parse(JSON.stringify(parsed)) as unknown;
    assert.deepStrictEqual(roundTripped, JSON.parse(raw));
  });
}

test("contract: transcript frame sequence folds identically", () => {
  const fixture = JSON.parse(
    readFileSync(join(contractDir, "transcript_frames.json"), "utf-8"),
  ) as {
    events: Array<{ id?: string; frame: SessionTranscriptFrame }>;
    expected: {
      cursor: string;
      message_ids: string[];
      renderable_ids: string[];
      renderable_turn_id: string;
      renderable_turn_ids: string[];
      null_content_id: string;
      resolved_action_message_id: string;
      resolved_action_name: string;
      tool_shape_message_id: string;
      flat_resolved_action_name: string;
      meta_resolved_action_name: string;
      help_wire_name: string;
      deduped_tool_block_count: number;
      tool_result_text_message_id: string;
      tool_result_text: string;
      waiting_turn_id: string;
      wait_interaction_id: string;
      wait_tool_call_id: string;
      failed_turn_id: string;
      failed_turn_error_type: string;
      failed_turn_error_message: string;
    };
  };
  const transcript = new SessionTranscript();
  for (const event of fixture.events) transcript.apply(event);

  assert.equal(transcript.cursor, fixture.expected.cursor);
  assert.deepEqual(
    transcript.messages().map((message) => message.id),
    fixture.expected.message_ids,
  );
  const visible = transcript.renderableMessages();
  assert.deepEqual(
    visible.map((message) => message.id),
    fixture.expected.renderable_ids,
  );
  assert.deepEqual(
    transcript
      .renderableMessagesForTurn(fixture.expected.renderable_turn_id)
      .map((message) => message.id),
    fixture.expected.renderable_turn_ids,
  );
  assert.deepEqual(
    transcript.message(fixture.expected.null_content_id)?.content,
    [],
  );
  const resolved = transcript.message(
    fixture.expected.resolved_action_message_id,
  )!.content[0]!;
  assert.equal(
    normalizeToolUse(resolved as never).resolvedAction?.name,
    fixture.expected.resolved_action_name,
  );
  const shapeMessage = transcript.message(
    fixture.expected.tool_shape_message_id,
  )!;
  const flat = normalizeToolUse(shapeMessage.content[0] as never);
  const meta = normalizeToolUse(shapeMessage.content[1] as never);
  const help = normalizeToolUse(shapeMessage.content[2] as never);
  assert.equal(
    flat.resolvedAction?.name,
    fixture.expected.flat_resolved_action_name,
  );
  assert.equal(
    meta.resolvedAction?.name,
    fixture.expected.meta_resolved_action_name,
  );
  assert.equal(help.wireName, fixture.expected.help_wire_name);
  assert.equal(help.resolvedAction, undefined);
  assert.equal(
    visible.find((message) => message.id === "m_final")?.content.length,
    fixture.expected.deduped_tool_block_count,
  );
  const resultMessage = transcript.message(
    fixture.expected.tool_result_text_message_id,
  )!;
  assert.equal(
    toolResultText(resultMessage.content[0] as never),
    fixture.expected.tool_result_text,
  );
  const waiting = transcript.turn(fixture.expected.waiting_turn_id)!;
  assert.equal(
    waiting.wait?.interaction_id,
    fixture.expected.wait_interaction_id,
  );
  assert.equal(
    waiting.wait?.tool_call_id,
    fixture.expected.wait_tool_call_id,
  );
  const failed = transcript.turn(fixture.expected.failed_turn_id)!;
  assert.equal(failed.error_type, fixture.expected.failed_turn_error_type);
  assert.equal(failed.error_message, fixture.expected.failed_turn_error_message);
});

test("contract: transcript snapshot pages converge", () => {
  const fixture = JSON.parse(
    readFileSync(join(contractDir, "transcript_snapshot.json"), "utf-8"),
  ) as {
    pages: SessionTranscriptSnapshot[];
    expected: {
      cursor: string;
      message_ids: string[];
      renderable_ids: string[];
      turn_id: string;
      turn_status: string;
    };
  };
  const transcript = new SessionTranscript();
  for (const page of fixture.pages) transcript.applySnapshot(page);
  assert.equal(transcript.cursor, fixture.expected.cursor);
  assert.deepEqual(
    transcript.messages().map((message) => message.id),
    fixture.expected.message_ids,
  );
  assert.deepEqual(
    transcript.renderableMessages().map((message) => message.id),
    fixture.expected.renderable_ids,
  );
  assert.equal(
    transcript.turn(fixture.expected.turn_id)?.status,
    fixture.expected.turn_status,
  );
});
