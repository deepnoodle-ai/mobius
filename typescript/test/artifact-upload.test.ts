import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";

test("client: project-authorized artifact upload sends multipart metadata and idempotency", async () => {
  const originalFetch = globalThis.fetch;
  let capturedURL = "";
  let capturedHeaders = new Headers();
  let capturedForm: FormData | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    capturedURL = typeof input === "string" ? input : input.toString();
    capturedHeaders = new Headers(init?.headers);
    capturedForm = init?.body as FormData;
    return Response.json({
      id: "art_fixture",
      visibility: "private",
      name: "renders/report.html",
      mime_type: "text/html",
      size_bytes: 15,
      created_at: "2026-07-15T00:00:00Z",
      metadata: { renderer: "omni" },
    });
  }) as typeof fetch;

  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      project: "omni",
      retry: 0,
    });
    const artifact = await client.createArtifact({
      name: "renders/report.html",
      file: new TextEncoder().encode("<h1>report</h1>"),
      mimeType: "text/html",
      metadata: { renderer: "omni" },
      idempotencyKey: "render-1",
    });
    assert.equal(artifact.id, "art_fixture");
  } finally {
    globalThis.fetch = originalFetch;
  }

  assert.equal(
    capturedURL,
    "https://api.example.invalid/v1/projects/omni/artifacts",
  );
  assert.equal(capturedHeaders.get("Authorization"), "Bearer mbx_test");
  assert.equal(capturedHeaders.get("Idempotency-Key"), "render-1");
  assert.equal(capturedHeaders.has("Content-Type"), false);
  assert.ok(capturedForm instanceof FormData);
  assert.equal(capturedForm.get("name"), "renders/report.html");
  assert.equal(capturedForm.get("mime"), "text/html");
  assert.equal(capturedForm.get("size_bytes"), "15");
  assert.equal(capturedForm.get("metadata"), '{"renderer":"omni"}');
  assert.equal(capturedForm.has("action_invocation"), false);
  const file = capturedForm.get("file");
  assert.ok(file instanceof Blob);
  assert.equal(await file.text(), "<h1>report</h1>");
});

test("client: artifact idempotency key is optional and bounded", async () => {
  const originalFetch = globalThis.fetch;
  let capturedHeaders = new Headers();
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    capturedHeaders = new Headers(init?.headers);
    return Response.json({
      id: "art_fixture",
      visibility: "private",
      name: "report.txt",
      mime_type: "text/plain",
      size_bytes: 1,
      created_at: "2026-07-15T00:00:00Z",
    });
  }) as typeof fetch;

  try {
    const client = new Client({ apiKey: "mbx_test", retry: 0 });
    await client.createArtifact({
      name: "report.txt",
      file: new Uint8Array([1]),
      mimeType: "text/plain",
    });
    assert.equal(capturedHeaders.has("Idempotency-Key"), false);

    await assert.rejects(
      client.createArtifact({
        name: "report.txt",
        file: new Uint8Array([1]),
        idempotencyKey: "x".repeat(256),
      }),
      /idempotencyKey must be at most 255 characters/,
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});
