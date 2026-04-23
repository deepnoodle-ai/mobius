import type {
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
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
  worker_id: string;
  attempt: number;
  events: JobEventEntry[];
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
      worker_id: string;
      attempt: number;
      type: string;
      payload: Record<string, unknown>;
    },
  ): Promise<void> {
    await this.emitJobEvents(jobId, {
      worker_id: req.worker_id,
      attempt: req.attempt,
      events: [{ type: req.type, payload: req.payload }],
    });
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
}

export type { JobClaim, JobHeartbeat };

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
