import type {
  CancelLoopRunRequest,
  CreateLoopRequest,
  Loop,
  LoopListResponse,
  LoopRun,
  LoopRunEvent,
  LoopRunListResponse,
  LoopRunSource,
  LoopRunStatus,
  LoopStatus,
  SignalLoopRunRequest,
  StartLoopRunRequest,
  TagMap,
  UpdateLoopRequest,
} from "./api/index.js";
import {
  DEFAULT_MAX_RETRIES,
  RateLimitError,
  wrapFetchWithRetry,
} from "./retry.js";

export { RateLimitError } from "./retry.js";

export interface ClientOptions {
  apiKey: string;
  baseURL?: string;
  /** Project handle used for all project-scoped operations. */
  project?: string;
  /** Compatibility alias for older callers. */
  namespace?: string;
  /** Fetch timeout in milliseconds. Defaults to 60_000. */
  timeoutMs?: number;
  /** Number of retries for 429/503 responses. */
  retry?: number;
}

export const DEFAULT_BASE_URL = "https://api.mobiusops.ai";
export const DEFAULT_PROJECT = "default";
export const DEFAULT_NAMESPACE = DEFAULT_PROJECT;

export class AuthRevokedError extends Error {
  constructor() {
    super("mobius: credential revoked");
    this.name = "AuthRevokedError";
  }
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

export class LeaseLostError extends Error {
  constructor(public readonly jobId: string) {
    super(`lease lost for job ${jobId}`);
    this.name = "LeaseLostError";
  }
}

export class PayloadTooLargeError extends Error {
  constructor(public readonly jobId: string) {
    super(`custom event payload too large for job ${jobId}`);
    this.name = "PayloadTooLargeError";
  }
}

export class RateLimitedError extends RateLimitError {
  readonly jobId: string;

  constructor(jobId: string, retryAfter?: number) {
    super({
      retryAfter: retryAfter ?? 0,
      message:
        retryAfter != null
          ? `custom event rate limited for job ${jobId} (retry after ${retryAfter}s)`
          : `custom event rate limited for job ${jobId}`,
    });
    this.name = "RateLimitedError";
    this.jobId = jobId;
  }
}

export class WorkerInstanceConflictError extends Error {
  constructor(
    public readonly workerInstanceId: string | undefined,
    public readonly projectHandle: string,
    message?: string,
  ) {
    super(
      message ??
        (workerInstanceId
          ? `mobius: worker_instance_id ${JSON.stringify(workerInstanceId)} is already registered in project ${JSON.stringify(projectHandle)} by another live process`
          : "mobius: worker instance conflict"),
    );
    this.name = "WorkerInstanceConflictError";
  }
}

const HANDLE_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;

function extractHandleFromApiKey(apiKey: string): string | null {
  if (!apiKey.startsWith("mbx_") && !apiKey.startsWith("mbc_")) return null;
  const dot = apiKey.lastIndexOf(".");
  if (dot < 0 || dot === apiKey.length - 1) return null;
  const handle = apiKey.slice(dot + 1);
  if (!HANDLE_RE.test(handle)) {
    throw new ConfigError(
      `invalid project handle suffix in API key: ${JSON.stringify(handle)}`,
    );
  }
  return handle;
}

export interface LoopOptions {
  name: string;
  description?: string;
  agent_id?: string;
  default_config?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  tags?: TagMap;
  /**
   * Authoring definition for the loop. Recognised keys are schema_version,
   * steps, event, config, triggers, defaults, limits, output, repositories,
   * cleanup, …. When it carries steps the loop is runnable immediately.
   */
  spec?: Record<string, unknown>;
}

export interface UpdateLoopOptions {
  name?: string;
  description?: string;
  agent_id?: string;
  default_config?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  status?: LoopStatus;
  tags?: TagMap;
  /** Replacement authoring definition. See {@link LoopOptions.spec}. */
  spec?: Record<string, unknown>;
}

export interface ListLoopsOptions {
  status?: LoopStatus;
  cursor?: string;
  limit?: number;
}

export interface StartRunOptions {
  /** Exact event object that starts the run, reachable in templates at `event.*`. */
  event?: Record<string, unknown>;
  /** Optional static or caller-provided configuration, reachable at `config.*`. */
  config?: Record<string, unknown>;
  /** Optional caller-supplied event metadata; Mobius adds its own provenance. */
  meta?: Record<string, unknown>;
  source?: LoopRunSource;
  external_id?: string;
}

export interface ListRunsOptions {
  status?: LoopRunStatus;
  loop_id?: string;
  cursor?: string;
  limit?: number;
}

export interface WatchRunOptions {
  since?: number;
  signal?: AbortSignal;
}

export interface WaitRunOptions extends WatchRunOptions {
  reconnectDelayMs?: number;
}

export type RunEvent = LoopRunEvent;

export class Client {
  private readonly baseURL: string;
  readonly project: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof globalThis.fetch;

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    const explicitProject = opts.project ?? opts.namespace;
    const handleInKey = extractHandleFromApiKey(opts.apiKey);
    if (handleInKey != null) {
      if (
        explicitProject != null &&
        explicitProject !== DEFAULT_PROJECT &&
        explicitProject !== handleInKey
      ) {
        throw new ConfigError(
          `project=${JSON.stringify(explicitProject)} conflicts with the handle embedded in the API key (${JSON.stringify(handleInKey)})`,
        );
      }
      this.project = handleInKey;
    } else {
      this.project = explicitProject ?? DEFAULT_PROJECT;
    }
    this.headers = {
      Authorization: `Bearer ${opts.apiKey}`,
      "Content-Type": "application/json",
    };
    this.timeoutMs = opts.timeoutMs ?? 60_000;
    this.fetchFn = wrapFetchWithRetry(
      (input, init) => globalThis.fetch(input, init),
      { maxRetries: opts.retry ?? DEFAULT_MAX_RETRIES },
    );
  }

