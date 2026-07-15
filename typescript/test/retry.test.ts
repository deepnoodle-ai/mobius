import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  MAX_RETRY_BACKOFF_SECONDS,
  RateLimitError,
  wrapFetchWithRetry,
} from "../src/retry.js";

interface Stub {
  status: number;
  headers?: Record<string, string>;
  body?: string;
}

function sequencedFetch(
  stubs: Stub[],
): { fetch: typeof globalThis.fetch; callCount: () => number } {
  let i = 0;
  const fetchFn: typeof globalThis.fetch = async () => {
    const stub = stubs[i] ?? stubs[stubs.length - 1];
    i += 1;
    return new Response(stub?.body ?? null, {
      status: stub?.status ?? 500,
      headers: stub?.headers,
    });
  };
  return { fetch: fetchFn, callCount: () => i };
}

class SleepRecorder {
  readonly calls: number[] = [];
  sleep = async (seconds: number): Promise<void> => {
    this.calls.push(seconds);
  };
}

test("retry: 429 then 200 succeeds after one retry", async () => {
  const { fetch, callCount } = sequencedFetch([
    { status: 429, headers: { "Retry-After": "0" } },
    { status: 200 },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 200);
  assert.equal(callCount(), 2);
  assert.deepEqual(rec.calls, []);
});

test("retry: honors Retry-After seconds", async () => {
  const { fetch } = sequencedFetch([
    { status: 429, headers: { "Retry-After": "7" } },
    { status: 200 },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 200);
  assert.deepEqual(rec.calls, [7]);
});

test("retry: honors Retry-After HTTP-date", async () => {
  const now = Date.UTC(2025, 0, 1, 12, 0, 0);
  const future = new Date(now + 9_000).toUTCString();
  const { fetch } = sequencedFetch([
    { status: 429, headers: { "Retry-After": future } },
    { status: 200 },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
    now: () => now,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 200);
  assert.deepEqual(rec.calls, [9]);
});

test("retry: clamps Retry-After to 60s cap", async () => {
  const { fetch } = sequencedFetch([
    { status: 429, headers: { "Retry-After": "9999" } },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 1,
    sleep: rec.sleep,
  });
  await assert.rejects(() => wrapped("https://x/y"), RateLimitError);
  assert.deepEqual(rec.calls, [MAX_RETRY_BACKOFF_SECONDS]);
});

test("retry: POST without Idempotency-Key surfaces RateLimitError immediately", async () => {
  const { fetch, callCount } = sequencedFetch([
    {
      status: 429,
      headers: {
        "Retry-After": "3",
        "X-RateLimit-Limit": "10000",
        "X-RateLimit-Remaining": "0",
        "X-RateLimit-Reset": "1735689600",
        "X-RateLimit-Scope": "key",
        "X-RateLimit-Policy": "10000;w=18000",
      },
    },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 5,
    sleep: rec.sleep,
  });
  await assert.rejects(
    async () => {
      await wrapped("https://x/y", {
        method: "POST",
        body: JSON.stringify({ a: 1 }),
      });
    },
    (err: unknown) => {
      assert.ok(err instanceof RateLimitError);
      const rle = err as RateLimitError;
      assert.equal(rle.retryAfter, 3);
      assert.equal(rle.limit, 10000);
      assert.equal(rle.remaining, 0);
      assert.equal(rle.scope, "key");
      assert.equal(rle.policy, "10000;w=18000");
      assert.ok(rle.resetAt instanceof Date);
      return true;
    },
  );
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});

test("retry: POST with Idempotency-Key is retried on 429", async () => {
  const { fetch, callCount } = sequencedFetch([
    { status: 429, headers: { "Retry-After": "0" } },
    { status: 200 },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y", {
    method: "POST",
    headers: { "Idempotency-Key": "k1" },
  });
  assert.equal(resp.status, 200);
  assert.equal(callCount(), 2);
});

test("retry: replay-safe POST reuses the exact body and key after network and 503 failures", async () => {
  const bodies: string[] = [];
  const keys: Array<string | null> = [];
  let calls = 0;
  const fetchFn: typeof globalThis.fetch = async (_input, init) => {
    calls += 1;
    bodies.push(String(init?.body));
    keys.push(new Headers(init?.headers).get("Idempotency-Key"));
    if (calls === 1) throw new Error("connection reset by peer");
    if (calls === 2) {
      return new Response(null, {
        status: 503,
        headers: { "Retry-After": "0" },
      });
    }
    return Response.json({ ok: true });
  };
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetchFn, {
    maxRetries: 3,
    sleep: rec.sleep,
  });

  const resp = await wrapped("https://x/y", {
    method: "POST",
    headers: { "Idempotency-Key": "k1" },
    body: '{"message":"same"}',
  });

  assert.equal(resp.status, 200);
  assert.equal(calls, 3);
  assert.deepEqual(bodies, [
    '{"message":"same"}',
    '{"message":"same"}',
    '{"message":"same"}',
  ]);
  assert.deepEqual(keys, ["k1", "k1", "k1"]);
  assert.deepEqual(rec.calls, [1]);
});

test("retry: unreadable or invalid replay-safe JSON acknowledgement is retried", async () => {
  let calls = 0;
  const fetchFn: typeof globalThis.fetch = async () => {
    calls += 1;
    if (calls === 1) {
      return new Response("{", {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    return Response.json({ accepted: true }, { status: 202 });
  };
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetchFn, {
    maxRetries: 2,
    sleep: rec.sleep,
  });

  const resp = await wrapped("https://x/y", {
    method: "POST",
    headers: { "Idempotency-Key": "k1" },
    body: "{}",
  });

  assert.deepEqual(await resp.json(), { accepted: true });
  assert.equal(calls, 2);
  assert.deepEqual(rec.calls, [1]);
});

test("retry: SSE failures after response start never reinvoke", async () => {
  let calls = 0;
  const fetchFn: typeof globalThis.fetch = async () => {
    calls += 1;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("event: ready\n\n"));
        controller.error(new Error("stream disconnected"));
      },
    });
    return new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    });
  };
  const wrapped = wrapFetchWithRetry(fetchFn, { maxRetries: 3 });

  const resp = await wrapped("https://x/y", {
    method: "POST",
    headers: {
      Accept: "text/event-stream",
      "Idempotency-Key": "k1",
    },
    body: "{}",
  });

  await assert.rejects(() => resp.text(), /stream disconnected/);
  assert.equal(calls, 1);
});

