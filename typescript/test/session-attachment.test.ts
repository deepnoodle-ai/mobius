import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "../src/client.js";

const ATTACHMENT_FIXTURE = {
  artifact: {
    id: "art_fixture",
    owner: { kind: "person", id: "user_1" },
    visibility: "private",
    container: null,
    posture: "only_you",
    name: "brief.md",
    mime_type: "text/markdown",
    size_bytes: 7,
    created_at: "2026-07-15T00:00:00Z",
  },
  content_block: {
    type: "document",
    source: { type: "artifact", artifact_id: "art_1" },
    title: "brief.md",
    media_type: "text/markdown",
  },
};

test("client: session attachment posts multipart to the session-scoped path", async () => {
  const originalFetch = globalThis.fetch;
  let capturedURL = "";
  let capturedHeaders = new Headers();
  let capturedForm: FormData | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    capturedURL = typeof input === "string" ? input : input.toString();
    capturedHeaders = new Headers(init?.headers);
    capturedForm = init?.body as FormData;
    return Response.json(ATTACHMENT_FIXTURE);
  }) as typeof fetch;

  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      retry: 0,
    });
    const attachment = await client.createSessionAttachment({
      sessionId: "sess_1",
      name: "brief.md",
      file: new TextEncoder().encode("# brief"),
      mimeType: "text/markdown",
      idempotencyKey: "turn-1:brief",
    });
    assert.equal(attachment.artifact.id, "art_fixture");
    assert.equal(attachment.content_block.type, "document");
  } finally {
    globalThis.fetch = originalFetch;
  }

  assert.equal(
    capturedURL,
    "https://api.example.invalid/v1/sessions/sess_1/attachments",
  );
  assert.equal(capturedHeaders.get("Authorization"), "Bearer mbx_test");
  assert.equal(capturedHeaders.get("Idempotency-Key"), "turn-1:brief");
  assert.equal(capturedHeaders.has("Content-Type"), false);
  assert.ok(capturedForm instanceof FormData);
  assert.equal(capturedForm.get("name"), "brief.md");
  assert.equal(capturedForm.get("mime"), "text/markdown");
  assert.equal(capturedForm.get("size_bytes"), "7");
  // Attachments carry no caller metadata; the artifact endpoint does.
  assert.equal(capturedForm.has("metadata"), false);
  const file = capturedForm.get("file");
  assert.ok(file instanceof Blob);
  assert.equal(await file.text(), "# brief");
});

test("client: session attachment rejects a blank name and an overlong retry key", async () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "https://api.example.invalid",
    retry: 0,
  });
  await assert.rejects(
    () =>
      client.createSessionAttachment({
        sessionId: "sess_1",
        name: "   ",
        file: new TextEncoder().encode("x"),
      }),
    /attachment name is required/,
  );
  await assert.rejects(
    () =>
      client.createSessionAttachment({
        sessionId: "sess_1",
        name: "brief.md",
        file: new TextEncoder().encode("x"),
        idempotencyKey: "k".repeat(256),
      }),
    /at most 255 characters/,
  );
});

test("client: deleting a session attachment escapes both identifiers", async () => {
  const originalFetch = globalThis.fetch;
  let capturedURL = "";
  let capturedMethod = "";
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    capturedURL = typeof input === "string" ? input : input.toString();
    capturedMethod = init?.method ?? "";
    return new Response(null, { status: 204 });
  }) as typeof fetch;

  try {
    const client = new Client({
      apiKey: "mbx_test",
      baseURL: "https://api.example.invalid",
      retry: 0,
    });
    await client.deleteSessionAttachment("sess/1", "art 1");
  } finally {
    globalThis.fetch = originalFetch;
  }

  assert.equal(capturedMethod, "DELETE");
  assert.equal(
    capturedURL,
    "https://api.example.invalid/v1/sessions/sess%2F1/attachments/art%201",
  );
});
