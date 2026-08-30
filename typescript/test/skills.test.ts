import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";
import type { Skill } from "../src/api/index.js";

const SKILL_DOC =
  "---\nallowed_tools:\n  - github.create_review_comment\n---\nCheck the diff and leave concise findings.\n";

function skill(id = "skill_1", source: Skill["source"] = "custom"): Skill {
  return {
    id,
    owner: { kind: "person", id: "user_1" },
    visibility: "private",
    container: null,
    posture: "only_you",
    name: "Pull request review",
    source,
    instructions: "Check the diff and leave concise findings.",
    allowed_tools: ["github.create_review_comment"],
    owner: { kind: "team" },
    visibility: "organization",
    container: null,
    posture: "team",
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
      retry: 0,
    });
    await fn(client);
  } finally {
    globalThis.fetch = originalFetch;
  }
}

test("client: skill lifecycle hits the documented routes", async () => {
  const seen: string[] = [];
  await withMockFetch(
    (method, url, init) => {
      seen.push(`${method} ${url.pathname}`);
      if (method === "DELETE") return new Response(null, { status: 204 });
      if (method === "POST") {
        assert.equal(
          JSON.parse(String(init?.body)).name,
          "Pull request review",
        );
        return Response.json(skill(), { status: 201 });
      }
      if (method === "PUT") {
        const body = JSON.parse(String(init?.body));
        assert.ok(body.instructions, "update must send the full body");
        return Response.json(skill());
      }
      if (url.pathname === "/v1/skills") {
        assert.equal(url.searchParams.get("include_system"), "false");
        return Response.json({ items: [skill()] });
      }
      return Response.json(skill());
    },
    async (client) => {
      const page = await client.listSkills({ includeSystem: false });
      assert.equal(page.items[0]?.id, "skill_1");
      const req = {
        name: "Pull request review",
        instructions: "Check the diff and leave concise findings.",
      };
      await client.createSkill(req);
      await client.getSkill("skill_1");
      await client.updateSkill("skill_1", req);
      await client.deleteSkill("skill_1");
    },
  );
  assert.deepEqual(seen, [
    "GET /v1/skills",
    "POST /v1/skills",
    "GET /v1/skills/skill_1",
    "PUT /v1/skills/skill_1",
    "DELETE /v1/skills/skill_1",
  ]);
});

test("client: importSkill sends the document verbatim", async () => {
  const bodies: unknown[] = [];
  await withMockFetch(
    (_method, url, init) => {
      assert.equal(url.pathname, "/v1/skills/import");
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json(skill(), { status: 201 });
    },
    async (client) => {
      const imported = await client.importSkill(SKILL_DOC, {
        name: "Pull request review",
      });
      assert.equal(imported.source, "custom");
      await client.importSkill("Just instructions.");
    },
  );
  assert.deepEqual(bodies, [
    { content: SKILL_DOC, name: "Pull request review" },
    { content: "Just instructions." },
  ]);
});

test("client: replaceAgentSkillAssignments preserves order and allows empty", async () => {
  const bodies: unknown[] = [];
  await withMockFetch(
    (method, url, init) => {
      assert.equal(
        url.pathname,
        "/v1/agents/agent_1/skill-assignments",
      );
      if (method === "GET") return Response.json({ items: [] });
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json({
        items: [
          {
            agent_id: "agent_1",
            skill_id: "skill_2",
            enabled: true,
            position: 0,
            created_at: "2026-07-17T00:00:00Z",
          },
          {
            agent_id: "agent_1",
            skill_id: "skill_1",
            enabled: true,
            position: 1,
            created_at: "2026-07-17T00:00:00Z",
          },
        ],
      });
    },
    async (client) => {
      await client.listAgentSkillAssignments("agent_1");
      const page = await client.replaceAgentSkillAssignments("agent_1", [
        "skill_2",
        "skill_1",
      ]);
      assert.deepEqual(
        page.items.map((a) => a.skill_id),
        ["skill_2", "skill_1"],
      );
      await client.replaceAgentSkillAssignments("agent_1", []);
    },
  );
  assert.deepEqual(bodies, [
    { skill_ids: ["skill_2", "skill_1"] },
    { skill_ids: [] },
  ]);
});
