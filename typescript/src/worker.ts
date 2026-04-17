import type {
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
} from "./api/index.js";
import { Client, LeaseLostError } from "./client.js";

/**
 * Logger interface used by Worker for status and error output. Compatible with
 * the global `console` object, so `console` is a valid logger. Pass `null` to
 * `WorkerConfig.logger` to silence all output.
 */
export interface Logger {
  info(...args: unknown[]): void;
  warn(...args: unknown[]): void;
  error(...args: unknown[]): void;
}

const silentLogger: Logger = {
  info: () => {},
  warn: () => {},
  error: () => {},
};

const defaultLogger: Logger = {
  info: (...args) => console.log(...args),
  warn: (...args) => console.warn(...args),
  error: (...args) => console.error(...args),
};

export interface WorkerConfig {
  /** Stable, unique identifier for this worker instance. Required. */
  workerId: string;
  /** Human-readable name reported to Mobius (e.g. "billing-worker"). */
  name?: string;
  /** Version string reported to Mobius (e.g. "1.0.0"). */
  version?: string;
  /**
   * Queue names this worker subscribes to. Empty (or omitted) means claim
   * jobs from any queue in the namespace. Runs default to the "default"
   * queue.
   */
  queues?: string[];
  /**
   * Optional filter of action names this worker will claim. When empty the
   * worker will only claim jobs for actions it has registered.
   */
  actions?: string[];
  /** Maximum parallel executions. Defaults to 10. */
  concurrency?: number;
  /** Long-poll window per claim request in seconds (0–30). Defaults to 20. */
  pollWaitSeconds?: number;
  /**
   * Heartbeat interval in milliseconds. When unset, the SDK uses the interval
   * advertised by the server in the claim response, falling back to 10_000.
   */
  heartbeatIntervalMs?: number;
  /**
   * Logger for worker status and error output. Defaults to `console`. Pass
   * `null` to silence all worker output, or supply a custom {@link Logger}
   * (e.g. pino, winston) to route messages elsewhere.
   */
  logger?: Logger | null;
}

/**
 * ActionFn implements a single Mobius action. Receives the JSON-decoded
 * parameters delivered with the job claim and returns a JSON-serialisable
 * result.
 */
export type ActionFn = (
  params: Record<string, unknown>,
  signal: AbortSignal,
) => Promise<unknown>;

/**
 * Worker claims jobs from Mobius and dispatches each to the corresponding
 * registered action function. A *job* is a single action invocation on
 * behalf of a workflow run; the backend owns the workflow engine.
 */
export class Worker {
  private readonly config: Required<Omit<WorkerConfig, "logger" | "heartbeatIntervalMs">> & {
    heartbeatIntervalMs: number | undefined;
  };
  private readonly logger: Logger;
  private readonly actions = new Map<string, ActionFn>();
  private abortController = new AbortController();

  constructor(
    private readonly client: Client,
    config: WorkerConfig,
  ) {
    this.config = {
      workerId: config.workerId,
      name: config.name ?? "",
      version: config.version ?? "",
      queues: config.queues ?? [],
      actions: config.actions ?? [],
      concurrency: config.concurrency ?? 10,
      pollWaitSeconds: config.pollWaitSeconds ?? 20,
      heartbeatIntervalMs: config.heartbeatIntervalMs,
    };
    this.logger =
      config.logger === null ? silentLogger : (config.logger ?? defaultLogger);
  }

  /** Register an action function under the given name. */
  register(name: string, fn: ActionFn): this {
    this.actions.set(name, fn);
    return this;
  }

  /**
   * Start the claim loop. Returns a promise that resolves when the worker is
   * stopped via {@link stop} or the given signal is aborted.
   */
  async run(signal?: AbortSignal): Promise<void> {
    this.abortController = new AbortController();
    const combined = signal
      ? anySignal(signal, this.abortController.signal)
      : this.abortController.signal;

    this.logger.info(`[mobius] worker ${this.config.workerId} started`);

    const running = new Set<Promise<void>>();

    while (!combined.aborted) {
      if (running.size >= this.config.concurrency) {
        await Promise.race(running);
      }

      let task: JobClaim | null;
      try {
        const claimReq: JobClaimRequest = {
          worker_id: this.config.workerId,
          wait_seconds: this.config.pollWaitSeconds,
        };
        if (this.config.name) claimReq.worker_name = this.config.name;
        if (this.config.version) claimReq.worker_version = this.config.version;
        if (this.config.queues.length > 0) claimReq.queues = this.config.queues;
        if (this.config.actions.length > 0) claimReq.actions = this.config.actions;
        task = await this.client.claimJob(claimReq, combined);
      } catch (err) {
        if (combined.aborted) break;
        this.logger.error("[mobius] claim error:", err);
        await sleep(2000, combined);
        continue;
      }

      if (task == null) {
        await sleep(0, combined);
        continue;
      }

      const p = this.executeTask(task, combined).finally(() => running.delete(p));
      running.add(p);
    }

    await Promise.allSettled(running);
    this.logger.info(`[mobius] worker ${this.config.workerId} stopped`);
  }

