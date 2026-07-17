import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client, MobiusAPIError } from "../src/client.js";
import type { Skill } from "../src/api/index.js";

const SKILL_DOC =
  "---\nallowed_tools:\n  - github.create_review_comment\n---\nCheck the diff and leave concise findings.\n";

function skill(id = "skill_1", source: Skill["source"] = "project"): Skill {
  return {
    id,
    name: "Pull request review",
    source,
    instructions: "Check the diff and leave concise findings.",
    allowed_tools: ["github.create_review_comment"],
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

test("client: project skill lifecycle hits the documented routes", async () => {
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
      if (url.pathname === "/v1/projects/test-project/skills") {
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
    "GET /v1/projects/test-project/skills",
    "POST /v1/projects/test-project/skills",
    "GET /v1/projects/test-project/skills/skill_1",
    "PUT /v1/projects/test-project/skills/skill_1",
    "DELETE /v1/projects/test-project/skills/skill_1",
  ]);
});

test("client: importSkill sends the document verbatim", async () => {
  const bodies: unknown[] = [];
  await withMockFetch(
    (_method, url, init) => {
      assert.equal(url.pathname, "/v1/projects/test-project/skills/import");
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json(skill(), { status: 201 });
    },
    async (client) => {
      const imported = await client.importSkill(SKILL_DOC, {
        name: "Pull request review",
      });
      assert.equal(imported.source, "project");
      await client.importSkill("Just instructions.");
    },
  );
  assert.deepEqual(bodies, [
    { content: SKILL_DOC, name: "Pull request review" },
    { content: "Just instructions." },
  ]);
});

test("client: organization skill routes preserve provenance and usage", async () => {
  await withMockFetch(
    (method, url) => {
      if (url.pathname === "/v1/organization/skills" && method === "GET") {
        return Response.json({ items: [skill("skill_org", "organization")] });
      }
      if (url.pathname === "/v1/organization/skills/import") {
        return Response.json(skill("skill_org", "organization"), {
          status: 201,
        });
      }
      if (url.pathname === "/v1/organization/skills/skill_org") {
        return Response.json(skill("skill_org", "organization"));
      }
      assert.equal(url.pathname, "/v1/organization/skills/skill_org/usage");
      return Response.json({
        skill_id: "skill_org",
        assignment_count: 3,
        project_count: 2,
        projects: [
          { project_id: "proj_a", agent_count: 2 },
          { project_id: "proj_b", agent_count: 1 },
        ],
      });
    },
    async (client) => {
      const page = await client.listOrganizationSkills();
      assert.equal(page.items[0]?.source, "organization");
      await client.importOrganizationSkill(SKILL_DOC);
      await client.replaceOrganizationSkill("skill_org", {
        name: "Pull request review",
        instructions: "Check the diff.",
      });
      const usage = await client.getOrganizationSkillUsage("skill_org");
      assert.equal(usage.assignment_count, 3);
      assert.deepEqual(
        usage.projects.map((p) => p.project_id),
        ["proj_a", "proj_b"],
      );
    },
  );
});

test("client: deleteOrganizationSkill surfaces the skill_in_use conflict", async () => {
  await withMockFetch(
    () =>
      Response.json(
        {
          error: { code: "skill_in_use", message: "detach agents first" },
        },
        { status: 409 },
      ),
    async (client) => {
      await assert.rejects(
        client.deleteOrganizationSkill("skill_org"),
        (err: unknown) =>
          err instanceof MobiusAPIError &&
          err.status === 409 &&
          err.code === "skill_in_use",
      );
    },
  );
});

test("client: replaceAgentSkillAssignments preserves order and allows empty", async () => {
  const bodies: unknown[] = [];
  await withMockFetch(
    (method, url, init) => {
      assert.equal(
        url.pathname,
        "/v1/projects/test-project/agents/agent_1/skill-assignments",
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
