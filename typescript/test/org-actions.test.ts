import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";
import type { OrganizationAction } from "../src/api/index.js";

function orgAction(
  secret: string | undefined,
  statuses: Array<"pending" | "active" | "retiring" | "retired" | "revoked">,
): OrganizationAction {
  return {
    id: "act_1",
    name: "crm.sync",
    endpoint_url: "https://example.com/hook",
    invocation_format: "signed_context_v1",
    enabled: true,
    secret_ref: "osec_abc",
    signing_secret: secret,
    secret_versions: statuses.map((status, i) => ({
      version: i + 1,
      status,
      created_at: "2026-07-17T00:00:00Z",
    })),
    created_at: "2026-07-17T00:00:00Z",
    updated_at: "2026-07-17T00:00:00Z",
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

test("client: createOrganizationAction returns decoded secret material", async () => {
  const key = "super-secret-signing-key-32bytes";
  const encoded = btoa(key);
  await withMockFetch(
    (method, url, init) => {
      assert.equal(method, "POST");
      assert.equal(url.pathname, "/v1/organization/actions");
      assert.equal(JSON.parse(String(init?.body)).name, "crm.sync");
      return Response.json(orgAction(encoded, ["active"]), { status: 201 });
    },
    async (client) => {
      const material = await client.createOrganizationAction({
        name: "crm.sync",
        endpoint_url: "https://example.com/hook",
      });
      assert.equal(new TextDecoder().decode(material.keyBytes), key);
      assert.equal(material.secretRef, "osec_abc");
      assert.equal(material.version, 1);
      assert.equal(material.action.signing_secret, undefined);
    },
  );
});

test("client: rotateOrganizationActionSecret attributes the key to the pending version", async () => {
  await withMockFetch(
    (_method, url) => {
      assert.equal(
        url.pathname,
        "/v1/organization/actions/act_1/secret/rotate",
      );
      return Response.json(orgAction(btoa("rotated-key"), ["active", "pending"]));
    },
    async (client) => {
      const material = await client.rotateOrganizationActionSecret("act_1");
      assert.equal(material.version, 2);
      assert.equal(new TextDecoder().decode(material.keyBytes), "rotated-key");
    },
  );
});

test("client: secret material extraction rejects inconsistent responses", async () => {
  const cases: Array<{ action: OrganizationAction; match: RegExp }> = [
    {
      action: orgAction(undefined, ["active"]),
      match: /missing the one-time signing_secret/,
    },
    {
      action: orgAction(btoa("key"), ["active", "revoked"]),
      match: /status "revoked", want "active"/,
    },
    {
      action: orgAction("not base64 !!!", ["active"]),
      match: /not valid base64/,
    },
  ];
  for (const { action, match } of cases) {
    await withMockFetch(
      () => Response.json(action, { status: 201 }),
      async (client) => {
        await assert.rejects(
          client.createOrganizationAction({
            name: "crm.sync",
            endpoint_url: "https://example.com/hook",
          }),
          match,
        );
      },
    );
  }
});

test("client: org action admin methods hit the documented routes", async () => {
  const seen: string[] = [];
  await withMockFetch(
    (method, url, init) => {
      seen.push(`${method} ${url.pathname}`);
      if (method === "DELETE") return new Response(null, { status: 204 });
      if (method === "PATCH") {
        assert.deepEqual(JSON.parse(String(init?.body)), { enabled: false });
        return Response.json(orgAction(undefined, ["active"]));
      }
      if (url.pathname === "/v1/organization/actions") {
        assert.equal(url.searchParams.get("limit"), "10");
        return Response.json({
          items: [orgAction(undefined, ["active"])],
          has_more: false,
        });
      }
      return Response.json(orgAction(undefined, ["active"]));
    },
    async (client) => {
      const page = await client.listOrganizationActions({ limit: 10 });
      assert.equal(page.items.length, 1);
      await client.getOrganizationAction("act_1");
      await client.updateOrganizationAction("act_1", { enabled: false });
      await client.deleteOrganizationAction("act_1");
    },
  );
  assert.deepEqual(seen, [
    "GET /v1/organization/actions",
    "GET /v1/organization/actions/act_1",
    "PATCH /v1/organization/actions/act_1",
    "DELETE /v1/organization/actions/act_1",
  ]);
});

test("client: activate sends explicit zero overlap and validates the range", async () => {
  const bodies: unknown[] = [];
  await withMockFetch(
    (_method, url, init) => {
      assert.equal(
        url.pathname,
        "/v1/organization/actions/act_1/secret/versions/2/activate",
      );
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json(orgAction(undefined, ["retiring", "active"]));
    },
    async (client) => {
      await client.activateOrganizationActionSecretVersion("act_1", 2, {
        overlapSeconds: 0,
      });
      await assert.rejects(
        client.activateOrganizationActionSecretVersion("act_1", 2, {
          overlapSeconds: 86401,
        }),
        /between 0 and 86400/,
      );
    },
  );
  assert.deepEqual(bodies, [{ overlap_seconds: 0 }]);
});