  /** Gracefully stop the worker after in-flight tasks complete. */
  stop(): void {
    this.abortController.abort();
  }

  // ---------------------------------------------------------------------------

  private async executeTask(task: JobClaim, signal: AbortSignal): Promise<void> {
    const jobId = task.job_id;
    const workerId = this.config.workerId;
    const attempt = task.attempt;
    this.logger.info(
      `[mobius] job ${jobId} claimed (workflow=${task.workflow_name}, step=${task.step_name}, action=${task.action}, attempt=${attempt})`,
    );

    const fn = this.actions.get(task.action);
    if (!fn) {
      const msg = `action ${JSON.stringify(task.action)} not registered on this worker`;
      this.logger.error(`[mobius] ${msg}`);
      await this.failTask(jobId, workerId, attempt, "ActionNotRegistered", msg);
      return;
    }

    const actionController = new AbortController();
    const onAbort = () => actionController.abort();
    signal.addEventListener("abort", onAbort, { once: true });

    const interval = this.heartbeatInterval(task);
    let hbLost = false;
    const heartbeatTimer = setInterval(async () => {
      try {
        const hb = await this.client.heartbeatJob(jobId, {
          worker_id: workerId,
          attempt,
        });
        if (hb.directives.should_cancel) {
          this.logger.warn(`[mobius] job ${jobId}: cancel directive received`);
          actionController.abort();
        }
      } catch (err) {
        if (err instanceof LeaseLostError) {
          this.logger.warn(`[mobius] job ${jobId}: lease lost during heartbeat`);
          hbLost = true;
          actionController.abort();
        } else {
          this.logger.error(`[mobius] job ${jobId}: heartbeat error:`, err);
        }
      }
    }, interval);

    try {
      const result = await fn(task.parameters, actionController.signal);
      clearInterval(heartbeatTimer);
      signal.removeEventListener("abort", onAbort);
      if (hbLost) return;
      const completeReq: JobCompleteRequest = {
        worker_id: workerId,
        attempt,
        status: "completed",
      };
      if (result != null) {
        completeReq.result_b64 = Buffer.from(JSON.stringify(result)).toString("base64");
      }
      await this.client.completeJob(jobId, completeReq);
      this.logger.info(`[mobius] job ${jobId} completed`);
    } catch (err) {
      clearInterval(heartbeatTimer);
      signal.removeEventListener("abort", onAbort);
      if (err instanceof LeaseLostError || hbLost) {
        this.logger.warn(`[mobius] job ${jobId}: lease lost — will be retried`);
        return;
      }
      this.logger.error(`[mobius] job ${jobId} failed:`, err);
      const errType = actionController.signal.aborted ? "Timeout" : "Error";
      await this.failTask(jobId, workerId, attempt, errType, String(err));
    }
  }

  private heartbeatInterval(task: JobClaim): number {
    if (this.config.heartbeatIntervalMs != null) return this.config.heartbeatIntervalMs;
    if (task.heartbeat_interval_seconds != null) {
      return task.heartbeat_interval_seconds * 1000;
    }
    return 10_000;
  }

  private async failTask(
    jobId: string,
    workerId: string,
    attempt: number,
    errorType: string,
    msg: string,
  ): Promise<void> {
    try {
      await this.client.completeJob(jobId, {
        worker_id: workerId,
        attempt,
        status: "failed",
        error_type: errorType,
        error_message: msg,
      });
    } catch (err) {
      this.logger.error(`[mobius] failed to report failure for job ${jobId}:`, err);
    }
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });
}

function anySignal(...signals: AbortSignal[]): AbortSignal {
  const controller = new AbortController();
  for (const s of signals) {
    if (s.aborted) {
      controller.abort();
      break;
    }
    s.addEventListener("abort", () => controller.abort(), { once: true });
  }
  return controller.signal;
}