  workerSocketURL(): string {
    const url = new URL(this.baseURL);
    if (url.protocol === "http:") url.protocol = "ws:";
    if (url.protocol === "https:") url.protocol = "wss:";
    url.pathname = `${url.pathname.replace(/\/$/, "")}/v1/projects/${encodeURIComponent(this.project)}/workers/socket`;
    url.search = "";
    return url.toString();
  }

  async listLoops(opts: ListLoopsOptions = {}): Promise<LoopListResponse> {
    const resp = await this.request(withQuery("/v1/projects/:project/loops", opts), {
      method: "GET",
    });
    return (await resp.json()) as LoopListResponse;
  }

  async getLoop(id: string): Promise<Loop> {
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Loop;
  }

  async createLoop(opts: LoopOptions): Promise<Loop> {
    const body = {
      schema_version: "1" as const,
      ...(opts.spec ?? {}),
      ...removeUndefined({
        name: opts.name,
        description: opts.description,
        agent_id: opts.agent_id,
        default_config: opts.default_config,
        settings: opts.settings,
        tags: opts.tags,
      }),
    } as CreateLoopRequest;
    const resp = await this.request("/v1/projects/:project/loops", {
      method: "POST",
      body,
    });
    return (await resp.json()) as Loop;
  }

  async updateLoop(id: string, opts: UpdateLoopOptions): Promise<Loop> {
    const { spec, ...meta } = opts;
    const body = {
      ...(spec ?? {}),
      ...removeUndefined(meta),
    } as UpdateLoopRequest;
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "PATCH", body },
    );
    return (await resp.json()) as Loop;
  }

  async deleteLoop(id: string): Promise<void> {
    await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
  }

  async startRun(
    loopId: string,
    opts: StartRunOptions = {},
  ): Promise<LoopRun> {
    const body: StartLoopRunRequest = removeUndefined({
      event: opts.event,
      config: opts.config,
      meta: opts.meta,
      source: opts.source,
      idempotency_key: opts.external_id,
    });
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(loopId)}/runs`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async listRuns(opts: ListRunsOptions = {}): Promise<LoopRunListResponse> {
    const resp = await this.request(
      withQuery("/v1/projects/:project/runs", opts),
      { method: "GET" },
    );
    return (await resp.json()) as LoopRunListResponse;
  }

  async getRun(runId: string): Promise<LoopRun> {
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as LoopRun;
  }

  async cancelRun(runId: string, reason?: string): Promise<LoopRun> {
    const body: CancelLoopRunRequest = removeUndefined({ reason });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/cancel`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async signalRun(
    runId: string,
    stepKey: string,
    result?: Record<string, unknown>,
  ): Promise<LoopRun> {
    const body: SignalLoopRunRequest = removeUndefined({
      step_key: stepKey,
      result,
    });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/signals`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async *watchRun(
    runId: string,
    opts: WatchRunOptions = {},
  ): AsyncGenerator<LoopRunEvent> {
    const path = withQuery(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/events.stream`,
      opts.since && opts.since > 0 ? { after_sequence: opts.since } : {},
    );
    const resp = await this.fetchFn(this.url(path), {
      method: "GET",
      headers: this.headers,
      signal: opts.signal,
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API GET ${path}: HTTP ${resp.status}: ${text}`);
    }
    if (!resp.body) {
      throw new Error("mobius API GET run events: response body is not readable");
    }
    for await (const evt of parseSSE(resp.body)) {
      if (!evt.data) continue;
      yield JSON.parse(evt.data) as LoopRunEvent;
    }
  }

  async waitRun(runId: string, opts: WaitRunOptions = {}): Promise<LoopRun> {
    let since = opts.since ?? 0;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    for (;;) {
      const run = await this.getRun(runId);
      if (isTerminalRunStatus(run.status)) return run;
      try {
        for await (const ev of this.watchRun(runId, { ...opts, since })) {
          if (ev.sequence > since) since = ev.sequence;
          const status = ev.payload?.status;
          if (typeof status === "string" && isTerminalRunStatus(status)) {
            return await this.getRun(runId);
          }
        }
      } catch (err) {
        if (opts.signal?.aborted) throw err;
      }
      await delay(reconnectDelayMs, opts.signal);
    }
  }

  private async request(
    path: string,
    opts: { method: string; body?: unknown; signal?: AbortSignal | undefined },
  ): Promise<Response> {
    const timeout = AbortSignal.timeout(this.timeoutMs);
    const signal = opts.signal ? anySignal(opts.signal, timeout) : timeout;
    const init: RequestInit = {
      method: opts.method,
      headers: this.headers,
      signal,
    };
    if (opts.body != null) init.body = JSON.stringify(opts.body);
    const resp = await this.fetchFn(this.url(path), init);
    if (!resp.ok && resp.status !== 204) {
      if (resp.status === 401) throw new AuthRevokedError();
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API ${opts.method} ${path}: HTTP ${resp.status}: ${text}`);
    }
    return resp;
  }

  private url(path: string): string {
    return this.baseURL + path.replace(":project", encodeURIComponent(this.project));
  }
}

