import type {
  ConfigEntries,
  CreateWorkflowRequest,
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
  RunSignal,
  SendRunSignalRequest,
  StartBoundRunRequest,
  TagMap,
  UpdateWorkflowRequest,
  WorkflowDefinitionListResponse,
  WorkflowDefinitionSummary,
  WorkflowRun,
  WorkflowRunListResponse,
  WorkflowRunStatus,
  WorkflowSpec,
  components,
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
  /**
   * Number of retries for 429/503 responses. Defaults to
   * {@link DEFAULT_MAX_RETRIES}. Set to 0 to disable retries — 429
   * responses then surface as {@link RateLimitError} on the first attempt.
   * See `../../docs/retries.md` for the shared retry policy.
   */
  retry?: number;
}

export const DEFAULT_BASE_URL = "https://api.mobiusops.ai";
export const DEFAULT_PROJECT = "default";
export const DEFAULT_NAMESPACE = DEFAULT_PROJECT;

export class LeaseLostError extends Error {
  constructor(public readonly jobId: string) {
    super(`lease lost for job ${jobId}`);
    this.name = "LeaseLostError";
  }
}

/**
 * Thrown when the server returns HTTP 401 on a worker-loop request.
 * The credential has been revoked mid-execution; the process needs
 * to restart under a fresh credential. Distinct from
 * {@link LeaseLostError} (409 — lease reclaimed by scheduler) because
 * the remedy is operational, not workflow-level.
 */
export class AuthRevokedError extends Error {
  constructor(public readonly jobId?: string) {
    super(
      jobId
        ? `mobius: credential revoked (job ${jobId})`
        : "mobius: credential revoked",
    );
    this.name = "AuthRevokedError";
  }
}

/**
 * Thrown when the server returns HTTP 409 with `worker_instance_conflict`
 * on a claim. Another live process has already registered this
 * `worker_instance_id` in the project under a different session token.
 * Surfaces from {@link Worker.run} as a hard error so the operator
 * notices the misconfiguration instead of the worker silently retrying —
 * fix by configuring a unique instance ID per process or by relying on
 * the SDK's auto-detection.
 */
export class WorkerInstanceConflictError extends Error {
  constructor(
    public readonly workerInstanceId: string | undefined,
    public readonly projectHandle: string,
    message?: string,
  ) {
    super(
      message ??
        (workerInstanceId
          ? `mobius: worker_instance_id ${JSON.stringify(workerInstanceId)} is already registered in project ${JSON.stringify(projectHandle)} by another live process; configure a unique instance ID per process or rely on auto-detection`
          : "mobius: worker instance conflict"),
    );
    this.name = "WorkerInstanceConflictError";
  }
}

/**
 * Thrown from {@link Client} construction when the API key or project
 * options are malformed (e.g. a project-pinned key whose handle prefix
 * doesn't match the server's handle regex, or a handle conflict
 * between `WithProjectHandle` and the handle embedded in the key).
 */
export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

// Mirrors the server-side handle regex (domain/validate.go) so the
// extracted prefix is rejected here rather than as a 403 on first
// request.
const HANDLE_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;

function extractHandleFromApiKey(apiKey: string): string | null {
  const slash = apiKey.indexOf("/");
  if (slash < 0) return null;
  const handle = apiKey.slice(0, slash);
  if (!HANDLE_RE.test(handle)) {
    throw new ConfigError(
      `invalid project handle prefix in API key: ${JSON.stringify(handle)}`,
    );
  }
  return handle;
}

export class PayloadTooLargeError extends Error {
  constructor(public readonly jobId: string) {
    super(`custom event payload too large for job ${jobId}`);
    this.name = "PayloadTooLargeError";
  }
}

/**
 * Legacy per-job rate-limit error raised by {@link Client.emitJobEvents}.
 * Subclass of {@link RateLimitError} so callers catching the newer,
 * transport-raised {@link RateLimitError} also catch this. New code
 * should prefer {@link RateLimitError}.
 */
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

export interface JobEventEntry {
  type: string;
  payload: Record<string, unknown>;
}

