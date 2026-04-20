import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Worker } from "../src/worker.js";
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

  async claimJob(): Promise<JobClaim | null> {
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

test("worker: action context can emit custom events", async () => {
  const client = new FakeClient();
  const worker = new Worker(client as never, {
    workerId: "worker-1",
    eventBatchSize: 10,
    logger: null,
  });

  worker.register("demo.action", async (params, _signal, ctx) => {
    ctx.emitEvent("scrape.started", { url: params.url as string });
    return { ok: true };
  });

  await (
    worker as unknown as {
      executeTask(task: JobClaim, signal: AbortSignal): Promise<void>;
    }
  ).executeTask(
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
