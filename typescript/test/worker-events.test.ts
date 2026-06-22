import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  AuthRevokedError,
  Client,
  WorkerInstanceConflictError,
} from "../src/client.js";
import { Worker, WorkerPool, resolveInstanceID } from "../src/worker.js";

function testClient(): Client {
  return new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "test-project",
  });
}

test("worker: resolveInstanceID honors explicit value", async () => {
  const resolved = await resolveInstanceID("worker-1");
  assert.deepEqual(resolved, { id: "worker-1", source: "configured" });
});

test("worker: registers action functions fluently", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "default",
  });
  const worker = new Worker(client, { logger: null });
  assert.equal(
    worker.register("demo.action", async () => ({ ok: true })),
    worker,
  );
});

test("worker: classifies terminal protocol error codes", () => {
  const worker = new Worker(testClient(), {
    workerInstanceId: "dup",
    logger: null,
  });
  const classify = (error: { code?: string; message?: string }) =>
    (worker as unknown as {
      terminalProtocolError(e: unknown): Error | undefined;
    }).terminalProtocolError(error);

  const conflict = classify({
    code: "worker_instance_conflict",
    message: "already registered",
  });
  assert.ok(conflict instanceof WorkerInstanceConflictError);
  assert.equal(conflict.workerInstanceId, "dup");
  assert.equal(conflict.projectHandle, "test-project");
  assert.equal(conflict.message, "already registered");

  assert.ok(classify({ code: "invalid_actor" }) instanceof AuthRevokedError);
  assert.equal(classify({ code: "register_failed" }), undefined);
});

test("worker: run rethrows instance conflict without reconnecting", async () => {
  const worker = new Worker(testClient(), {
    workerInstanceId: "dup",
    reconnectDelayMs: 1,
    logger: null,
  });
  let calls = 0;
  (worker as unknown as { runSocket(): Promise<void> }).runSocket = async () => {
    calls += 1;
    if (calls > 1) {
      // Regression guard: a reconnect means the fix is broken. Stop the loop
      // so the test fails fast (run resolves) instead of hanging.
      worker.stop();
      return;
    }
    throw new WorkerInstanceConflictError("dup", "test-project");
  };

  await assert.rejects(worker.run(), WorkerInstanceConflictError);
  assert.equal(calls, 1);
});

test("worker pool: registers shared action functions fluently", () => {
  const client = new Client({
    apiKey: "mbx_test",
    baseURL: "http://localhost:8080",
    project: "default",
  });
  const pool = new WorkerPool(client, { logger: null, count: 2 });
  assert.equal(pool.register("demo.action", async () => ({ ok: true })), pool);
});

// FakeSocket is an in-memory stand-in for the worker WebSocket. It is always
// "open" so waitForOpen resolves immediately; emit() delivers a server frame
// to the runSocket loop, send() records what the worker writes, and close()
// ends the frame iterator. Substituted via the protected openSocket() seam.
class FakeSocket extends EventTarget {
  readonly OPEN = 1;
  readyState = 1;
  closed = false;
  sent: Array<Record<string, unknown>> = [];

  send(data: string): void {
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.readyState = 3;
    this.dispatchEvent(new Event("close"));
  }

  emit(frame: Record<string, unknown>): void {
    const event = new Event("message");
    (event as unknown as { data: string }).data = JSON.stringify(frame);
    this.dispatchEvent(event);
  }

  claims(): Array<Record<string, unknown>> {
    return this.sent.filter((f) => f.type === "jobs.claim");
  }
}

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

function workerWithFakeSocket(
  ws: FakeSocket,
  claimResponseTimeoutMs: number,
): Worker {
  const worker = new Worker(testClient(), {
    workerInstanceId: "worker-1",
    logger: null,
  });
  (worker as unknown as { claimResponseTimeoutMs: number }).claimResponseTimeoutMs =
    claimResponseTimeoutMs;
  (worker as unknown as { openSocket(): unknown }).openSocket = () => ws;
  return worker;
}

function runSocket(worker: Worker, signal: AbortSignal): Promise<void> {
  return (
    worker as unknown as { runSocket(s: AbortSignal): Promise<void> }
  ).runSocket(signal);
}

test("worker: drops the socket when a claim goes unanswered", async () => {
  const ws = new FakeSocket();
  const worker = workerWithFakeSocket(ws, 20);
  const ac = new AbortController();
  const done = runSocket(worker, ac.signal);

  await tick();
  ws.emit({ type: "worker.registered", worker_session_token: "tok" });

  // The claim is never answered; runSocket should close the socket after the
  // deadline and reject so run() reconnects.
  await assert.rejects(done, /unanswered/);
  assert.equal(ws.claims().length, 1);
  assert.equal(ws.closed, true);
});

test("worker: clears an outstanding claim answered by a matching error", async () => {
  const ws = new FakeSocket();
  const worker = workerWithFakeSocket(ws, 10_000);
  const ac = new AbortController();
  const done = runSocket(worker, ac.signal);

  await tick();
  ws.emit({ type: "worker.registered", worker_session_token: "tok" });
  await tick();
  const claimId = String(ws.claims()[0].message_id);

  // A nonterminal error that answers the claim must clear claimOutstanding so
  // the next work.available re-claims.
  ws.emit({ type: "error", message_id: claimId, error: { code: "claim_failed" } });
  await tick();
  ws.emit({ type: "work.available" });
  await tick();

  assert.equal(ws.claims().length, 2);
  ac.abort();
  await done;
});

test("worker: keeps the claim outstanding on an unmatched error", async () => {
  const ws = new FakeSocket();
  const worker = workerWithFakeSocket(ws, 10_000);
  const ac = new AbortController();
  const done = runSocket(worker, ac.signal);

  await tick();
  ws.emit({ type: "worker.registered", worker_session_token: "tok" });
  await tick();

  // An error for some other message must not clear our outstanding claim, so
  // work.available stays a no-op (claimOutstanding still set).
  ws.emit({ type: "error", message_id: "msg_other", error: { code: "claim_failed" } });
  await tick();
  ws.emit({ type: "work.available" });
  await tick();

  assert.equal(ws.claims().length, 1);
  ac.abort();
  await done;
});

test("worker: does not re-claim after an empty jobs.claimed", async () => {
  const ws = new FakeSocket();
  const worker = workerWithFakeSocket(ws, 10_000);
  const ac = new AbortController();
  const done = runSocket(worker, ac.signal);

  await tick();
  ws.emit({ type: "worker.registered", worker_session_token: "tok" });
  await tick();
  ws.emit({ type: "jobs.claimed", jobs: [] });
  await tick();

  // No job ran, so nothing re-claims; an empty response must not hot-poll.
  assert.equal(ws.claims().length, 1);
  ac.abort();
  await done;
});
