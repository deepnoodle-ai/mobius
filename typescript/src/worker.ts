import { randomUUID } from "node:crypto";
import { hostname as systemHostname } from "node:os";
import type {
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
} from "./api/index.js";
import {
  AuthRevokedError,
  Client,
  LeaseLostError,
  PayloadTooLargeError,
  RateLimitedError,
  WorkerInstanceConflictError,
  type JobEventEntry,
} from "./client.js";

export type InstanceIDSource =
  | "configured"
  | "cloud_run_revision_instance"
  | "hostname"
  | "fly_machine_id"
  | "railway_replica_id"
  | "render_instance_id"
  | "system_hostname"
  | "generated_uuid";

const CLOUD_RUN_METADATA_TIMEOUT_MS = 1000;

/**
 * Resolve a per-process `worker_instance_id` from the runtime
 * environment. Order: explicit → Cloud Run K_REVISION + metadata →
 * HOSTNAME env → FLY_MACHINE_ID → RAILWAY_REPLICA_ID →
 * RENDER_INSTANCE_ID → `os.hostname()` suffixed with a per-boot
 * random tag (laptops and bare metal) → generated UUID.
 *
 * The system_hostname rung carries a random suffix because
 * `os.hostname()` identifies the host, not the process — two
 * processes started on the same machine would otherwise auto-detect
 * the same identifier and collide on the server's conflict detector.
 * Set {@link WorkerConfig.workerInstanceId} explicitly only when a
 * stable identity across restarts is desired (named singleton
 * workers).
 *
 * The returned source is informational only — workers log it once at
 * startup so operators can confirm the right platform was picked up.
 */
export async function resolveInstanceID(
  explicit: string | undefined,
): Promise<{ id: string; source: InstanceIDSource }> {
  if (explicit) {
    const trimmed = explicit.trim();
    if (trimmed) return { id: trimmed, source: "configured" };
  }
  const cloudRun = await cloudRunInstanceID();
  if (cloudRun) return { id: cloudRun, source: "cloud_run_revision_instance" };
  const hostname = (process.env.HOSTNAME ?? "").trim();
  if (hostname) return { id: hostname, source: "hostname" };
  const fly = (process.env.FLY_MACHINE_ID ?? "").trim();
  if (fly) return { id: fly, source: "fly_machine_id" };
  const railway = (process.env.RAILWAY_REPLICA_ID ?? "").trim();
  if (railway) return { id: railway, source: "railway_replica_id" };
  const render = (process.env.RENDER_INSTANCE_ID ?? "").trim();
  if (render) return { id: render, source: "render_instance_id" };
  try {
    const host = systemHostname().trim();
    if (host) {
      const suffix = randomUUID().replace(/-/g, "").slice(0, 8);
      return { id: `${host}-${suffix}`, source: "system_hostname" };
    }
  } catch {
    // fall through to UUID
  }
  return { id: randomUUID(), source: "generated_uuid" };
}

