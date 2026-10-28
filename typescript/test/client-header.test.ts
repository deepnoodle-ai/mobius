import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { createServer, type IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { Client } from "../src/client.js";
import { Worker } from "../src/worker.js";
import {
  MOBIUS_CLIENT,
  MOBIUS_CLIENT_HEADER,
  SDK_VERSION,
} from "../src/version.js";

// The server's rules: a name of up to 32 [a-z0-9._-], then an optional version
// of up to 64 [A-Za-z0-9._+-].
const SERVER_CLIENT_VALUE = /^[a-z0-9._-]{1,32}(\/[A-Za-z0-9._+-]{1,64})?$/;

// Opens the worker socket against a local server and returns the handshake
// headers it received.
async function socketHandshakeHeaders(
  version: string | undefined,
): Promise<IncomingHttpHeaders> {
  const server = createServer();
  const received = new Promise<IncomingHttpHeaders>((resolve) => {
    server.on("upgrade", (req, socket) => {
      resolve(req.headers);
      socket.destroy();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  try {
    const worker = new Worker(
      new Client({ apiKey: "k", baseURL: `http://127.0.0.1:${port}` }),
      { logger: null, version },
    );
    const ws = (worker as unknown as { openSocket(): WebSocket }).openSocket();
    ws.onerror = () => {};
    return await received;
  } finally {
    server.close();
  }
}

test("the worker socket sends X-Mobius-Client: worker/<version>", async () => {
  const named = await socketHandshakeHeaders("v1.2.3");
  assert.equal(named["x-mobius-client"], "worker/v1.2.3");
  assert.equal(named.authorization, "Bearer k");

  const unnamed = await socketHandshakeHeaders(undefined);
  assert.equal(unnamed["x-mobius-client"], `worker/${SDK_VERSION}`);
  assert.match(String(unnamed["x-mobius-client"]), SERVER_CLIENT_VALUE);
});

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