export function isTerminalRunStatus(status: LoopRunStatus | string): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

function anySignal(...signals: AbortSignal[]): AbortSignal {
  const controller = new AbortController();
  for (const signal of signals) {
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

function withQuery(path: string, params: object): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value == null || value === "") continue;
    if (Array.isArray(value)) {
      for (const item of value) query.append(key, String(item));
    } else {
      query.set(key, String(value));
    }
  }
  const qs = query.toString();
  return qs ? `${path}?${qs}` : path;
}

function removeUndefined<T extends object>(obj: T): T {
  return Object.fromEntries(
    Object.entries(obj).filter(([, v]) => v !== undefined),
  ) as T;
}

async function delay(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) return;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    if (signal) {
      signal.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          reject(signal.reason ?? new Error("aborted"));
        },
        { once: true },
      );
    }
  });
}

interface SSEEvent {
  data: string;
}

async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      for (;;) {
        const match = /\r?\n\r?\n/.exec(buffer);
        if (!match) break;
        const raw = buffer.slice(0, match.index);
        buffer = buffer.slice(match.index + match[0].length);
        const data = raw
          .split(/\r?\n/)
          .filter((line) => line.startsWith("data:"))
          .map((line) => line.slice(5).trimStart())
          .join("\n");
        yield { data };
      }
    }
  } finally {
    reader.releaseLock();
  }
}
