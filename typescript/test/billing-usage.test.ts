import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";

async function withMockFetch(
  handler: (url: URL) => Response,
  fn: (client: Client) => Promise<void>,
): Promise<void> {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    return handler(
      new URL(typeof input === "string" ? input : input.toString()),
    );
  }) as typeof fetch;
  try {
    await fn(
      new Client({
        apiKey: "mbx_test",
        baseURL: "https://api.example.invalid",
        project: "test-project",
        retry: 0,
      }),
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
}

test("client: listBillingUsageEvents requires a project and encodes the exact reader filters", async () => {
  await withMockFetch(
    (url) => {
      assert.equal(url.pathname, "/v1/billing/usage-events");
      assert.deepEqual(Object.fromEntries(url.searchParams), {
        project_id: "prj_1",
        recorded_after: "2026-07-22T12:00:00Z",
        period_start: "2026-07-01T00:00:00Z",
        counter: "llm.tokens",
        source_type: "agent_turn",
        source_id: "turn_1",
        run_id: "run_1",
        job_id: "job_1",
        api_key_id: "key_1",
        cursor: "cur_1",
        limit: "25",
      });
      return Response.json({
        items: [],
        has_more: false,
        total_raw_quantity: 0,
        total_credit_cost: 0,
        total_credit_cost_milli: 0,
      });
    },
    async (client) => {
      await client.listBillingUsageEvents({
        projectId: "prj_1",
        recordedAfter: "2026-07-22T12:00:00Z",
        periodStart: "2026-07-01T00:00:00Z",
        counter: "llm.tokens",
        sourceType: "agent_turn",
        sourceId: "turn_1",
        runId: "run_1",
        jobId: "job_1",
        apiKeyId: "key_1",
        cursor: "cur_1",
        limit: 25,
      });
      await assert.rejects(
        client.listBillingUsageEvents({ projectId: "  " }),
        /projectId is required/,
      );
    },
  );
});

test("client: iterateBillingUsageEvents drains chronological cursor pages", async () => {
  const seenCursors: Array<string | null> = [];
  await withMockFetch(
    (url) => {
      seenCursors.push(url.searchParams.get("cursor"));
      const cursor = url.searchParams.get("cursor");
      return Response.json({
        items: [{ id: cursor ? "bue_2" : "bue_1", credit_cost_milli: cursor ? 20 : 10 }],
        has_more: cursor == null,
        next_cursor: cursor == null ? "cur_2" : undefined,
        total_raw_quantity: 2,
        total_credit_cost: 0.03,
        total_credit_cost_milli: 30,
      });
    },
    async (client) => {
      const items = [];
      for await (const item of client.iterateBillingUsageEvents({
        projectId: "prj_1",
        recordedAfter: "2026-07-22T12:00:00Z",
        limit: 1,
      })) {
        items.push([item.id, item.credit_cost_milli]);
      }
      assert.deepEqual(items, [
        ["bue_1", 10],
        ["bue_2", 20],
      ]);
    },
  );
  assert.deepEqual(seenCursors, [null, "cur_2"]);
});

test("client: iterateBillingUsageEvents fails fast on has_more without next_cursor", async () => {
  await withMockFetch(
    () =>
      Response.json({
        items: [{ id: "bue_1", credit_cost_milli: 10 }],
        has_more: true,
        total_raw_quantity: 1,
        total_credit_cost: 0.01,
        total_credit_cost_milli: 10,
      }),
    async (client) => {
      const drained: string[] = [];
      await assert.rejects(
        (async () => {
          for await (const item of client.iterateBillingUsageEvents({
            projectId: "prj_1",
          })) {
            drained.push(item.id);
          }
        })(),
        /has_more without next_cursor/,
      );
      // The first page's item is still yielded before the guard trips.
      assert.deepEqual(drained, ["bue_1"]);
    },
  );
});