export interface JobEventsRequest {
  worker_instance_id?: string;
  worker_session_token?: string;
  attempt: number;
  events: JobEventEntry[];
}

export type WorkflowRunDetail = components["schemas"]["WorkflowRunDetail"];
export type WorkflowDefinition = components["schemas"]["WorkflowDefinition"];

export interface StartRunOptions {
  queue?: string;
  inputs?: Record<string, unknown>;
  metadata?: Record<string, string>;
  tags?: TagMap;
  external_id?: string;
  config?: ConfigEntries;
}

export interface ListRunsOptions {
  status?: WorkflowRunStatus;
  workflow_type?: string;
  queue?: string;
  parent_run_id?: string;
  initiated_by?: string;
  external_id?: string;
  cursor?: string;
  limit?: number;
}

export interface RunEvent {
  type: string;
  run_id: string;
  seq: number;
  timestamp: string;
  data: Record<string, unknown>;
}

export interface WatchRunOptions {
  since?: number;
  signal?: AbortSignal;
}

export interface WaitRunOptions {
  since?: number;
  signal?: AbortSignal;
  reconnectDelayMs?: number;
}

export interface WorkflowOptions {
  /** Defaults to spec.name when omitted. */
  name?: string;
  /** Stable URL-safe workflow identifier. Server-derived from name when omitted on create. */
  handle?: string;
  description?: string;
  published_as_tool?: boolean;
  tags?: TagMap;
}

export interface UpdateWorkflowOptions {
  name?: string;
  description?: string;
  published_as_tool?: boolean;
  spec?: WorkflowSpec;
  tags?: TagMap;
}

export interface ListWorkflowsOptions {
  cursor?: string;
  limit?: number;
  tag?: string[];
}

export interface WorkflowSyncResult {
  definition: WorkflowDefinition;
  created: boolean;
  updated: boolean;
}

export interface WorkflowDefinitionConfig {
  spec: WorkflowSpec;
  options?: WorkflowOptions;
}

/**
 * Low-level Mobius runtime API client. Prefer {@link Worker} in worker.ts
 * rather than calling these methods directly.
 *
 * Request and response shapes mirror the OpenAPI spec exactly (snake_case).
 * A worker claims individual *jobs* — one action invocation on behalf of
 * a workflow run — and reports each job's result back via this client.
 */
export class Client {
  private readonly baseURL: string;
  readonly project: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof globalThis.fetch;

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    const explicitProject = opts.project ?? opts.namespace;
    // Project-pinned keys arrive as "<handle>/mbx_<secret>". Split the
    // handle off and use it as the project; any explicit project option
    // must either match or be the default sentinel.
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