async function cloudRunInstanceID(): Promise<string | null> {
  // Returns null on any metadata-server failure rather than the bare
  // revision — every replica sharing the same revision string would
  // otherwise collapse onto one row and trip the conflict detector.
  // The next strategy (HOSTNAME, which Cloud Run sets per-instance)
  // takes over.
  const revision = (process.env.K_REVISION ?? "").trim();
  if (!revision) return null;
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), CLOUD_RUN_METADATA_TIMEOUT_MS);
    try {
      const resp = await fetch(
        "http://metadata.google.internal/computeMetadata/v1/instance/id",
        { headers: { "Metadata-Flavor": "Google" }, signal: ctrl.signal },
      );
      if (!resp.ok) return null;
      const id = (await resp.text()).trim();
      return id ? `${revision}-${id}` : null;
    } finally {
      clearTimeout(timer);
    }
  } catch {
    return null;
  }
}

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
  /**
   * Identifier for this worker process. Leave omitted for the
   * common case: the SDK auto-detects an identifier that is unique
   * per running process. Resolution prefers platform-native IDs
   * that are unique-per-replica by design (Cloud Run revision
   * instance, Kubernetes HOSTNAME, Fly machine, Railway replica,
   * Render instance), then falls back to OS hostname plus a
   * per-boot random suffix so two processes on the same host
   * (back-to-back tests, parallel CI workers) cannot collide.
   *
   * Set this explicitly only when you want a stable identity
   * across restarts of a named singleton worker. Two live
   * processes using the same explicit value in the same project
   * will collide and the second will fail with
   * {@link WorkerInstanceConflictError}.
   */
  workerInstanceId?: string;
  /**
   * Maximum number of jobs this worker holds in flight simultaneously.
   * Defaults to 1; raise to claim several jobs from one worker process
   * while still surfacing as a single row on the workers page.
   */
  concurrency?: number;
  /** Human-readable name reported to Mobius (e.g. "billing-worker"). */
  name?: string;
  /** Version string reported to Mobius (e.g. "1.0.0"). */
  version?: string;
  /**
   * Queue names this worker subscribes to. Empty (or omitted) means claim
   * jobs from any queue in the project. Runs default to the "default"
   * queue.
   */
  queues?: string[];
  /**
   * Optional filter of action names this worker will claim. When empty the
   * worker will only claim jobs for actions it has registered.
   */
  actions?: string[];
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
  /** Max buffered custom events per in-flight job before dropping oldest. */
  eventQueueSize?: number;
  /** Max custom events per HTTP batch. Defaults to 20. */
  eventBatchSize?: number;
}

/**
 * ActionFn implements a single Mobius action. Receives the JSON-decoded
 * parameters delivered with the job claim and returns a JSON-serialisable
 * result.
 */
export type ActionFn = (
  params: Record<string, unknown>,
  signal: AbortSignal,
  ctx: ActionContext,
) => Promise<unknown>;

export interface ActionContext {
  jobId: string;
  runId: string;
  projectId?: string;
  workerInstanceId: string;
  attempt: number;
  queue?: string;
  workflowName?: string;
  stepName?: string;
  action?: string;
  emitEvent(type: string, payload: Record<string, unknown>): void;
}

/**
 * Worker claims jobs from Mobius and dispatches each to the corresponding
 * registered action function. A *job* is a single action invocation on
 * behalf of a workflow run; the backend owns the workflow engine.
 *
 * With `concurrency > 1` the worker holds up to N jobs in flight
 * simultaneously, all reporting the same `worker_instance_id` and
 * session token; the admin UI shows one row with a saturation bar
 * rather than N independent rows.
 */
export class Worker {
  private readonly config: Required<
    Omit<WorkerConfig, "logger" | "heartbeatIntervalMs">
  > & {
    heartbeatIntervalMs: number | undefined;
  };
  private readonly logger: Logger;
  private readonly actions: Map<string, ActionFn>;
  private readonly sessionToken: string;
  private instanceIDResolved = false;
  private abortController = new AbortController();
  private authRevoked = false;

  constructor(
    private readonly client: Client,
    config: WorkerConfig,
    actions?: Map<string, ActionFn>,
  ) {
    const logger =
      config.logger === null ? silentLogger : (config.logger ?? defaultLogger);
    this.config = {
      workerInstanceId: config.workerInstanceId ?? "",
      concurrency: config.concurrency && config.concurrency > 0 ? config.concurrency : 1,
      name: config.name ?? "",
      version: config.version ?? "",
      queues: config.queues ?? [],
      actions: config.actions ?? [],
      pollWaitSeconds: config.pollWaitSeconds ?? 20,
      heartbeatIntervalMs: config.heartbeatIntervalMs,
      eventQueueSize: config.eventQueueSize ?? 256,
      eventBatchSize: config.eventBatchSize ?? 20,
    };
    this.logger = logger;
    this.actions = actions ?? new Map<string, ActionFn>();
    this.sessionToken = randomUUID();
  }

  /** Register an action function under the given name. */
  register(name: string, fn: ActionFn): this {
    this.actions.set(name, fn);
    return this;
  }