test("retry: maxRetries=0 surfaces RateLimitError immediately", async () => {
  const { fetch, callCount } = sequencedFetch([
    { status: 429, headers: { "X-RateLimit-Scope": "org" } },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 0,
    sleep: rec.sleep,
  });
  await assert.rejects(
    async () => {
      await wrapped("https://x/y");
    },
    (err: unknown) => {
      assert.ok(err instanceof RateLimitError);
      assert.equal((err as RateLimitError).scope, "org");
      return true;
    },
  );
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});

test("retry: exhausts budget and returns RateLimitError", async () => {
  const stubs: Stub[] = [];
  for (let i = 0; i < 4; i++) {
    stubs.push({ status: 429 });
  }
  const { fetch, callCount } = sequencedFetch(stubs);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  await assert.rejects(() => wrapped("https://x/y"), RateLimitError);
  assert.equal(callCount(), 4);
  assert.deepEqual(rec.calls, [1, 2, 4]);
});

test("retry: reports the status-specific reason for every transient response", async () => {
  const cases = [
    { status: 429, reason: "rate_limited" },
    { status: 500, reason: "server_error" },
    { status: 502, reason: "server_error" },
    { status: 503, reason: "service_unavailable" },
    { status: 504, reason: "server_error" },
  ] as const;

  for (const { status, reason } of cases) {
    const events: string[] = [];
    const { fetch } = sequencedFetch([
      { status, headers: { "Retry-After": "0" } },
      { status: 200 },
    ]);
    const wrapped = wrapFetchWithRetry(fetch, {
      maxRetries: 1,
      onRetry: (event) => events.push(event.reason),
    });

    assert.equal((await wrapped("https://x/y")).status, 200);
    assert.deepEqual(events, [reason]);
  }
});

