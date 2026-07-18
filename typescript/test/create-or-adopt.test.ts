import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client, ConfigError, MobiusAPIError } from "../src/client.js";
import type { Agent, Project } from "../src/api/index.js";

function agent(id = "agent_1"): Agent {
  return {
    id,
    principal_id: id,
    name: "PR reviewer",
    status: "active",
    external_ref: "tenant-42/pr-reviewer",
    created_at: "2026-07-17T00:00:00Z",
    updated_at: "2026-07-17T00:00:00Z",
  } as Agent;
}

function project(id = "prj_1"): Project {
  return {
    id,
    name: "Product Ops",
    handle: "product-ops",
    access_mode: "restricted",
    external_ref: "workspace-42",
    created_at: "2026-07-17T00:00:00Z",
    updated_at: "2026-07-17T00:00:00Z",
  } as Project;
}

async function withMockFetch(
  handler: (method: string, url: URL, init?: RequestInit) => Response,
  fn: (client: Client) => Promise<void>,
  opts: { retry?: number } = {},
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
      retry: opts.retry ?? 0,
    });
    await fn(client);
  } finally {
    globalThis.fetch = originalFetch;
  }
}

test("client: createAgent adopt sends if_exists/external_ref and accepts 200", async () => {
  await withMockFetch(
    (method, url, init) => {
      assert.equal(method, "POST");
      assert.equal(url.pathname, "/v1/projects/test-project/agents");
      const body = JSON.parse(String(init?.body));
      assert.equal(body.if_exists, "adopt");
      assert.equal(body.external_ref, "tenant-42/pr-reviewer");
      // Adopt of an existing agent answers 200, not 201.
      return Response.json(agent(), { status: 200 });
    },
    async (client) => {
      const created = await client.createAgent(
        { name: "PR reviewer" },
        { adoptExisting: true, externalRef: "tenant-42/pr-reviewer" },
      );
      assert.equal(created.id, "agent_1");
    },
  );
});

test("client: createAgent plain create omits if_exists and accepts 201", async () => {
  await withMockFetch(
    (_method, _url, init) => {
      const body = JSON.parse(String(init?.body));
      assert.ok(!("if_exists" in body), "plain create must not send if_exists");
      assert.ok(
        !("external_ref" in body),
        "plain create without externalRef must not send external_ref",
      );
      return Response.json(agent(), { status: 201 });
    },
    async (client) => {
      const created = await client.createAgent({ name: "PR reviewer" });
      assert.equal(created.id, "agent_1");
    },
  );
});

test("client: createAgent adopt without externalRef fails before any request", async () => {
  let requests = 0;
  await withMockFetch(
    () => {
      requests++;
      return Response.json(agent());
    },
    async (client) => {
      await assert.rejects(
        client.createAgent({ name: "PR reviewer" }, { adoptExisting: true }),
        (err: unknown) => {
          assert.ok(err instanceof ConfigError);
          assert.match(String(err), /externalRef/);
          return true;
        },
      );
    },
  );
  assert.equal(requests, 0, "the invalid call must never reach the network");
});

test("client: adopt-mode create retries a transient 503", async () => {
  let requests = 0;
  await withMockFetch(
    () => {
      requests++;
      if (requests === 1) {
        return new Response("upstream unavailable", {
          status: 503,
          headers: { "Retry-After": "0" },
        });
      }
      return Response.json(agent(), { status: 200 });
    },
    async (client) => {
      const created = await client.createAgent(
        { name: "PR reviewer" },
        { adoptExisting: true, externalRef: "tenant-42/pr-reviewer" },
      );
      assert.equal(created.id, "agent_1");
    },
    { retry: 2 },
  );
  assert.equal(requests, 2, "the transient 503 must be retried in adopt mode");
});

test("client: plain create POST is not retried", async () => {
  let requests = 0;
  await withMockFetch(
    () => {
      requests++;
      return new Response("upstream unavailable", {
        status: 503,
        headers: { "Retry-After": "0" },
      });
    },
    async (client) => {
      await assert.rejects(client.createAgent({ name: "PR reviewer" }));
    },
    { retry: 2 },
  );
  assert.equal(requests, 1, "a plain create POST must not be replayed");
});

test("client: adopt conflict surfaces the documented 409 code", async () => {
  await withMockFetch(
    () =>
      Response.json(
        {
          error: {
            code: "external_identity_conflict",
            message: "external_ref is owned by a deleted agent",
          },
        },
        { status: 409 },
      ),
    async (client) => {
      await assert.rejects(
        client.createAgent(
          { name: "PR reviewer" },
          { adoptExisting: true, externalRef: "tenant-42/pr-reviewer" },
        ),
        (err: unknown) => {
          assert.ok(err instanceof MobiusAPIError);
          assert.equal(err.code, MobiusAPIError.EXTERNAL_IDENTITY_CONFLICT);
          assert.equal(err.status, 409);
          return true;
        },
      );
    },
  );
});

test("client: createProject adopt sends if_exists and hits /v1/projects", async () => {
  await withMockFetch(
    (method, url, init) => {
      assert.equal(method, "POST");
      assert.equal(url.pathname, "/v1/projects");
      const body = JSON.parse(String(init?.body));
      assert.equal(body.if_exists, "adopt");
      assert.equal(body.external_ref, "workspace-42");
      return Response.json(project(), { status: 200 });
    },
    async (client) => {
      const created = await client.createProject(
        { name: "Product Ops" },
        { adoptExisting: true, externalRef: "workspace-42" },
      );
      assert.equal(created.id, "prj_1");
    },
  );
});

test("client: createProject adopt without externalRef fails before any request", async () => {
  let requests = 0;
  await withMockFetch(
    () => {
      requests++;
      return Response.json(project());
    },
    async (client) => {
      await assert.rejects(
        client.createProject({ name: "Product Ops" }, { adoptExisting: true }),
        (err: unknown) => err instanceof ConfigError,
      );
    },
  );
  assert.equal(requests, 0);
});

test("client: project adopt conflict codes are documented constants", async () => {
  assert.equal(MobiusAPIError.PROJECT_ARCHIVED, "project_archived");
  assert.equal(
    MobiusAPIError.PROJECT_CAPACITY_REACHED,
    "project_capacity_reached",
  );
  await withMockFetch(
    () =>
      Response.json(
        { error: { code: "project_archived", message: "project is archived" } },
        { status: 409 },
      ),
    async (client) => {
      await assert.rejects(
        client.createProject(
          { name: "Product Ops" },
          { adoptExisting: true, externalRef: "workspace-42" },
        ),
        (err: unknown) => {
          assert.ok(err instanceof MobiusAPIError);
          assert.equal(err.code, MobiusAPIError.PROJECT_ARCHIVED);
          return true;
        },
      );
    },
  );
});