  /**
   * Start the claim loop. Returns a promise that resolves when the worker is
   * stopped via {@link stop} or the given signal is aborted. Throws
   * {@link AuthRevokedError} when the credential is revoked mid-flight
   * and {@link WorkerInstanceConflictError} when another live process
   * has already registered this worker_instance_id.
   *
   * Cancellation has two distinct shapes:
   *
   * - {@link stop} aborts the claim loop only — in-flight jobs continue
   *   to run to natural completion and the returned promise resolves
   *   once they all settle. This is graceful drain.
   * - The caller's `signal`, when aborted, propagates into both the
   *   claim loop and every running action. This is the emergency-stop
   *   path (e.g. SIGTERM with a hard deadline).
   */
  async run(signal?: AbortSignal): Promise<void> {
    this.abortController = new AbortController();
    // The claim loop watches both signals so stop() and the caller's
    // emergency abort both halt new claims; executeJob only watches the
    // caller's signal so stop() doesn't kill jobs that have already
    // been claimed.
    const callerSignal = signal;
    const combined = callerSignal
      ? anySignal(callerSignal, this.abortController.signal)
      : this.abortController.signal;

    if (!this.instanceIDResolved) {
      const { id, source } = await resolveInstanceID(
        this.config.workerInstanceId || undefined,
      );
      this.config.workerInstanceId = id;
      this.instanceIDResolved = true;
      this.logger.info(
        `[mobius] worker instance id ${id} (source: ${source})`,
      );
    }

    this.logger.info(
      `[mobius] worker ${this.config.workerInstanceId} started (concurrency=${this.config.concurrency})`,
    );

    const inflight = new Set<Promise<void>>();
    while (!combined.aborted) {
      if (this.authRevoked) break;
      while (inflight.size >= this.config.concurrency && !combined.aborted) {
        const aborted = abortAsPromise(combined);
        try {
          await Promise.race([...inflight, aborted.promise]);
        } finally {
          aborted.cancel();
        }
      }
      if (combined.aborted) break;

      let job: JobClaim | null;
      try {
        const claimReq: JobClaimRequest = {
          worker_instance_id: this.config.workerInstanceId,
          worker_session_token: this.sessionToken,
          concurrency_limit: this.config.concurrency,
          wait_seconds: this.config.pollWaitSeconds,
        };
        if (this.config.name) claimReq.worker_name = this.config.name;
        if (this.config.version) claimReq.worker_version = this.config.version;
        if (this.config.queues.length > 0) claimReq.queues = this.config.queues;
        if (this.config.actions.length > 0)
          claimReq.actions = this.config.actions;
        job = await this.client.claimJob(claimReq, combined);
      } catch (err) {
        if (combined.aborted) break;
        if (err instanceof AuthRevokedError) {
          this.logger.error("[mobius] claim rejected: credential revoked");
          throw err;
        }
        if (err instanceof WorkerInstanceConflictError) {
          this.logger.error(
            `[mobius] claim rejected: worker_instance_id ${this.config.workerInstanceId} is already in use by another live process`,
          );
          throw err;
        }
        this.logger.error("[mobius] claim error:", err);
        await sleep(2000, combined);
        continue;
      }

      if (job == null) {
        await sleep(0, combined);
        continue;
      }

      const task = this.executeJob(job, callerSignal).finally(() => {
        inflight.delete(task);
      });
      inflight.add(task);
    }

    await Promise.allSettled(inflight);
    this.logger.info(`[mobius] worker ${this.config.workerInstanceId} stopped`);
    if (this.authRevoked) {
      throw new AuthRevokedError();
    }
  }

  /** Gracefully stop the worker after in-flight jobs complete. */
  stop(): void {
    this.abortController.abort();
  }

  // ---------------------------------------------------------------------------