for (const status of [500, 502, 503, 504]) {
  test(`retry: ${status} is retried then passes through`, async () => {
    const { fetch, callCount } = sequencedFetch([
      { status },
      { status },
      { status },
    ]);
    const rec = new SleepRecorder();
    const wrapped = wrapFetchWithRetry(fetch, {
      maxRetries: 2,
      sleep: rec.sleep,
    });
    const resp = await wrapped("https://x/y");
    assert.equal(resp.status, status);
    assert.equal(callCount(), 3);
    assert.deepEqual(rec.calls, [1, 2]);
  });

  test(`retry: ${status} respects the POST idempotency gate`, async () => {
    const keyed = sequencedFetch([
      { status, headers: { "Retry-After": "0" } },
      { status: 200 },
    ]);
    const wrappedKeyed = wrapFetchWithRetry(keyed.fetch, { maxRetries: 2 });
    const keyedResp = await wrappedKeyed("https://x/y", {
      method: "POST",
      body: '{"same":true}',
      headers: { "Idempotency-Key": "k1" },
    });
    assert.equal(keyedResp.status, 200);
    assert.equal(keyed.callCount(), 2);

    const unkeyed = sequencedFetch([{ status }]);
    const wrappedUnkeyed = wrapFetchWithRetry(unkeyed.fetch, { maxRetries: 2 });
    const unkeyedResp = await wrappedUnkeyed("https://x/y", {
      method: "POST",
      body: '{"same":true}',
    });
    assert.equal(unkeyedResp.status, status);
    assert.equal(unkeyed.callCount(), 1);
  });
}

test("retry: non-retryable statuses pass through unchanged", async () => {
  const { fetch, callCount } = sequencedFetch([{ status: 501 }]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 501);
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});

// A fetch that throws (transport-level error) for the first `failures` calls,
// then resolves 200. Models a connection reset / DNS failure / I/O timeout.
function throwingFetch(
  failures: number,
  err: Error,
): { fetch: typeof globalThis.fetch; callCount: () => number } {
  let i = 0;
  const fetchFn: typeof globalThis.fetch = async () => {
    i += 1;
    if (i <= failures) throw err;
    return new Response(`{"ok":true}`, { status: 200 });
  };
  return { fetch: fetchFn, callCount: () => i };
}

test("retry: transport error then 200 succeeds", async () => {
  const { fetch, callCount } = throwingFetch(
    2,
    new Error("connection reset by peer"),
  );
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, { maxRetries: 3, sleep: rec.sleep });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 200);
  assert.equal(callCount(), 3);
  assert.deepEqual(rec.calls, [1, 2]);
});

test("retry: transport error exhausts budget and rethrows", async () => {
  const err = new Error("dial: connection refused");
  const { fetch, callCount } = throwingFetch(100, err);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, { maxRetries: 2, sleep: rec.sleep });
  await assert.rejects(() => wrapped("https://x/y"), /connection refused/);
  // attempts 0 and 1 back off; attempt 2 is out of budget and rethrows.
  assert.equal(callCount(), 3);
  assert.deepEqual(rec.calls, [1, 2]);
});

test("retry: transport error not retried for non-idempotent POST", async () => {
  const { fetch, callCount } = throwingFetch(100, new Error("unexpected EOF"));
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, { maxRetries: 3, sleep: rec.sleep });
  await assert.rejects(
    () => wrapped("https://x/y", { method: "POST", body: "{}" }),
    /unexpected EOF/,
  );
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});

test("retry: aborted request is not retried", async () => {
  const abort = new Error("the operation was aborted");
  abort.name = "AbortError";
  const { fetch, callCount } = throwingFetch(100, abort);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, { maxRetries: 3, sleep: rec.sleep });
  await assert.rejects(
    () => wrapped("https://x/y"),
    (e: unknown) => (e as Error).name === "AbortError",
  );
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});

test("retry: timeout errors are not retried", async () => {
  const timeout = new Error("request timed out");
  timeout.name = "TimeoutError";
  const { fetch, callCount } = throwingFetch(100, timeout);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, { maxRetries: 3, sleep: rec.sleep });
  await assert.rejects(
    () =>
      wrapped("https://x/y", {
        method: "POST",
        headers: { "Idempotency-Key": "k1" },
      }),
    (e: unknown) => (e as Error).name === "TimeoutError",
  );
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});
