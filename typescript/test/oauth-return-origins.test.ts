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

test("client: oauth return-origin allowlist hits the documented routes", async () => {
  const seen: string[] = [];
  await withMockFetch(
    (method, url, init) => {
      seen.push(`${method} ${url.pathname}`);
      if (method === "PUT") {
        assert.deepEqual(JSON.parse(String(init?.body)), {
          origins: ["https://app.partner.example"],
        });
      }
      return Response.json({ origins: ["https://app.partner.example"] });
    },
    async (client) => {
      const current = await client.getOAuthReturnOrigins();
      assert.deepEqual(current.origins, ["https://app.partner.example"]);
      const replaced = await client.replaceOAuthReturnOrigins([
        "https://app.partner.example",
      ]);
      assert.deepEqual(replaced.origins, ["https://app.partner.example"]);
    },
  );
  assert.deepEqual(seen, [
    "GET /v1/organization/oauth-return-origins",
    "PUT /v1/organization/oauth-return-origins",
  ]);
});

test("client: replaceOAuthReturnOrigins sends an empty list to disable embedded return", async () => {
  await withMockFetch(
    (method, _url, init) => {
      assert.equal(method, "PUT");
      // An empty list is the documented way to disable embedded return; the
      // wrapper must pass it through without client-side validation.
      assert.deepEqual(JSON.parse(String(init?.body)), { origins: [] });
      return Response.json({ origins: [] });
    },
    async (client) => {
      const replaced = await client.replaceOAuthReturnOrigins([]);
      assert.deepEqual(replaced.origins, []);
    },
  );
});