  private async executeJob(
    job: JobClaim,
    signal: AbortSignal | undefined,
  ): Promise<void> {
    const jobId = job.job_id;
    const workerInstanceId = this.config.workerInstanceId;
    const sessionToken = this.sessionToken;
    const attempt = job.attempt;
    this.logger.info(
      `[mobius] job ${jobId} claimed (workflow=${job.workflow_name}, step=${job.step_name}, action=${job.action}, attempt=${attempt})`,
    );

    const fn = this.actions.get(job.action);
    if (!fn) {
      const msg = `action ${JSON.stringify(job.action)} not registered on this worker`;
      this.logger.error(`[mobius] ${msg}`);
      await this.failJob(
        jobId,
        workerInstanceId,
        sessionToken,
        attempt,
        "ActionNotRegistered",
        msg,
      );
      return;
    }

    const actionController = new AbortController();
    const onAbort = () => actionController.abort();
    // Only the caller's signal (passed to run()) propagates into actions.
    // stop() does not flow through here — that's how graceful drain works.
    signal?.addEventListener("abort", onAbort, { once: true });
    const eventer = new JobEventer(this.client, job, {
      workerInstanceId,
      sessionToken,
      queueSize: this.config.eventQueueSize,
      batchSize: this.config.eventBatchSize,
      logger: this.logger,
    });
    const eventerPromise = eventer.run();
    const actionContext: ActionContext = {
      jobId,
      runId: job.run_id,
      projectId: this.client.project,
      workerInstanceId,
      attempt,
      queue: job.queue,
      workflowName: job.workflow_name,
      stepName: job.step_name,
      action: job.action,
      emitEvent: (type, payload) => eventer.emit(type, payload),
    };

    const interval = this.heartbeatInterval(job);
    let hbLost = false;
    const heartbeatTimer = setInterval(async () => {
      try {
        const hb = await this.client.heartbeatJob(jobId, {
          worker_instance_id: workerInstanceId,
          worker_session_token: sessionToken,
          attempt,
        });
        if (hb.directives.should_cancel) {
          this.logger.warn(`[mobius] job ${jobId}: cancel directive received`);
          actionController.abort();
        }
      } catch (err) {
        if (err instanceof AuthRevokedError) {
          this.logger.warn(
            `[mobius] job ${jobId}: credential revoked; cancelling action`,
          );
          this.authRevoked = true;
          actionController.abort();
        } else if (err instanceof LeaseLostError) {
          this.logger.warn(
            `[mobius] job ${jobId}: lease lost during heartbeat`,
          );
          hbLost = true;
          actionController.abort();
        } else {
          this.logger.error(`[mobius] job ${jobId}: heartbeat error:`, err);
        }
      }
    }, interval);

    try {
      const result = await fn(
        job.parameters,
        actionController.signal,
        actionContext,
      );
      clearInterval(heartbeatTimer);
      signal?.removeEventListener("abort", onAbort);
      eventer.stop();
      await eventerPromise;
      if (hbLost) return;
      const completeReq: JobCompleteRequest = {
        worker_instance_id: workerInstanceId,
        worker_session_token: sessionToken,
        attempt,
        status: "completed",
      };
      if (result != null) {
        completeReq.result_b64 = Buffer.from(JSON.stringify(result)).toString(
          "base64",
        );
      }
      await this.client.completeJob(jobId, completeReq);
      this.logger.info(`[mobius] job ${jobId} completed`);
    } catch (err) {
      clearInterval(heartbeatTimer);
      signal?.removeEventListener("abort", onAbort);
      eventer.stop();
      await eventerPromise;
      if (err instanceof AuthRevokedError) {
        this.logger.warn(
          `[mobius] job ${jobId}: credential revoked during complete; worker will exit`,
        );
        this.authRevoked = true;
        return;
      }
      if (err instanceof LeaseLostError || hbLost) {
        this.logger.warn(`[mobius] job ${jobId}: lease lost — will be retried`);
        return;
      }
      this.logger.error(`[mobius] job ${jobId} failed:`, err);
      const errType = actionController.signal.aborted ? "Timeout" : "Error";
      await this.failJob(
        jobId,
        workerInstanceId,
        sessionToken,
        attempt,
        errType,
        String(err),
      );
    }
  }

