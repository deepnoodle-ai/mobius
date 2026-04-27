import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Worker, WorkerPool } from "../src/worker.js";
import type {
  JobClaim,
  JobCompleteRequest,
  JobHeartbeat,
} from "../src/api/index.js";

class FakeClient {
  public readonly project = "prj_1";
  public emitted: Array<{
    jobId: string;
    type: string;
    payload: Record<string, unknown>;
  }> = [];
  public completed: JobCompleteRequest[] = [];

  async claimJob(_req?: { worker_instance_id?: string }): Promise<JobClaim | null> {
    return null;
  }

  async heartbeatJob(): Promise<JobHeartbeat> {
    return {
      ok: true,
      directives: { should_cancel: false },
    };
  }

  async emitJobEvents(
    jobId: string,
    req: { events: Array<{ type: string; payload: Record<string, unknown> }> },
  ): Promise<void> {
    for (const event of req.events) {
      this.emitted.push({ jobId, type: event.type, payload: event.payload });
    }
  }

  async completeJob(_jobId: string, req: JobCompleteRequest): Promise<void> {
    this.completed.push(req);
  }
}

class SequencedClient extends FakeClient {
  public claims = 0;
  public workerIds: string[] = [];
  public sessionTokens: string[] = [];
  public concurrencyLimits: number[] = [];

  constructor(private readonly jobs: JobClaim[]) {
    super();
  }

  // Strict on purpose: the SDK must send worker_instance_id on
  // every claim. Throw when it's missing so any regression that
  // omits the field surfaces here rather than silently in production.
  override async claimJob(req?: {
    worker_instance_id?: string;
    worker_session_token?: string;
    concurrency_limit?: number;
  }): Promise<JobClaim | null> {
    this.claims += 1;
    if (!req || !req.worker_instance_id) {
      throw new Error(
        "SequencedClient.claimJob: worker_instance_id is required",
      );
    }
    this.workerIds.push(req.worker_instance_id);
    this.sessionTokens.push(req.worker_session_token ?? "");
    this.concurrencyLimits.push(req.concurrency_limit ?? 0);
    return this.jobs.shift() ?? null;
  }
}

const blockJob = (id: string): JobClaim => ({
  job_id: id,
  run_id: "run_1",
  workflow_name: "demo",
  step_name: "scrape",
  action: "demo.block",
  parameters: {},
  attempt: 1,
  queue: "default",
  heartbeat_interval_seconds: 3600,
});

test("worker: action context can emit custom events", async () => {
  const client = new FakeClient();
  const worker = new Worker(client as never, {
    workerInstanceId: "worker-1",
    eventBatchSize: 10,
    logger: null,
  });

  worker.register("demo.action", async (params, _signal, ctx) => {
    ctx.emitEvent("scrape.started", { url: params.url as string });
    return { ok: true };
  });

  await (
    worker as unknown as {
      executeJob(job: JobClaim, signal: AbortSignal): Promise<void>;
    }
  ).executeJob(
    {
      job_id: "job_1",
      run_id: "run_1",
      workflow_name: "demo",
      step_name: "scrape",
      action: "demo.action",
      parameters: { url: "https://example.com" },
      attempt: 1,
      queue: "default",
      heartbeat_interval_seconds: 3600,
    },
    new AbortController().signal,
  );

  assert.deepStrictEqual(client.emitted, [
    {
      jobId: "job_1",
      type: "scrape.started",
      payload: { url: "https://example.com" },
    },
  ]);
  assert.equal(client.completed.length, 1);
  assert.equal(client.completed[0].status, "completed");
});

test("worker: claims next job only after current job completes", async () => {
  const client = new SequencedClient([blockJob("job_1")]);
  const worker = new Worker(client as never, {
    workerInstanceId: "worker-1",
    pollWaitSeconds: 1,
    logger: null,
  });
  const controller = new AbortController();
  let release!: () => void;
  const started = new Promise<void>((resolve) => {
    worker.register("demo.block", async () => {
      resolve();
      await new Promise<void>((r) => {
        release = r;
      });
      controller.abort();
      return { ok: true };
    });
  });

  const run = worker.run(controller.signal);
  await started;
  await new Promise((resolve) => setTimeout(resolve, 25));
  assert.equal(client.claims, 1);
  release();
  await run;
});

test("worker pool: uses distinct single-job workers", async () => {
  const client = new SequencedClient([
    blockJob("job_1"),
    blockJob("job_2"),
    blockJob("job_3"),
  ]);
  const pool = new WorkerPool(client as never, {
    workerInstanceIdPrefix: "pool-worker",
    count: 3,
    pollWaitSeconds: 1,
    logger: null,
  });
  const controller = new AbortController();
  let started = 0;
  let release!: () => void;
  const releasePromise = new Promise<void>((resolve) => {
    release = resolve;
  });
  const allStarted = new Promise<void>((resolve) => {
    pool.register("demo.block", async () => {
      started += 1;
      if (started === 3) resolve();
      await releasePromise;
      return { ok: true };
    });
  });

  const run = pool.run(controller.signal);
  await allStarted;
  assert.deepStrictEqual(new Set(client.workerIds), new Set([
    "pool-worker-1",
    "pool-worker-2",
    "pool-worker-3",
  ]));
  // Each pool child generates its own per-boot session token; three
  // children → three distinct, non-empty tokens.
  const tokens = client.sessionTokens.slice(0, 3);
  assert.equal(tokens.filter((t) => t).length, 3);
  assert.equal(new Set(tokens).size, 3);
  release();
  controller.abort();
  await run;
});
