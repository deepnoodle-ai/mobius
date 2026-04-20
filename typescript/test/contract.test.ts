import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import type {
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
} from "../src/api/index.js";
import { Client } from "../src/client.js";

// Contract fixtures at <repo>/internal/testdata/contract are shared with the
// Go and Python SDKs. Each fixture is driven through the real Client against a
// fake fetch. For request fixtures we assert the body the client sends equals
// the fixture. For response fixtures we assert the client parses and returns
// the fixture losslessly. Parity holds when all three languages pass the same
// set.

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

const PROJECT = "test-project";

interface FixtureEntry {
  file: string;
  schema: string;
  kind: "request" | "response";
  endpoint: string;
}

interface Manifest {
  fixtures: FixtureEntry[];
}

function loadManifest(): Manifest {
  return JSON.parse(
    readFileSync(join(contractDir, "manifest.json"), "utf-8"),
  ) as Manifest;
}

function readFixture<T>(file: string): T {
  return JSON.parse(readFileSync(join(contractDir, file), "utf-8")) as T;
}

interface Captured {
  path: string;
  method: string;
  body: unknown;
}

function installFakeFetch(
  reply: { status: number; body?: unknown },
  captured: { last?: Captured },
): () => void {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const path = new URL(url).pathname;
    const bodyRaw = init?.body;
    captured.last = {
      path,
      method: init?.method ?? "GET",
      body: typeof bodyRaw === "string" ? JSON.parse(bodyRaw) : bodyRaw,
    };
    const responseInit: ResponseInit = {
      status: reply.status,
      headers: { "Content-Type": "application/json" },
    };
    const responseBody = reply.body != null ? JSON.stringify(reply.body) : null;
    return new Response(responseBody, responseInit);
  }) as typeof fetch;
  return () => {
    globalThis.fetch = originalFetch;
  };
}

function newClient(): Client {
  return new Client({
    baseURL: "https://api.example.invalid",
    apiKey: "mbx_test",
    project: PROJECT,
  });
}

const manifest = loadManifest();
assert.ok(manifest.fixtures.length > 0, "manifest has no fixtures");

// ---------- request fixtures ----------

test("contract: claim_request_minimal sent verbatim", async () => {
  const fixture = readFixture<JobClaimRequest>("claim_request_minimal.json");
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 204 }, captured);
  try {
    const result = await newClient().claimJob(fixture);
    assert.equal(result, null);
  } finally {
    restore();
  }
  assert.equal(captured.last?.path, `/projects/${PROJECT}/jobs/claim`);
  assert.equal(captured.last?.method, "POST");
  assert.deepStrictEqual(captured.last?.body, fixture);
});

test("contract: claim_request_full sent verbatim", async () => {
  const fixture = readFixture<JobClaimRequest>("claim_request_full.json");
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 204 }, captured);
  try {
    await newClient().claimJob(fixture);
  } finally {
    restore();
  }
  assert.deepStrictEqual(captured.last?.body, fixture);
});

test("contract: heartbeat_job_request sent verbatim", async () => {
  const fixture = readFixture<JobFenceRequest>("heartbeat_job_request.json");
  const reply = { ok: true, directives: {} };
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 200, body: reply }, captured);
  try {
    await newClient().heartbeatJob("job_test", fixture);
  } finally {
    restore();
  }
  assert.equal(
    captured.last?.path,
    `/projects/${PROJECT}/jobs/job_test/heartbeat`,
  );
  assert.deepStrictEqual(captured.last?.body, fixture);
});

test("contract: complete_job_request_success sent verbatim", async () => {
  const fixture = readFixture<JobCompleteRequest>(
    "complete_job_request_success.json",
  );
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 204 }, captured);
  try {
    await newClient().completeJob("job_test", fixture);
  } finally {
    restore();
  }
  assert.equal(
    captured.last?.path,
    `/projects/${PROJECT}/jobs/job_test/complete`,
  );
  assert.deepStrictEqual(captured.last?.body, fixture);
});

test("contract: complete_job_request_failed sent verbatim", async () => {
  const fixture = readFixture<JobCompleteRequest>(
    "complete_job_request_failed.json",
  );
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 204 }, captured);
  try {
    await newClient().completeJob("job_test", fixture);
  } finally {
    restore();
  }
  assert.deepStrictEqual(captured.last?.body, fixture);
});

// ---------- response fixtures ----------

test("contract: claim_response parsed losslessly", async () => {
  const fixture = readFixture<JobClaim>("claim_response.json");
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 200, body: fixture }, captured);
  let job;
  try {
    job = await newClient().claimJob({ worker_id: "worker-abc" });
  } finally {
    restore();
  }
  assert.ok(job, "expected job, got null");
  assert.deepStrictEqual(job, fixture);
});

test("contract: heartbeat_job_response parsed losslessly", async () => {
  const fixture = readFixture<JobHeartbeat>("heartbeat_job_response.json");
  const captured: { last?: Captured } = {};
  const restore = installFakeFetch({ status: 200, body: fixture }, captured);
  let heartbeat: JobHeartbeat;
  try {
    heartbeat = await newClient().heartbeatJob("job_test", {
      worker_id: "worker-abc",
      attempt: 1,
    });
  } finally {
    restore();
  }
  assert.deepStrictEqual(heartbeat, fixture);
});
