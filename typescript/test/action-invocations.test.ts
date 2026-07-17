import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";

async function withMockFetch(
  handler: (method: string, url: URL, init?: RequestInit) => Response,
  fn: (client: Client) => Promise<void>,
): Promise<void> {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    return handler(init?.method ?? "GET", url, init);
  }) as typeof fetch;
  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      project: "test-project",
      retry: 0,
    });
    await fn(client);
  } finally {
    globalThis.fetch = originalFetch;
  }
}

test("client: listActionInvocations encodes every filter", async () => {
  const queries: Array<Record<string, string>> = [];
  await withMockFetch(
    (_method, url) => {
      assert.equal(
        url.pathname,
        "/v1/projects/test-project/action-invocations",
      );
      queries.push(Object.fromEntries(url.searchParams));
      return Response.json({ items: [], has_more: false });
    },
    async (client) => {
      await client.listActionInvocations({
        runId: "run_1",
        jobId: "job_1",
        environmentId: "env_1",
        actionName: "crm.sync",
        actionId: "act_1",
        definitionScope: "organization",
        secretVersion: 2,
        deliveryId: "dlv_1",
        correlationId: "corr_1",
        status: "failed",
        cursor: "cur_1",
        limit: 25,
      });
      await client.listActionInvocations();
    },
  );
  assert.deepEqual(queries, [
    {
      run_id: "run_1",
      job_id: "job_1",
      environment_id: "env_1",
      action_name: "crm.sync",
      action_id: "act_1",
      definition_scope: "organization",
      secret_version: "2",
      delivery_id: "dlv_1",
      correlation_id: "corr_1",
      status: "failed",
      cursor: "cur_1",
      limit: "25",
    },
    {},
  ]);
});

test("client: listActionInvocations preserves provenance fields", async () => {
  await withMockFetch(
    () =>
      Response.json({
        items: [
          {
            id: "inv_1",
            action_name: "crm.sync",
            action_id: "act_1",
            definition_scope: "organization",
            secret_version: 2,
            delivery_id: "dlv_1",
            correlation_id: "corr_1",
            status: "success",
            source: "loop",
            retry_count: 0,
            started_at: "2026-07-17T00:00:00Z",
            finished_at: "2026-07-17T00:00:01Z",
          },
        ],
        next_cursor: "cur_2",
        has_more: true,
      }),
    async (client) => {
      const page = await client.listActionInvocations();
      assert.equal(page.has_more, true);
      const entry = page.items[0]!;
      assert.equal(entry.action_id, "act_1");
      assert.equal(entry.definition_scope, "organization");
      assert.equal(entry.secret_version, 2);
      assert.equal(entry.delivery_id, "dlv_1");
      assert.equal(entry.correlation_id, "corr_1");
    },
  );
});
