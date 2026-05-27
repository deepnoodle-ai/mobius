import type {
  Automation,
  AutomationListResponse,
  AutomationRun,
  AutomationRunEvent,
  AutomationRunListResponse,
  AutomationRunSource,
  AutomationRunStatus,
  AutomationStatus,
  AutomationTriggerInput,
  AutomationVersion,
  AutomationVersionListResponse,
  CancelAutomationRunRequest,
  CreateAutomationRequest,
  CreateAutomationVersionRequest,
  SignalAutomationRunRequest,
  StartAutomationRunRequest,
  TagMap,
  UpdateAutomationRequest,
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

export interface AutomationOptions {
  name: string;
  handle: string;
  description?: string;
  default_agent_id?: string;
  default_inputs?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  tags?: TagMap;
  triggers?: AutomationTriggerInput[];
}

export interface UpdateAutomationOptions {
  name?: string;
  description?: string;
  default_agent_id?: string;
  default_inputs?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  status?: AutomationStatus;
  tags?: TagMap;
  triggers?: AutomationTriggerInput[];
}

export interface AutomationVersionOptions {
  compiled_plan?: Record<string, unknown>;
  publish?: boolean;
}

export interface ListAutomationsOptions {
  status?: AutomationStatus;
  cursor?: string;
  limit?: number;
}

export interface StartRunOptions {
  inputs?: Record<string, unknown>;
  source?: AutomationRunSource;
  external_id?: string;
}

export interface ListRunsOptions {
  status?: AutomationRunStatus;
  automation_id?: string;
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

export type RunEvent = AutomationRunEvent;

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

  async listAutomations(
    opts: ListAutomationsOptions = {},
  ): Promise<AutomationListResponse> {
    const resp = await this.request(withQuery("/v1/projects/:project/automations", opts), {
      method: "GET",
    });
    return (await resp.json()) as AutomationListResponse;
  }

  async getAutomation(handle: string): Promise<Automation> {
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Automation;
  }

  async createAutomation(opts: AutomationOptions): Promise<Automation> {
    const body: CreateAutomationRequest = removeUndefined({
      name: opts.name,
      handle: opts.handle,
      description: opts.description,
      default_agent_id: opts.default_agent_id,
      default_inputs: opts.default_inputs,
      settings: opts.settings,
      tags: opts.tags,
      triggers: opts.triggers,
    });
    const resp = await this.request("/v1/projects/:project/automations", {
      method: "POST",
      body,
    });
    return (await resp.json()) as Automation;
  }

  async updateAutomation(
    handle: string,
    opts: UpdateAutomationOptions,
  ): Promise<Automation> {
    const body: UpdateAutomationRequest = removeUndefined(opts);
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}`,
      { method: "PATCH", body },
    );
    return (await resp.json()) as Automation;
  }

  async deleteAutomation(handle: string): Promise<void> {
    await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}`,
      { method: "DELETE" },
    );
  }

  async listAutomationVersions(
    handle: string,
  ): Promise<AutomationVersionListResponse> {
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}/versions`,
      { method: "GET" },
    );
    return (await resp.json()) as AutomationVersionListResponse;
  }

  async createAutomationVersion(
    handle: string,
    spec: Record<string, unknown>,
    opts: AutomationVersionOptions = {},
  ): Promise<AutomationVersion> {
    const body: CreateAutomationVersionRequest = removeUndefined({
      spec,
      compiled_plan: opts.compiled_plan,
    });
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}/versions`,
      { method: "POST", body },
    );
    const version = (await resp.json()) as AutomationVersion;
    if (opts.publish) {
      await this.publishAutomationVersion(handle, version.version);
    }
    return version;
  }

  async publishAutomationVersion(
    handle: string,
    version: number,
  ): Promise<Automation> {
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(handle)}/versions/${version}/publication`,
      { method: "POST" },
    );
    return (await resp.json()) as Automation;
  }

  async startRun(
    automationHandle: string,
    opts: StartRunOptions = {},
  ): Promise<AutomationRun> {
    return this.startAutomationRun(automationHandle, opts);
  }

  async startAutomationRun(
    automationHandle: string,
    opts: StartRunOptions = {},
  ): Promise<AutomationRun> {
    const body: StartAutomationRunRequest = removeUndefined({
      inputs: opts.inputs,
      source: opts.source,
      external_id: opts.external_id,
    });
    const resp = await this.request(
      `/v1/projects/:project/automations/${encodeURIComponent(automationHandle)}/runs`,
      { method: "POST", body },
    );
    return (await resp.json()) as AutomationRun;
  }

  async listRuns(opts: ListRunsOptions = {}): Promise<AutomationRunListResponse> {
    const resp = await this.request(withQuery("/v1/projects/:project/runs", opts), {
      method: "GET",
    });
    return (await resp.json()) as AutomationRunListResponse;
  }

  async getRun(runId: string): Promise<AutomationRun> {
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as AutomationRun;
  }

  async cancelRun(runId: string, reason?: string): Promise<AutomationRun> {
    const body: CancelAutomationRunRequest = removeUndefined({ reason });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/cancellations`,
      { method: "POST", body },
    );
    return (await resp.json()) as AutomationRun;
  }

  async signalRun(
    runId: string,
    stepKey: string,
    result?: Record<string, unknown>,
  ): Promise<AutomationRun> {
    const body: SignalAutomationRunRequest = removeUndefined({
      step_key: stepKey,
      result,
    });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/signals`,
      { method: "POST", body },
    );
    return (await resp.json()) as AutomationRun;
  }

  async *watchRun(
    runId: string,
    opts: WatchRunOptions = {},
  ): AsyncGenerator<AutomationRunEvent> {
    const path = withQuery(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/events.stream`,
      opts.since && opts.since > 0 ? { since_sequence: opts.since } : {},
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
      yield JSON.parse(evt.data) as AutomationRunEvent;
    }
  }

  async waitRun(runId: string, opts: WaitRunOptions = {}): Promise<AutomationRun> {
    let since = opts.since ?? 0;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    for (;;) {
      const run = await this.getRun(runId);
      if (isTerminalRunStatus(run.status)) return run;
      try {
        for await (const ev of this.watchRun(runId, opts)) {
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

export type {
  Automation,
  AutomationListResponse,
  AutomationRun,
  AutomationRunEvent,
  AutomationRunListResponse,
  AutomationRunStatus,
  AutomationVersion,
  AutomationVersionListResponse,
};

export function isTerminalRunStatus(status: AutomationRunStatus | string): boolean {
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
      let idx;
      while ((idx = buffer.indexOf("\n\n")) >= 0) {
        const raw = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
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
