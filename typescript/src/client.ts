import type {
  JobClaim,
  JobClaimDataResponse,
  JobClaimRequest,
  JobCompleteRequest,
  JobFenceRequest,
  JobHeartbeat,
  JobHeartbeatDataResponse,
} from "./api/index.js";

export interface ClientOptions {
  apiKey: string;
  baseURL?: string;
  /** Namespace slug used for all namespace-scoped operations. */
  namespace?: string;
  /** Fetch timeout in milliseconds. Defaults to 60_000. */
  timeoutMs?: number;
}

export const DEFAULT_BASE_URL = "https://api.mobiusops.ai";
export const DEFAULT_NAMESPACE = "default";

export class LeaseLostError extends Error {
  constructor(public readonly jobId: string) {
    super(`lease lost for job ${jobId}`);
    this.name = "LeaseLostError";
  }
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
  private readonly namespace: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    this.namespace = opts.namespace ?? DEFAULT_NAMESPACE;
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
  async claimTask(
    req: JobClaimRequest,
    signal?: AbortSignal,
  ): Promise<JobClaim | null> {
    const resp = await this.request(
      `/namespaces/${encodeURIComponent(this.namespace)}/jobs/claim`,
      { method: "POST", body: { data: req }, signal },
    );
    if (resp.status === 204) return null;
    const body = (await resp.json()) as JobClaimDataResponse;
    return body.data;
  }

  /** Refresh the lease on a claimed job. */
  async heartbeatTask(
    jobId: string,
    req: JobFenceRequest,
  ): Promise<JobHeartbeat> {
    const resp = await this.request(
      `/namespaces/${encodeURIComponent(this.namespace)}/jobs/${encodeURIComponent(jobId)}/heartbeat`,
      { method: "POST", body: { data: req } },
    );
    if (resp.status === 409) throw new LeaseLostError(jobId);
    const body = (await resp.json()) as JobHeartbeatDataResponse;
    return body.data;
  }

  /** Report the terminal status of a claimed job. */
  async completeTask(jobId: string, req: JobCompleteRequest): Promise<void> {
    const resp = await this.request(
      `/namespaces/${encodeURIComponent(this.namespace)}/jobs/${encodeURIComponent(jobId)}/complete`,
      { method: "POST", body: { data: req } },
    );
    if (resp.status === 409) throw new LeaseLostError(jobId);
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
    if (!resp.ok && resp.status !== 204 && resp.status !== 409) {
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API ${opts.method} ${path}: HTTP ${resp.status}: ${text}`);
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
    s.addEventListener("abort", () => controller.abort(s.reason), { once: true });
  }
  return controller.signal;
}