  private heartbeatInterval(job: JobClaim): number {
    if (this.config.heartbeatIntervalMs != null)
      return this.config.heartbeatIntervalMs;
    if (job.heartbeat_interval_seconds != null) {
      return job.heartbeat_interval_seconds * 1000;
    }
    return 10_000;
  }

  private async failJob(
    jobId: string,
    workerInstanceId: string,
    sessionToken: string,
    attempt: number,
    errorType: string,
    msg: string,
  ): Promise<void> {
    try {
      await this.client.completeJob(jobId, {
        worker_instance_id: workerInstanceId,
        worker_session_token: sessionToken,
        attempt,
        status: "failed",
        error_type: errorType,
        error_message: msg,
      });
    } catch (err) {
      this.logger.error(
        `[mobius] failed to report failure for job ${jobId}:`,
        err,
      );
    }
  }
}

export interface WorkerPoolConfig extends Omit<WorkerConfig, "workerInstanceId"> {
  /** Number of worker instances to run. Defaults to 1. */
  count?: number;
  /**
   * Prefix used to derive child instance IDs as `<prefix>-<index>`.
   * When omitted, the SDK generates a per-boot prefix; child workers
   * each get their own session token, so a pool of N produces N rows
   * on the workers page.
   */
  workerInstanceIdPrefix?: string;
}

/**
 * Runs multiple worker instances in one process. Most callers do not
 * need a pool — to run several jobs from one process, set
 * {@link WorkerConfig.concurrency} on a single {@link Worker} and the
 * admin UI will show one row with a saturation bar. Reach for a pool
 * only when each child should surface as its own row on the workers
 * page (independent draining, in-flight isolation).
 */
export class WorkerPool {
  private readonly config: Required<
    Omit<
      WorkerPoolConfig,
      "logger" | "heartbeatIntervalMs" | "workerInstanceIdPrefix"
    >
  > & {
    workerInstanceIdPrefix: string;
    heartbeatIntervalMs: number | undefined;
    logger: Logger | null | undefined;
  };
  private readonly actions = new Map<string, ActionFn>();
  private abortController = new AbortController();

  constructor(
    private readonly client: Client,
    config: WorkerPoolConfig,
  ) {
    const prefix =
      config.workerInstanceIdPrefix && config.workerInstanceIdPrefix !== ""
        ? config.workerInstanceIdPrefix
        : `worker-${randomUUID()}`;
    this.config = {
      workerInstanceIdPrefix: prefix,
      concurrency: config.concurrency && config.concurrency > 0 ? config.concurrency : 1,
      name: config.name ?? "",
      version: config.version ?? "",
      queues: config.queues ?? [],
      actions: config.actions ?? [],
      count: config.count && config.count > 0 ? config.count : 1,
      pollWaitSeconds: config.pollWaitSeconds ?? 20,
      heartbeatIntervalMs: config.heartbeatIntervalMs,
      logger: config.logger,
      eventQueueSize: config.eventQueueSize ?? 256,
      eventBatchSize: config.eventBatchSize ?? 20,
    };
  }

  /** Register an action function under the given name for every pool worker. */
  register(name: string, fn: ActionFn): this {
    this.actions.set(name, fn);
    return this;
  }

  /**
   * Start all workers in the pool. Rejects with AuthRevokedError if any child
   * worker sees credential revocation.
   */
  async run(signal?: AbortSignal): Promise<void> {
    this.abortController = new AbortController();
    const combined = signal
      ? anySignal(signal, this.abortController.signal)
      : this.abortController.signal;

    let authRevoked = false;
    let firstError: unknown;
    const workers = Array.from({ length: this.config.count }, (_, i) => {
      return new Worker(
        this.client,
        {
          workerInstanceId: `${this.config.workerInstanceIdPrefix}-${i + 1}`,
          concurrency: this.config.concurrency,
          name: this.config.name,
          version: this.config.version,
          queues: this.config.queues,
          actions: this.config.actions,
          pollWaitSeconds: this.config.pollWaitSeconds,
          heartbeatIntervalMs: this.config.heartbeatIntervalMs,
          logger: this.config.logger,
          eventQueueSize: this.config.eventQueueSize,
          eventBatchSize: this.config.eventBatchSize,
        },
        this.actions,
      );
    });

    await Promise.allSettled(
      workers.map(async (worker) => {
        try {
          await worker.run(combined);
        } catch (err) {
          if (err instanceof AuthRevokedError) {
            authRevoked = true;
            this.abortController.abort();
          } else if (!combined.aborted) {
            firstError ??= err;
            this.abortController.abort();
          }
        }
      }),
    );

    if (authRevoked) {
      throw new AuthRevokedError();
    }
    if (firstError != null) {
      throw firstError;
    }
  }