  /**
   * Long-poll for the next claimable job. Returns null when the poll
   * window closes without a job being available.
   */
  async claimJob(
    req: JobClaimRequest,
    signal?: AbortSignal,
  ): Promise<JobClaim | null> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/jobs/claim`,
      { method: "POST", body: req, signal },
    );
    if (resp.status === 204) return null;
    if (resp.status === 401) throw new AuthRevokedError();
    if (resp.status === 409) {
      const text = await resp.text().catch(() => "");
      // The backend wraps errors as {"error":{"code","message"}}.
      let body: { error?: { code?: string; message?: string } } = {};
      try {
        body = text ? (JSON.parse(text) as typeof body) : {};
      } catch {
        body = {};
      }
      if (body.error?.code === "worker_instance_conflict") {
        throw new WorkerInstanceConflictError(
          req.worker_instance_id,
          this.project,
          body.error.message,
        );
      }
      throw new Error(
        `mobius API POST /jobs/claim: HTTP 409: ${text || "(no body)"}`,
      );
    }
    return (await resp.json()) as JobClaim;
  }

  /** Refresh the lease on a claimed job. */
  async heartbeatJob(
    jobId: string,
    req: JobFenceRequest,
  ): Promise<JobHeartbeat> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/heartbeat`,
      { method: "POST", body: req },
    );
    if (resp.status === 401) throw new AuthRevokedError(jobId);
    if (resp.status === 409) throw new LeaseLostError(jobId);
    return (await resp.json()) as JobHeartbeat;
  }

  /** Report the terminal status of a claimed job. */
  async completeJob(jobId: string, req: JobCompleteRequest): Promise<void> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/complete`,
      { method: "POST", body: req },
    );
    if (resp.status === 401) throw new AuthRevokedError(jobId);
    if (resp.status === 409) throw new LeaseLostError(jobId);
  }

  async emitJobEvents(jobId: string, req: JobEventsRequest): Promise<void> {
    // 429 responses surface as RateLimitError (thrown from the retry
    // transport below). 401/409/413 are non-retryable and handled here.
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/events`,
      { method: "POST", body: req },
    );
    if (resp.status === 401) throw new AuthRevokedError(jobId);
    if (resp.status === 409) throw new LeaseLostError(jobId);
    if (resp.status === 413) throw new PayloadTooLargeError(jobId);
  }

  async emitJobEvent(
    jobId: string,
    req: {
      worker_instance_id?: string;
      worker_session_token?: string;
      attempt: number;
      type: string;
      payload: Record<string, unknown>;
    },
  ): Promise<void> {
    await this.emitJobEvents(jobId, {
      worker_instance_id: req.worker_instance_id,
      worker_session_token: req.worker_session_token,
      attempt: req.attempt,
      events: [{ type: req.type, payload: req.payload }],
    });
  }

  async startRun(
    spec: WorkflowSpec,
    opts: StartRunOptions = {},
  ): Promise<WorkflowRun> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/runs`,
      { method: "POST", body: { mode: "inline", spec, ...opts } },
    );
    return (await resp.json()) as WorkflowRun;
  }

  async startWorkflowRun(
    workflowId: string,
    opts: StartRunOptions = {},
  ): Promise<WorkflowRun> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/workflows/${encodeURIComponent(workflowId)}/runs`,
      { method: "POST", body: opts satisfies StartBoundRunRequest },
    );
    return (await resp.json()) as WorkflowRun;
  }

  async listRuns(opts: ListRunsOptions = {}): Promise<WorkflowRunListResponse> {
    const path = withQuery(
      `/v1/projects/${encodeURIComponent(this.project)}/runs`,
      opts,
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as WorkflowRunListResponse;
  }

  async getRun(runId: string): Promise<WorkflowRunDetail> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/runs/${encodeURIComponent(runId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as WorkflowRunDetail;
  }

  async cancelRun(runId: string): Promise<void> {
    await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/runs/${encodeURIComponent(runId)}/cancellations`,
      { method: "POST" },
    );
  }

  async resumeRun(runId: string): Promise<void> {
    await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/runs/${encodeURIComponent(runId)}/resumptions`,
      { method: "POST" },
    );
  }

  async sendRunSignal(
    runId: string,
    req: SendRunSignalRequest,
  ): Promise<RunSignal> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/runs/${encodeURIComponent(runId)}/signals`,
      { method: "POST", body: req },
    );
    return (await resp.json()) as RunSignal;
  }

  async *watchRun(
    runId: string,
    opts: WatchRunOptions = {},
  ): AsyncGenerator<RunEvent> {
    const path = withQuery(
      `/v1/projects/${encodeURIComponent(this.project)}/runs/${encodeURIComponent(runId)}/events`,
      opts.since && opts.since > 0 ? { since: opts.since } : {},
    );
    const resp = await this.fetchFn(this.baseURL + path, {
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
      yield JSON.parse(evt.data) as RunEvent;
    }
  }

  async waitRun(
    runId: string,
    opts: WaitRunOptions = {},
  ): Promise<WorkflowRunDetail> {
    let since = opts.since ?? 0;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    for (;;) {
      const run = await this.getRun(runId);
      if (isTerminalRunStatus(run.status)) return run;

      try {
        for await (const ev of this.watchRun(runId, {
          since,
          signal: opts.signal,
        })) {
          if (ev.seq > since) since = ev.seq;
          if (ev.type !== "run_updated") continue;
          const status = ev.data.status;
          if (status === "completed" || status === "failed") {
            return await this.getRun(runId);
          }
        }
      } catch (err) {
        if (opts.signal?.aborted) throw err;
      }
      await delay(reconnectDelayMs, opts.signal);
    }
  }

  async listWorkflows(
    opts: ListWorkflowsOptions = {},
  ): Promise<WorkflowDefinitionListResponse> {
    const path = withQuery(
      `/v1/projects/${encodeURIComponent(this.project)}/workflows`,
      opts,
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as WorkflowDefinitionListResponse;
  }

  async getWorkflow(workflowId: string): Promise<WorkflowDefinition> {
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/workflows/${encodeURIComponent(workflowId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as WorkflowDefinition;
  }

  async createWorkflow(
    spec: WorkflowSpec,
    opts: WorkflowOptions = {},
  ): Promise<WorkflowDefinition> {
    const body: CreateWorkflowRequest = createWorkflowRequest(spec, opts);
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/workflows`,
      { method: "POST", body },
    );
    return (await resp.json()) as WorkflowDefinition;
  }

  async updateWorkflow(
    workflowId: string,
    opts: UpdateWorkflowOptions = {},
  ): Promise<WorkflowDefinition> {
    const body: UpdateWorkflowRequest = removeUndefined({
      name: opts.name,
      description: opts.description,
      published_as_tool: opts.published_as_tool,
      spec: opts.spec,
      tags: opts.tags,
    });
    const resp = await this.request(
      `/v1/projects/${encodeURIComponent(this.project)}/workflows/${encodeURIComponent(workflowId)}`,
      { method: "PATCH", body },
    );
    return (await resp.json()) as WorkflowDefinition;
  }

  async ensureWorkflow(
    spec: WorkflowSpec,
    opts: WorkflowOptions = {},
  ): Promise<WorkflowSyncResult> {
    const desired = normalizeWorkflowOptions(spec, opts);
    const existing = await this.findWorkflow(desired);
    if (!existing) {
      return {
        definition: await this.createWorkflow(spec, desired),
        created: true,
        updated: false,
      };
    }

    const current = await this.getWorkflow(existing.id);
    const update = workflowUpdateForDiff(current, spec, desired);
    if (!update) {
      return { definition: current, created: false, updated: false };
    }
    return {
      definition: await this.updateWorkflow(current.id, update),
      created: false,
      updated: true,
    };
  }

  async syncWorkflows(
    defs: WorkflowDefinitionConfig[],
  ): Promise<WorkflowSyncResult[]> {
    const results: WorkflowSyncResult[] = [];
    for (const def of defs) {
      results.push(await this.ensureWorkflow(def.spec, def.options ?? {}));
    }
    return results;
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
    if (opts.body != null) {
      init.body = JSON.stringify(opts.body);
    }
    const resp = await this.fetchFn(this.baseURL + path, init);
    if (
      !resp.ok &&
      resp.status !== 204 &&
      resp.status !== 401 &&
      resp.status !== 409 &&
      resp.status !== 413 &&
      resp.status !== 429
    ) {
      const text = await resp.text().catch(() => "");
      throw new Error(
        `mobius API ${opts.method} ${path}: HTTP ${resp.status}: ${text}`,
      );
    }
    return resp;
  }

  private async findWorkflow(
    desired: Required<Pick<WorkflowOptions, "name">> & WorkflowOptions,
  ): Promise<WorkflowDefinitionSummary | null> {
    if (!desired.handle && !desired.name) {
      throw new Error("mobius: ensure workflow requires a handle, name, or spec name");
    }
    let cursor = "";
    for (;;) {
      const page = await this.listWorkflows({ cursor, limit: 100 });
      for (const item of page.items) {
        if (desired.handle && item.handle === desired.handle) return item;
        if (!desired.handle && item.name === desired.name) return item;
      }
      if (!page.has_more || !page.next_cursor) return null;
      cursor = page.next_cursor;
    }
  }
}

