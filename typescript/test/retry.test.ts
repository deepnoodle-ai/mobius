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

test("retry: 503 is retried then passes through", async () => {
  const { fetch, callCount } = sequencedFetch([
    { status: 503 },
    { status: 503 },
    { status: 503 },
  ]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 2,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 503);
  assert.equal(callCount(), 3);
  assert.deepEqual(rec.calls, [1, 2]);
});

test("retry: non-retryable statuses pass through unchanged", async () => {
  const { fetch, callCount } = sequencedFetch([{ status: 500 }]);
  const rec = new SleepRecorder();
  const wrapped = wrapFetchWithRetry(fetch, {
    maxRetries: 3,
    sleep: rec.sleep,
  });
  const resp = await wrapped("https://x/y");
  assert.equal(resp.status, 500);
  assert.equal(callCount(), 1);
  assert.deepEqual(rec.calls, []);
});
