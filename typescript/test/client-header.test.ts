import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { Client } from "../src/client.js";
import {
  MOBIUS_CLIENT,
  MOBIUS_CLIENT_HEADER,
  SDK_VERSION,
} from "../src/version.js";

// The server's rules: a name of up to 32 [a-z0-9._-], then an optional version
// of up to 64 [A-Za-z0-9._+-].
const SERVER_CLIENT_VALUE = /^[a-z0-9._-]{1,32}(\/[A-Za-z0-9._+-]{1,64})?$/;

test("SDK_VERSION matches package.json", () => {
  // Tests run from build-test/test/, two levels below typescript/.
  const pkg = JSON.parse(
    readFileSync(new URL("../../package.json", import.meta.url), "utf8"),
  ) as { version: string };
  assert.equal(SDK_VERSION, pkg.version);
});

test("requests send X-Mobius-Client: sdk-ts/<version>", async () => {
  assert.equal(MOBIUS_CLIENT_HEADER, "X-Mobius-Client");
  assert.equal(MOBIUS_CLIENT, `sdk-ts/${SDK_VERSION}`);
  assert.match(MOBIUS_CLIENT, SERVER_CLIENT_VALUE);

  const seen: (string | null)[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    seen.push(new Headers(init?.headers).get(MOBIUS_CLIENT_HEADER));
    return new Response(JSON.stringify({ items: [], has_more: false }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof globalThis.fetch;
  try {
    const client = new Client({ apiKey: "k", baseURL: "http://test" });
    await client.listSkills();
  } finally {
    globalThis.fetch = original;
  }
  assert.deepEqual(seen, [MOBIUS_CLIENT]);
});
