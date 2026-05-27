import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import type {
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
  kind: "websocket_frame";
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

for (const fixture of manifest.fixtures) {
  test(`contract: ${fixture.file} round-trips as ${fixture.schema}`, () => {
    const raw = readFileSync(join(contractDir, fixture.file), "utf-8");
    const parsed = JSON.parse(raw) as WorkerFrame;
    const roundTripped = JSON.parse(JSON.stringify(parsed)) as unknown;
    assert.deepStrictEqual(roundTripped, JSON.parse(raw));
  });
}