export type { JobClaim, JobHeartbeat };

export type {
  RunSignal,
  SendRunSignalRequest,
  WorkflowDefinitionListResponse,
  WorkflowDefinitionSummary,
  WorkflowRun,
  WorkflowRunListResponse,
  WorkflowRunStatus,
  WorkflowSpec,
};

function anySignal(...signals: AbortSignal[]): AbortSignal {
  const controller = new AbortController();
  for (const s of signals) {
    if (s.aborted) {
      controller.abort(s.reason);
      break;
    }
    s.addEventListener("abort", () => controller.abort(s.reason), {
      once: true,
    });
  }
  return controller.signal;
}

export function isTerminalRunStatus(status: WorkflowRunStatus): boolean {
  return status === "completed" || status === "failed";
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

function createWorkflowRequest(
  spec: WorkflowSpec,
  opts: WorkflowOptions,
): CreateWorkflowRequest {
  const normalized = normalizeWorkflowOptions(spec, opts);
  return removeUndefined({
    name: normalized.name,
    handle: normalized.handle,
    description: normalized.description,
    published_as_tool: normalized.published_as_tool,
    spec,
    tags: normalized.tags,
  });
}

function normalizeWorkflowOptions(
  spec: WorkflowSpec,
  opts: WorkflowOptions,
): Required<Pick<WorkflowOptions, "name">> & WorkflowOptions {
  return { ...opts, name: opts.name || spec.name };
}

function workflowUpdateForDiff(
  current: WorkflowDefinition,
  spec: WorkflowSpec,
  desired: Required<Pick<WorkflowOptions, "name">> & WorkflowOptions,
): UpdateWorkflowOptions | null {
  const update: UpdateWorkflowOptions = {};
  if (desired.name && current.name !== desired.name) update.name = desired.name;
  if (desired.description && current.description !== desired.description) {
    update.description = desired.description;
  }
  if (
    desired.published_as_tool !== undefined &&
    current.published_as_tool !== desired.published_as_tool
  ) {
    update.published_as_tool = desired.published_as_tool;
  }
  if (desired.tags !== undefined && !jsonEqual(current.tags, desired.tags)) {
    update.tags = desired.tags;
  }
  if (!jsonEqual(current.spec, spec)) update.spec = spec;
  return Object.keys(update).length > 0 ? update : null;
}

function jsonEqual(a: unknown, b: unknown): boolean {
  return stableJsonStringify(a) === stableJsonStringify(b);
}

function stableJsonStringify(value: unknown): string | undefined {
  if (value === null || typeof value !== "object") {
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map((item) => stableJsonStringify(item) ?? "null").join(",")}]`;
  }
  const obj = value as Record<string, unknown>;
  const entries = Object.keys(obj)
    .sort()
    .filter((key) => obj[key] !== undefined)
    .map((key) => `${JSON.stringify(key)}:${stableJsonStringify(obj[key])}`);
  return `{${entries.join(",")}}`;
}

function removeUndefined<T extends Record<string, unknown>>(obj: T): T {
  return Object.fromEntries(
    Object.entries(obj).filter(([, value]) => value !== undefined),
  ) as T;
}

interface ParsedSSEEvent {
  event?: string;
  id?: string;
  data: string;
}

async function* parseSSE(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<ParsedSSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let event = "";
  let id = "";
  let dataLines: string[] = [];

  const dispatch = (): ParsedSSEEvent | null => {
    if (dataLines.length === 0) {
      event = "";
      return null;
    }
    const out: ParsedSSEEvent = {
      event: event || "message",
      id,
      data: dataLines.join("\n"),
    };
    event = "";
    dataLines = [];
    return out;
  };

  const processLine = (rawLine: string): ParsedSSEEvent | null => {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (line === "") return dispatch();
    if (line.startsWith(":")) return null;
    const colon = line.indexOf(":");
    const field = colon >= 0 ? line.slice(0, colon) : line;
    let value = colon >= 0 ? line.slice(colon + 1) : "";
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    if (field === "id" && !value.includes("\0")) id = value;
    if (field === "data") dataLines.push(value);
    return null;
  };

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    for (;;) {
      const newline = buffer.indexOf("\n");
      if (newline < 0) break;
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      const evt = processLine(line);
      if (evt) yield evt;
    }
  }
  buffer += decoder.decode();
  if (buffer.length > 0) {
    const evt = processLine(buffer);
    if (evt) yield evt;
  }
  const evt = dispatch();
  if (evt) yield evt;
}

function delay(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(signal.reason);
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timeout);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}