  /** Gracefully stop every worker in the pool after in-flight jobs complete. */
  stop(): void {
    this.abortController.abort();
  }
}

// sleep resolves after ms or when the signal aborts, whichever comes
// first. Detaches the abort listener on the natural-resolution path
// so it doesn't accumulate on a long-lived signal across thousands of
// poll cycles.
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  // AbortSignal does not retroactively dispatch abort to listeners
  // added after the signal already fired, so an already-aborted signal
  // would otherwise wait the full ms before resolving.
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const onAbort = () => {
      clearTimeout(t);
      resolve();
    };
    const t = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

// abortAsPromise returns a promise that resolves when the signal
// aborts, plus a cancel function the caller invokes when the promise
// is no longer needed (e.g. when something else won a Promise.race).
// Without the cancel handle, every Promise.race in the concurrency
// loop would leak a listener on the long-lived worker signal.
function abortAsPromise(signal: AbortSignal): {
  promise: Promise<void>;
  cancel: () => void;
} {
  if (signal.aborted) return { promise: Promise.resolve(), cancel: () => {} };
  let resolveFn!: () => void;
  const promise = new Promise<void>((resolve) => {
    resolveFn = resolve;
  });
  const onAbort = () => resolveFn();
  signal.addEventListener("abort", onAbort, { once: true });
  return {
    promise,
    cancel: () => signal.removeEventListener("abort", onAbort),
  };
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

class JobEventer {
  private readonly queue: JobEventEntry[] = [];
  private closed = false;
  private wake: (() => void) | null = null;

  constructor(
    private readonly client: Client,
    private readonly job: JobClaim,
    private readonly options: {
      workerInstanceId: string;
      sessionToken: string;
      queueSize: number;
      batchSize: number;
      logger: Logger;
    },
  ) {}

  emit(type: string, payload: Record<string, unknown>): void {
    if (!type || this.closed) return;
    if (this.queue.length >= this.options.queueSize) {
      this.queue.shift();
      this.options.logger.warn(
        "[mobius] custom event queue full; dropping oldest event",
      );
    }
    this.queue.push({ type, payload });
    this.wake?.();
  }

  stop(): void {
    this.closed = true;
    this.wake?.();
  }

  async run(): Promise<void> {
    while (!this.closed || this.queue.length > 0) {
      const batch = this.queue.splice(0, this.options.batchSize);
      if (batch.length === 0) {
        await new Promise<void>((resolve) => {
          this.wake = () => {
            this.wake = null;
            resolve();
          };
        });
        continue;
      }
      try {
        await this.client.emitJobEvents(this.job.job_id, {
          worker_instance_id: this.options.workerInstanceId,
          worker_session_token: this.options.sessionToken,
          attempt: this.job.attempt,
          events: batch,
        });
      } catch (err) {
        if (err instanceof LeaseLostError) {
          this.options.logger.warn(
            `[mobius] job ${this.job.job_id}: lease lost during custom event emit`,
          );
          return;
        }
        if (err instanceof PayloadTooLargeError) {
          this.options.logger.warn(
            `[mobius] job ${this.job.job_id}: custom event payload too large`,
          );
          continue;
        }
        if (err instanceof RateLimitedError) {
          this.options.logger.warn(
            `[mobius] job ${this.job.job_id}: custom event rate limited`,
          );
          continue;
        }
        this.options.logger.error(
          `[mobius] job ${this.job.job_id}: custom event emit failed:`,
          err,
        );
      }
    }
  }
}
