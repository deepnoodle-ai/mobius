// 429/503-aware retrying `fetch` wrapper. Implements the shared retry
// policy documented in ../../docs/retries.md.

export class RateLimitError extends Error {
  readonly retryAfter: number;
  readonly limit: number | null;
  readonly remaining: number | null;
  readonly resetAt: Date | null;
  readonly scope: string | null;
  readonly policy: string | null;

  constructor(init: {
    retryAfter?: number;
    limit?: number | null;
    remaining?: number | null;
    resetAt?: Date | null;
    scope?: string | null;
    policy?: string | null;
    message?: string;
  } = {}) {
    const scope = init.scope ?? null;
    const scopeLabel = scope ?? "unknown";
    const retryAfter = init.retryAfter ?? 0;
    const message =
      init.message ??
      (retryAfter > 0
        ? `mobius: rate limit exceeded (scope=${scopeLabel}, retry after ${retryAfter}s)`
        : `mobius: rate limit exceeded (scope=${scopeLabel})`);
    super(message);
    this.name = "RateLimitError";
    this.retryAfter = retryAfter;
    this.limit = init.limit ?? null;
    this.remaining = init.remaining ?? null;
    this.resetAt = init.resetAt ?? null;
    this.scope = scope;
    this.policy = init.policy ?? null;
  }
}

export const DEFAULT_MAX_RETRIES = 3;
export const MAX_RETRY_BACKOFF_SECONDS = 60;
const BASE_RETRY_BACKOFF_SECONDS = 1;

const IDEMPOTENT_METHODS = new Set(["GET", "HEAD", "PUT", "DELETE", "OPTIONS"]);

type FetchFn = typeof globalThis.fetch;

export interface WrapRetryOptions {
  /** Number of retries for 429/503 responses. 0 disables retries. */
  maxRetries?: number;
  /** Override for tests — called in place of `setTimeout`-based sleeping. */
  sleep?: (seconds: number) => Promise<void>;
  /** Override for tests — called in place of `Date.now()` (milliseconds). */
  now?: () => number;
}

/**
 * Wrap a `fetch` function so that 429 and 503 responses are retried per
 * the shared policy. The returned function is a drop-in replacement for
 * the global `fetch`.
 */
export function wrapFetchWithRetry(
  fetchFn: FetchFn,
  options: WrapRetryOptions = {},
): FetchFn {
  const maxRetries = Math.max(0, options.maxRetries ?? DEFAULT_MAX_RETRIES);
  const sleep = options.sleep ?? defaultSleep;
  const now = options.now ?? (() => Date.now());

  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    let attempt = 0;
    // Capture method / idempotency bits once — retries re-send the same request.
    const { method, hasIdempotencyKey } = describeRequest(input, init);
    const idempotent = isIdempotent(method, hasIdempotencyKey);

    while (true) {
      const response = await fetchFn(input, init);
      const status = response.status;
      if (status !== 429 && status !== 503) {
        return response;
      }

      const outOfBudget = attempt >= maxRetries || !idempotent;
      if (status === 429 && outOfBudget) {
        // Drain body so the connection can be reused.
        await drainBody(response);
        throw buildRateLimitError(response);
      }
      if (status === 503 && outOfBudget) {
        return response;
      }

      const wait = retryAfterOrBackoff(response, attempt, now);
      await drainBody(response);
      if (wait > 0) {
        await sleep(wait);
      }
      attempt++;
    }
  }) as FetchFn;
}

function describeRequest(
  input: RequestInfo | URL,
  init?: RequestInit,
): { method: string; hasIdempotencyKey: boolean } {
  let method = (init?.method ?? "GET").toUpperCase();
  let headers: Headers | undefined;
  if (init?.headers) {
    headers = new Headers(init.headers);
  }
  if (input instanceof Request) {
    if (!init?.method) method = input.method.toUpperCase();
    if (!headers) headers = new Headers(input.headers);
  }
  const hasIdempotencyKey = headers
    ? (headers.get("Idempotency-Key") ?? "").trim() !== ""
    : false;
  return { method, hasIdempotencyKey };
}

function isIdempotent(method: string, hasIdempotencyKey: boolean): boolean {
  if (IDEMPOTENT_METHODS.has(method)) return true;
  if (method === "POST" || method === "PATCH") return hasIdempotencyKey;
  return false;
}

async function drainBody(response: Response): Promise<void> {
  try {
    await response.text();
  } catch {
    // ignore
  }
}

function retryAfterOrBackoff(
  response: Response,
  attempt: number,
  now: () => number,
): number {
  const header = response.headers.get("Retry-After");
  const parsed = parseRetryAfter(header, now);
  if (parsed !== null) return clamp(parsed);
  return clamp(BASE_RETRY_BACKOFF_SECONDS * 2 ** attempt);
}

function parseRetryAfter(value: string | null, now: () => number): number | null {
  if (value == null || value.trim() === "") return null;
  const trimmed = value.trim();
  if (/^-?\d+$/.test(trimmed)) {
    return Number(trimmed);
  }
  const ts = Date.parse(trimmed);
  if (Number.isNaN(ts)) return null;
  const delta = (ts - now()) / 1000;
  return Math.max(0, delta);
}

function clamp(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds < 0) return 0;
  if (seconds > MAX_RETRY_BACKOFF_SECONDS) return MAX_RETRY_BACKOFF_SECONDS;
  return seconds;
}

function buildRateLimitError(response: Response): RateLimitError {
  const headers = response.headers;
  const retryAfter =
    parseRetryAfter(headers.get("Retry-After"), () => Date.now()) ?? 0;
  return new RateLimitError({
    retryAfter,
    limit: intOrNull(headers.get("X-RateLimit-Limit")),
    remaining: intOrNull(headers.get("X-RateLimit-Remaining")),
    resetAt: unixOrNull(headers.get("X-RateLimit-Reset")),
    scope: headers.get("X-RateLimit-Scope"),
    policy: headers.get("X-RateLimit-Policy"),
  });
}

function intOrNull(v: string | null): number | null {
  if (v == null || v === "") return null;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? n : null;
}

function unixOrNull(v: string | null): Date | null {
  if (v == null || v === "") return null;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? new Date(n * 1000) : null;
}

function defaultSleep(seconds: number): Promise<void> {
  return new Promise<void>((resolve) => setTimeout(resolve, seconds * 1000));
}
