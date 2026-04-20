import type {
  JobClaim,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
} from "./api/index.js";

export interface ClientOptions {
  apiKey: string;
  baseURL?: string;
  /** Project handle used for all project-scoped operations. */
  project?: string;
  /** Compatibility alias for older callers. */
  namespace?: string;
  /** Fetch timeout in milliseconds. Defaults to 60_000. */
  timeoutMs?: number;
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

export class PayloadTooLargeError extends Error {
  constructor(public readonly jobId: string) {
    super(`custom event payload too large for job ${jobId}`);
    this.name = "PayloadTooLargeError";
  }
}

export class RateLimitedError extends Error {
  constructor(
    public readonly jobId: string,
    public readonly retryAfter?: number,
  ) {
    super(
      retryAfter != null
        ? `custom event rate limited for job ${jobId} (retry after ${retryAfter}s)`
        : `custom event rate limited for job ${jobId}`,
    );
    this.name = "RateLimitedError";
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

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    this.project = opts.project ?? opts.namespace ?? DEFAULT_PROJECT;
    this.headers = {
      Authorization: `Bearer ${opts.apiKey}`,
      "Content-Type": "application/json",
    };
    this.timeoutMs = opts.timeoutMs ?? 60_000;
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
      `/projects/${encodeURIComponent(this.project)}/jobs/claim`,
      { method: "POST", body: req, signal },
    );
    if (resp.status === 204) return null;
    return (await resp.json()) as JobClaim;
  }

  /** Refresh the lease on a claimed job. */
  async heartbeatJob(
    jobId: string,
    req: JobFenceRequest,
  ): Promise<JobHeartbeat> {
    const resp = await this.request(
      `/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/heartbeat`,
      { method: "POST", body: req },
    );
    if (resp.status === 409) throw new LeaseLostError(jobId);
    return (await resp.json()) as JobHeartbeat;
  }

  /** Report the terminal status of a claimed job. */
  async completeJob(jobId: string, req: JobCompleteRequest): Promise<void> {
    const resp = await this.request(
      `/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/complete`,
      { method: "POST", body: req },
    );
    if (resp.status === 409) throw new LeaseLostError(jobId);
  }

  async emitJobEvents(jobId: string, req: JobEventsRequest): Promise<void> {
    const resp = await this.request(
      `/projects/${encodeURIComponent(this.project)}/jobs/${encodeURIComponent(jobId)}/events`,
      { method: "POST", body: req },
    );
    if (resp.status === 409) throw new LeaseLostError(jobId);
    if (resp.status === 413) throw new PayloadTooLargeError(jobId);
    if (resp.status === 429) {
      const retryAfterHeader = resp.headers.get("Retry-After");
      const retryAfter = retryAfterHeader
        ? Number.parseInt(retryAfterHeader, 10)
        : undefined;
      throw new RateLimitedError(
        jobId,
        Number.isFinite(retryAfter) ? retryAfter : undefined,
      );
    }
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
    const resp = await fetch(this.baseURL + path, init);
    if (
      !resp.ok &&
      resp.status !== 204 &&
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
