import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client, MobiusAPIError } from "../src/client.js";
import type {
  AgentMemoryChange,
  AgentMemoryEntry,
} from "../src/api/index.js";

function entry(entryId: string, key: string): AgentMemoryEntry {
  return {
    key,
    kind: "fact",
    entry_id: entryId,
    importance: 50,
    pinned: false,
    version: 1,
    created_at: "2026-07-17T00:00:00Z",
    updated_at: "2026-07-17T00:00:00Z",
  };
}

function change(changeId: string, version: number): AgentMemoryChange {
  return {
    id: changeId,
    agent_id: "agent_1",
    memory_entry_id: "mem_1",
    memory_key: "prefs",
    operation: "updated",
    version,
    reason: "remembered",
    created_at: "2026-07-17T00:00:00Z",
  };
}

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

test("client: listAgentMemoryEntries encodes search params and preserves coverage", async () => {
  let capturedURL: URL | undefined;
  await withMockFetch(
    (_method, url) => {
      capturedURL = url;
      return Response.json({
        items: [entry("mem_1", "prefs")],
        has_more: false,
        search_coverage: { indexed_entries: 9, total_entries: 10, complete: false },
      });
    },
    async (client) => {
      const page = await client.listAgentMemoryEntries("agent_1", {
        query: "preferences",
        searchMode: "hybrid",
        kind: "fact",
        cursor: "cur_1",
        limit: 25,
      });
      assert.equal(page.items[0]?.key, "prefs");
      assert.equal(page.search_coverage?.complete, false);
      assert.equal(page.search_coverage?.indexed_entries, 9);
    },
  );

  assert.ok(capturedURL);
  assert.equal(
    capturedURL.pathname,
    "/v1/projects/test-project/agents/agent_1/memory/entries",
  );
  assert.equal(capturedURL.searchParams.get("query"), "preferences");
  assert.equal(capturedURL.searchParams.get("search_mode"), "hybrid");
  assert.equal(capturedURL.searchParams.get("kind"), "fact");
  assert.equal(capturedURL.searchParams.get("cursor"), "cur_1");
  assert.equal(capturedURL.searchParams.get("limit"), "25");
});

test("client: semantic search unavailable is surfaced, not downgraded", async () => {
  const modes: Array<string | null> = [];
  await withMockFetch(
    (_method, url) => {
      modes.push(url.searchParams.get("search_mode"));
      return Response.json(
        {
          error: {
            code: "memory_semantic_search_unavailable",
            message: "index offline",
          },
        },
        { status: 503 },
      );
    },
    async (client) => {
      await assert.rejects(
        client.listAgentMemoryEntries("agent_1", {
          query: "preferences",
          searchMode: "semantic",
        }),
        (err: unknown) => {
          assert.ok(err instanceof MobiusAPIError);
          assert.equal(err.status, 503);
          assert.equal(err.code, "memory_semantic_search_unavailable");
          return true;
        },
      );
    },
  );
  assert.deepEqual(modes, ["semantic"]);
});

test("client: memory summary, save, and delete hit the documented routes", async () => {
  const seen: string[] = [];
  await withMockFetch(
    (method, url, init) => {
      seen.push(`${method} ${url.pathname}`);
      if (method === "GET") {
        return Response.json({
          agent_id: "agent_1",
          entry_count: 2,
          counts_by_kind: { fact: 2 },
          updated_at: "2026-07-17T00:00:00Z",
        });
      }
      if (method === "PUT") {
        assert.equal(
          JSON.parse(String(init?.body)).content,
          "dark mode",
        );
        return Response.json(entry("mem_1", "prefs"), { status: 201 });
      }
      return new Response(null, { status: 204 });
    },
    async (client) => {
      const memory = await client.getAgentMemory("agent_1");
      assert.equal(memory.entry_count, 2);
      assert.equal(memory.counts_by_kind?.fact, 2);

      const saved = await client.saveAgentMemoryEntry("agent_1", "prefs", {
        content: "dark mode",
      });
      assert.equal(saved.entry_id, "mem_1");

      await client.deleteAgentMemoryEntry("agent_1", "prefs");
    },
  );

  assert.deepEqual(seen, [
    "GET /v1/projects/test-project/agents/agent_1/memory",
    "PUT /v1/projects/test-project/agents/agent_1/memory/entries/prefs",
    "DELETE /v1/projects/test-project/agents/agent_1/memory/entries/prefs",
  ]);
});

test("client: syncAgentMemory drains change pages", async () => {
  await withMockFetch(
    (_method, url) => {
      assert.equal(
        url.pathname,
        "/v1/projects/test-project/agents/agent_1/memory/changes",
      );
      if (url.searchParams.get("after") === "cur_0") {
        return Response.json({
          items: [change("chg_1", 1)],
          has_more: true,
          next_cursor: "cur_1",
        });
      }
      assert.equal(url.searchParams.get("after"), "cur_1");
      return Response.json({
        items: [change("chg_2", 2)],
        has_more: false,
        next_cursor: "cur_2",
      });
    },
    async (client) => {
      const result = await client.syncAgentMemory("agent_1", "cur_0");
      assert.equal(result.reset, false);
      assert.ok(!result.reset);
      assert.deepEqual(
        result.changes.map((c) => c.id),
        ["chg_1", "chg_2"],
      );
      assert.equal(result.nextCursor, "cur_2");
    },
  );
});

test("client: syncAgentMemory recovers from an expired cursor with a snapshot", async () => {
  await withMockFetch(
    (_method, url) => {
      if (url.pathname.endsWith("/memory/changes")) {
        if (url.searchParams.get("after") === "cur_stale") {
          return Response.json(
            {
              error: {
                code: "memory_cursor_expired",
                message: "cursor predates retained history",
              },
            },
            { status: 410 },
          );
        }
        return Response.json({
          items: [change("chg_9", 9)],
          has_more: false,
          next_cursor: "cur_fresh",
        });
      }
      assert.ok(url.pathname.endsWith("/memory/entries"));
      if (url.searchParams.get("cursor") === "ecur_1") {
        return Response.json({
          items: [entry("mem_2", "style")],
          has_more: false,
        });
      }
      return Response.json({
        items: [entry("mem_1", "prefs")],
        has_more: true,
        next_cursor: "ecur_1",
      });
    },
    async (client) => {
      const result = await client.syncAgentMemory("agent_1", "cur_stale");
      assert.equal(result.reset, true);
      assert.ok(result.reset);
      assert.deepEqual(
        result.entries.map((e) => e.key),
        ["prefs", "style"],
      );
      assert.equal(result.nextCursor, "cur_fresh");
    },
  );
});

test("client: syncAgentMemory propagates non-cursor errors", async () => {
  await withMockFetch(
    () =>
      Response.json(
        { error: { code: "not_found", message: "no such agent" } },
        { status: 404 },
      ),
    async (client) => {
      await assert.rejects(
        client.syncAgentMemory("agent_missing"),
        (err: unknown) => {
          assert.ok(err instanceof MobiusAPIError);
          assert.equal(err.status, 404);
          return true;
        },
      );
    },
  );
});
