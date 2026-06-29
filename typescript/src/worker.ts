import { randomUUID } from "node:crypto";
import { hostname as systemHostname } from "node:os";
import type {
  WorkerSocketClaimedJob,
  WorkerSocketGenerationDeltaFrame,
  WorkerSocketJobHeartbeatFrame,
  WorkerSocketJobReportFrame,
  WorkerSocketJobsClaimFrame,
  WorkerSocketModelCapability,
  WorkerSocketRegisterFrame,
} from "./api/index.js";
import {
  AuthRevokedError,
  Client,
  WorkerInstanceConflictError,
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
const BOOT_INSTANCE_ID = randomUUID();

export async function resolveInstanceID(
  explicit: string | undefined,
): Promise<{ id: string; source: InstanceIDSource }> {
  if (explicit?.trim()) return { id: explicit.trim(), source: "configured" };
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
      return {
        id: `${host}-${BOOT_INSTANCE_ID.replace(/-/g, "").slice(0, 8)}`,
        source: "system_hostname",
      };
    }
  } catch {
    // fall through
  }
  return { id: BOOT_INSTANCE_ID, source: "generated_uuid" };
}

async function cloudRunInstanceID(): Promise<string | null> {
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

export interface Logger {
  info(...args: unknown[]): void;
  warn(...args: unknown[]): void;
  error(...args: unknown[]): void;
}

const silentLogger: Logger = { info: () => {}, warn: () => {}, error: () => {} };
const defaultLogger: Logger = {
  info: (...args) => console.log(...args),
  warn: (...args) => console.warn(...args),
  error: (...args) => console.error(...args),
};

export interface ModelCapability {
  provider: string;
  model: string;
}

export interface WorkerConfig {
  workerInstanceId?: string;
  concurrency?: number;
  name?: string;
  version?: string;
  queues?: string[];
  actions?: string[];
  models?: ModelCapability[];
  reconnectDelayMs?: number;
  heartbeatIntervalMs?: number;
  logger?: Logger | null;
}

export interface ActionContext {
  jobId: string;
  runId?: string;
  sessionId?: string;
  agentTurnId?: string;
  toolCallId?: string;
  projectId?: string;
  workerInstanceId: string;
  attempt: number;
  queue?: string;
  stepId?: string;
  action?: string;
  emitEvent(type: string, payload: Record<string, unknown>): void;
}

export type ActionFn = (
  params: Record<string, unknown>,
  signal: AbortSignal,
  ctx: ActionContext,
) => Promise<unknown>;

export interface GenerationJob {
  jobId: string;
  runId?: string;
  sessionId?: string;
  agentTurnId?: string;
  toolCallId?: string;
  provider?: string;
  model?: string;
  spec: Record<string, unknown>;
}

export type GenerationEmitter = (delta: Record<string, unknown>) => void;
export type GenerationFn = (
  ctx: ActionContext,
  job: GenerationJob,
  emit: GenerationEmitter,
) => Promise<Record<string, unknown>>;

export class Worker {
  private readonly logger: Logger;
  private readonly actions = new Map<string, ActionFn>();
  private readonly generators = new Map<string, GenerationFn>();
  // Concrete provider/model pairs passed to registerGenerator (wildcards
  // excluded). Merged with config.models when advertising so a registered
  // generator is always announced — the advertised set and the handler set
  // cannot drift.
  private readonly registeredModels: ModelCapability[] = [];
  private sessionToken = "";
  private stopping = false;
  private readonly stopController = new AbortController();

  // claimResponseTimeoutMs bounds how long an outstanding jobs.claim may go
  // unanswered before the worker abandons the socket and reconnects. The server
  // answers a claim within its long-poll window (tens of seconds) even when
  // idle — returning an empty jobs.claimed — so a claim unanswered well past
  // that means the frame or its response was lost, or a server-side handler
  // wedged. Without this bound a single lost claim/response strands the worker:
  // claimOutstanding stays set, work.available and post-job re-claims both
  // no-op, no further claims are sent, and Mobius Cloud's dead-worker reaper
  // eventually fails the run's pending jobs as environment_worker_unavailable.
  // Overridable in tests.
  private claimResponseTimeoutMs = 60_000;

  constructor(
    private readonly client: Client,
    private readonly config: WorkerConfig = {},
    actions?: Map<string, ActionFn>,
  ) {
    this.logger =
      config.logger === null ? silentLogger : (config.logger ?? defaultLogger);
    if (actions) {
      for (const [name, fn] of actions) this.actions.set(name, fn);
    }
  }

  register(name: string, fn: ActionFn): this {
    this.actions.set(name, fn);
    return this;
  }

  registerGenerator(provider: string, model: string, fn: GenerationFn): this {
    this.generators.set(`${provider}/${model}`, fn);
    // A concrete provider/model is advertised automatically (as if added to
    // config.models) so it appears in the worker-model catalog and is routable.
    // A "*" wildcard has no concrete model id to advertise; pair it with an
    // explicit config.models entry to make it routable.
    if (model !== "*") {
      this.registeredModels.push({ provider, model });
    }
    return this;
  }

  stop(): void {
    this.stopping = true;
    this.stopController.abort();
  }

  async run(signal?: AbortSignal): Promise<void> {
    const { id, source } = await resolveInstanceID(this.config.workerInstanceId);
    this.config.workerInstanceId = id;
    this.logger.info(`[mobius] worker instance id ${id} (source: ${source})`);
    const runSignal = mergeSignals(this.stopController.signal, signal);
    while (!this.stopping && !runSignal.aborted) {
      try {
        await this.runSocket(runSignal);
      } catch (err) {
        if (runSignal.aborted || this.stopping) return;
        if (
          err instanceof AuthRevokedError ||
          err instanceof WorkerInstanceConflictError
        ) {
          // Terminal protocol failures must not reconnect: a revoked
          // credential needs a fresh process, and a duplicate
          // worker_instance_id means another live process owns this
          // identity. Rethrow so a supervisor restarts (rotated credential)
          // or an operator fixes the duplicate ID, instead of spinning in a
          // reconnect loop.
          throw err;
        }
        this.logger.warn("[mobius] worker socket disconnected; reconnecting", err);
        await sleep(this.config.reconnectDelayMs ?? 2000, runSignal);
      }
    }
  }

  // openSocket dials the worker WebSocket. Isolated so tests can substitute a
  // fake socket and drive the frame loop deterministically.
  protected openSocket(): WebSocket {
    return new WebSocket(this.client.workerSocketURL());
  }

  private async runSocket(signal?: AbortSignal): Promise<void> {
    const ws = this.openSocket();
    const concurrency = this.config.concurrency && this.config.concurrency > 0
      ? this.config.concurrency
      : 1;
    const running = new Map<string, AbortController>();
    let claimOutstanding = false;
    let outstandingClaimId: string | undefined;

    // claimDeadline drops the socket when an outstanding jobs.claim goes
    // unanswered for claimResponseTimeoutMs, forcing run() to reconnect for a
    // clean slate (re-register + fresh claim). See the field doc.
    let claimTimer: ReturnType<typeof setTimeout> | undefined;
    let claimTimedOut = false;
    const disarmClaimDeadline = () => {
      if (claimTimer !== undefined) {
        clearTimeout(claimTimer);
        claimTimer = undefined;
      }
    };
    const armClaimDeadline = () => {
      disarmClaimDeadline();
      claimTimer = setTimeout(() => {
        claimTimer = undefined;
        claimTimedOut = true;
        try {
          ws.close();
        } catch {
          // ignore
        }
      }, this.claimResponseTimeoutMs);
    };

    const claim = () => {
      if (claimOutstanding || this.stopping) return;
      const available = concurrency - running.size;
      if (available <= 0) return;
      const messageId = msgID();
      const actions = this.config.actions?.length
        ? this.config.actions
        : [...this.actions.keys()].sort();
      const frame: WorkerSocketJobsClaimFrame = removeUndefined({
        type: "jobs.claim",
        message_id: messageId,
        available_slots: available,
        queues: this.config.queues?.length ? this.config.queues : undefined,
        action_names: actions.length ? actions : undefined,
        models: this.modelCapabilities(),
      }) as WorkerSocketJobsClaimFrame;
      claimOutstanding = true;
      outstandingClaimId = messageId;
      ws.send(JSON.stringify(frame));
      // Bound how long this claim may go unanswered; a lost claim/response
      // would otherwise strand the worker until the connection's max-age.
      armClaimDeadline();
    };

    try {
      await waitForOpen(ws, signal);
      ws.send(JSON.stringify(this.registerFrame(concurrency)));

      for await (const frame of socketFrames(ws, signal)) {
        switch (frame.type) {
          case "worker.registered":
            this.sessionToken = String(frame.worker_session_token ?? "");
            claim();
            break;
          case "jobs.claimed":
            claimOutstanding = false;
            disarmClaimDeadline();
            for (const job of (frame.jobs ?? []) as WorkerSocketClaimedJob[]) {
              if (running.size >= concurrency) break;
              const ctrl = new AbortController();
              running.set(job.id, ctrl);
              void this.executeJob(ws, job, ctrl.signal).finally(() => {
                running.delete(job.id);
                claim();
              });
            }
            // An empty jobs.claimed does not re-claim here: the per-job
            // re-claim above drains any backlog, and there is no hot-poll to
            // guard against (cf. the Go worker's ticker-driven claim loop).
            break;
          case "work.available":
            claim();
            break;
          case "job.cancel": {
            const ctrl = running.get(String(frame.job_id));
            ctrl?.abort();
            break;
          }
          case "job.heartbeat.ack": {
            const cancel = frame.cancel as { job_id?: string } | undefined;
            if (cancel?.job_id) running.get(cancel.job_id)?.abort();
            break;
          }
          case "worker.drain":
            this.stopping = true;
            ws.send(JSON.stringify({ type: "worker.draining", message_id: msgID() }));
            break;
          case "keepalive":
            ws.send(JSON.stringify({ type: "keepalive", message_id: msgID() }));
            break;
          case "error": {
            const terminal = this.terminalProtocolError(
              frame.error as { code?: string; message?: string } | undefined,
            );
            if (terminal) throw terminal;
            // If this nonterminal error answers our outstanding claim (matched
            // by message id), clear claimOutstanding so the next work.available
            // or post-job re-claim issues a fresh claim. Otherwise a claim the
            // server rejects with a recoverable error would stay "in flight"
            // forever and silently stop the worker from claiming.
            if (
              claimOutstanding &&
              outstandingClaimId &&
              String(frame.message_id ?? "") === outstandingClaimId
            ) {
              claimOutstanding = false;
              disarmClaimDeadline();
            }
            this.logger.error("[mobius] worker socket protocol error", frame.error);
            break;
          }
        }
        if (this.stopping && running.size === 0) {
          ws.close();
          return;
        }
      }
    } finally {
      disarmClaimDeadline();
    }

    // A fired claim deadline means this socket is a dead work channel; surface
    // it as an error so run() logs and reconnects, rather than treating the
    // closed socket as a clean shutdown.
    if (claimTimedOut && !this.stopping && !signal?.aborted) {
      throw new Error(
        `[mobius] jobs.claim went unanswered for ${this.claimResponseTimeoutMs}ms; reconnecting`,
      );
    }
  }

  // terminalProtocolError maps a worker-socket protocol error frame to a
  // terminal error the worker must not reconnect through, or undefined for
  // protocol errors it can keep running past (logged by the caller).
  // invalid_actor means the credential was revoked; worker_instance_conflict
  // means another live process already owns this worker_instance_id.
  private terminalProtocolError(
    error: { code?: string; message?: string } | undefined,
  ): Error | undefined {
    switch (error?.code) {
      case "invalid_actor":
        return new AuthRevokedError();
      case "worker_instance_conflict":
        return new WorkerInstanceConflictError(
          this.config.workerInstanceId,
          this.client.project,
          error.message,
        );
      default:
        return undefined;
    }
  }

  private registerFrame(concurrency: number): WorkerSocketRegisterFrame {
    const actions = this.config.actions?.length
      ? this.config.actions
      : [...this.actions.keys()].sort();
    return removeUndefined({
      type: "worker.register",
      message_id: msgID(),
      worker_instance_id: this.config.workerInstanceId ?? "",
      worker_session_token: this.sessionToken || undefined,
      concurrency_limit: concurrency,
      available_slots: concurrency,
      name: this.config.name,
      version: this.config.version,
      queues: this.config.queues?.length ? this.config.queues : undefined,
      action_names: actions.length ? actions : undefined,
      models: this.modelCapabilities(),
    }) as WorkerSocketRegisterFrame;
  }

  // modelCapabilities is the set of models the worker announces to Mobius Cloud:
  // the explicit config.models plus every concrete model registered via
  // registerGenerator, deduplicated and with wildcards excluded.
  private modelCapabilities(): WorkerSocketModelCapability[] | undefined {
    const seen = new Set<string>();
    const models: WorkerSocketModelCapability[] = [];
    for (const m of [...(this.config.models ?? []), ...this.registeredModels]) {
      if (!m.provider || !m.model || m.model === "*") continue;
      const key = `${m.provider}/${m.model}`;
      if (seen.has(key)) continue;
      seen.add(key);
      models.push({ provider: m.provider, model: m.model });
    }
    return models.length > 0 ? models : undefined;
  }

  private async executeJob(
    ws: WebSocket,
    job: WorkerSocketClaimedJob,
    signal: AbortSignal,
  ): Promise<void> {
    const heartbeat = setInterval(() => {
      const frame: WorkerSocketJobHeartbeatFrame = {
        type: "job.heartbeat",
        message_id: msgID(),
        job_id: job.id,
        lease_token: job.lease_token,
      };
      ws.send(JSON.stringify(frame));
    }, this.config.heartbeatIntervalMs ?? job.heartbeat_cadence_seconds * 1000);

    const ctx = this.actionContext(job);
    try {
      let result: unknown;
      if (job.kind === "action_execution") {
        const actionName = job.action_name ?? String(job.spec.action_name ?? "");
        const fn = this.actions.get(actionName);
        if (!fn) throw new Error(`action ${JSON.stringify(actionName)} is not registered`);
        result = await fn(parameters(job), signal, ctx);
      } else if (job.kind === "llm_generation") {
        const fn = this.generator(job.provider, job.model);
        if (!fn) throw new Error(`generation ${job.provider}/${job.model} is not registered`);
        let seq = 0;
        result = await fn(ctx, {
          jobId: job.id,
          runId: job.run_id,
          sessionId: job.session_id,
          agentTurnId: job.agent_turn_id,
          toolCallId: job.tool_call_id,
          provider: job.provider,
          model: job.model,
          spec: job.spec,
        }, (delta) => {
          seq += 1;
          const frame: WorkerSocketGenerationDeltaFrame = {
            type: "generation.delta",
            message_id: msgID(),
            job_id: job.id,
            lease_token: job.lease_token,
            sequence: seq,
            delta,
          };
          ws.send(JSON.stringify(frame));
        });
      } else {
        throw new Error(`unsupported job kind ${job.kind}`);
      }
      this.report(ws, job, "completed", result);
    } catch (err) {
      this.report(ws, job, "failed", undefined, signal.aborted ? "Cancelled" : "Error", String(err));
    } finally {
      clearInterval(heartbeat);
    }
  }

  private generator(provider?: string, model?: string): GenerationFn | undefined {
    if (!provider || !model) return undefined;
    return this.generators.get(`${provider}/${model}`) ?? this.generators.get(`${provider}/*`);
  }

  private actionContext(job: WorkerSocketClaimedJob): ActionContext {
    return {
      jobId: job.id,
      runId: job.run_id,
      sessionId: job.session_id,
      agentTurnId: job.agent_turn_id,
      toolCallId: job.tool_call_id,
      projectId: this.client.project,
      workerInstanceId: this.config.workerInstanceId ?? "",
      attempt: job.claim_attempt,
      queue: job.queue,
      stepId: job.step_id,
      action: job.action_name,
      emitEvent: () => {
        this.logger.warn("[mobius] custom worker events are not supported by the WebSocket protocol yet");
      },
    };
  }

  private report(
    ws: WebSocket,
    job: WorkerSocketClaimedJob,
    status: "completed" | "failed",
    result?: unknown,
    errorType?: string,
    errorMessage?: string,
  ): void {
    const frame: WorkerSocketJobReportFrame = removeUndefined({
      type: "job.report",
      message_id: msgID(),
      job_id: job.id,
      lease_token: job.lease_token,
      status,
      result: status === "completed" ? resultMap(result) : undefined,
      error_type: errorType,
      error_message: errorMessage,
    }) as WorkerSocketJobReportFrame;
    ws.send(JSON.stringify(frame));
  }
}

export interface WorkerPoolConfig extends Omit<WorkerConfig, "workerInstanceId"> {
  count?: number;
  workerInstanceIdPrefix?: string;
}

export class WorkerPool {
  private readonly actions = new Map<string, ActionFn>();
  private readonly generators = new Map<string, GenerationFn>();
  constructor(
    private readonly client: Client,
    private readonly config: WorkerPoolConfig,
  ) {}

  register(name: string, fn: ActionFn): this {
    this.actions.set(name, fn);
    return this;
  }

  registerGenerator(provider: string, model: string, fn: GenerationFn): this {
    this.generators.set(`${provider}/${model}`, fn);
    return this;
  }

  async run(signal?: AbortSignal): Promise<void> {
    const count = this.config.count && this.config.count > 0 ? this.config.count : 1;
    const prefix = this.config.workerInstanceIdPrefix ?? `worker-${randomUUID()}`;
    const workers = Array.from({ length: count }, (_, i) => {
      const worker = new Worker(
        this.client,
        { ...this.config, workerInstanceId: `${prefix}-${i + 1}` },
        this.actions,
      );
      for (const [key, fn] of this.generators) {
        // Split on the first "/" only: model ids may contain slashes
        // (e.g. "meta-llama/Llama-3"), which "/".split(key, 2) would truncate.
        const slash = key.indexOf("/");
        const provider = key.slice(0, slash);
        const model = key.slice(slash + 1);
        worker.registerGenerator(provider, model, fn);
      }
      return worker.run(signal);
    });
    await Promise.all(workers);
  }
}

function parameters(job: WorkerSocketClaimedJob): Record<string, unknown> {
  const raw = job.spec.parameters;
  return raw && typeof raw === "object" && !Array.isArray(raw)
    ? (raw as Record<string, unknown>)
    : {};
}

function resultMap(result: unknown): Record<string, unknown> {
  if (result && typeof result === "object" && !Array.isArray(result)) {
    return result as Record<string, unknown>;
  }
  return { output: result };
}

function msgID(): string {
  return `msg_${randomUUID()}`;
}

function removeUndefined<T extends object>(obj: T): T {
  return Object.fromEntries(
    Object.entries(obj).filter(([, v]) => v !== undefined),
  ) as T;
}

function mergeSignals(...signals: (AbortSignal | undefined)[]): AbortSignal {
  const controller = new AbortController();
  for (const signal of signals) {
    if (!signal) continue;
    if (signal.aborted) {
      controller.abort(signal.reason);
      break;
    }
    signal.addEventListener("abort", () => controller.abort(signal.reason), {
      once: true,
    });
  }
  return controller.signal;
}

function waitForOpen(ws: WebSocket, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    // Use the instance OPEN constant, not the global WebSocket.OPEN: the global
    // WebSocket is not defined on Node < 22, which would throw a ReferenceError
    // here (and breaks tests that substitute a fake socket via openSocket).
    if (ws.readyState === ws.OPEN) {
      resolve();
      return;
    }
    const onOpen = () => resolve();
    const onError = () => reject(new Error("worker socket failed to open"));
    ws.addEventListener("open", onOpen, { once: true });
    ws.addEventListener("error", onError, { once: true });
    signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
  });
}

async function* socketFrames(
  ws: WebSocket,
  signal?: AbortSignal,
): AsyncGenerator<Record<string, unknown>> {
  const queue: Record<string, unknown>[] = [];
  let wake: (() => void) | undefined;
  let done = false;
  ws.addEventListener("message", (event) => {
    queue.push(JSON.parse(String(event.data)) as Record<string, unknown>);
    wake?.();
  });
  ws.addEventListener("close", () => {
    done = true;
    wake?.();
  });
  signal?.addEventListener("abort", () => {
    done = true;
    try {
      ws.close();
    } catch {
      // ignore
    }
    wake?.();
  });
  while (!done || queue.length > 0) {
    if (queue.length === 0) {
      await new Promise<void>((resolve) => {
        wake = resolve;
      });
      wake = undefined;
      continue;
    }
    yield queue.shift()!;
  }
}

async function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) return;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason ?? new Error("aborted"));
      },
      { once: true },
    );
  });
}
