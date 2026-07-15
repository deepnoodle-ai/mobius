# SDK Retry & Rate-Limit Handling

The Mobius API may respond to any authenticated request with `429 Too Many
Requests` when a rate-limit bucket is exhausted, or a transient server error
(`500`, `502`, `503`, or `504`). A request may also fail at the transport layer
— connection reset, unexpected EOF, DNS failure, I/O timeout — before any
response is produced. All official SDKs (Go, Python, TypeScript) share the same
retry semantics for these cases so that customer behavior is consistent across
languages.

This document is the authoritative spec. If one language drifts from it,
that language has a bug.

## Scope

The retry layer sits **below** the generated client and wraps every outbound
request. It normally observes only the response status code and a small set of
headers. For a successful replay-safe JSON write, it also fully reads and
syntactically validates the acknowledgement before returning it; it does not
interpret application fields in request or response bodies.

## Retryable failures

A retry is triggered by any of:

| Failure | Meaning |
|---------|---------|
| `429 Too Many Requests` | Rate-limit exceeded for the credential or org. |
| `500 Internal Server Error` | The server failed unexpectedly before it could provide a stable application response. |
| `502 Bad Gateway` | A gateway or proxy received an invalid upstream response. |
| `503 Service Unavailable` | The backend is transiently unavailable or overloaded. |
| `504 Gateway Timeout` | A gateway or proxy timed out waiting for an upstream response. |
| Transport-level error | The request never produced an HTTP response — DNS failure, connection reset, unexpected EOF, or I/O timeout. |
| Replay-safe JSON acknowledgement failure | A successful `POST`/`PATCH` response could not be fully read or was not valid JSON. |

Other statuses, including non-transient `5xx` responses such as `501` and
`505`, are **not** retried. They reach the caller as ordinary responses.

Transport-level errors are retried only for replayable, idempotent requests
(see [idempotency gating](#idempotency-gating)); a non-idempotent request
surfaces the underlying error without a retry. Caller cancellation — a
cancelled context or aborted signal — stops the retry loop immediately and is
never itself treated as a retryable error.

## Idempotency gating

Retrying a non-idempotent write risks creating duplicates. The SDK retries
only when the request is safe to replay:

- `GET`, `HEAD`, `PUT`, `DELETE` — always retried on a retryable failure.
- `POST` — retried **only** when the request carries an `Idempotency-Key`
  header. A `POST` without that header is never retried; on `429` the SDK
  surfaces `RateLimitError` immediately, and on a transport error it surfaces
  the underlying error immediately.
- `PATCH` — treated like `POST`: retried only with `Idempotency-Key`.

## Backoff

For each retry the SDK sleeps a bounded number of seconds, chosen as:

1. If the response carries `Retry-After`, parse it:
   - integer number of seconds, **or**
   - HTTP-date (RFC 7231) — delta = date − now.
2. Otherwise fall back to exponential backoff: `1s`, `2s`, `4s`, ...
   (doubling each attempt).
3. In all cases clamp the per-attempt wait to `[0, 60s]`. A server that asks
   for a 600s wait is ignored beyond the cap; callers that want to honor
   very long waits should disable SDK retries and implement their own
   policy.

Transport-level errors carry no `Retry-After`, so they always use the
exponential-backoff schedule.

Unreadable or invalid successful JSON acknowledgements use the same
exponential-backoff schedule and consume the same retry budget. They do not
start a second high-level retry loop.

The context/`AbortSignal` passed by the caller is respected during sleep —
cancellation aborts the retry loop with the cancellation error.

## Retry budget

- Default: **3 retries** per request (so up to 4 total attempts).
- Configurable by the caller: `0` disables retries, any positive integer is
  permitted.
- When retries are exhausted, or when retrying is not allowed (see
  [idempotency gating](#idempotency-gating)), the SDK raises a typed
  `RateLimitError` populated from the last response's headers.

Retryable `5xx` responses that eventually give up do **not** wrap into
`RateLimitError` — the SDK passes the underlying response through so the
caller's existing status handling applies. Likewise, a transport-level error
that exhausts its budget (or is not allowed to retry) surfaces the underlying
network error unchanged — never wrapped as `RateLimitError`.

## High-level replay-safe writes

The Go, Python, and TypeScript high-level clients mirror a normalized caller
key into both the existing JSON field and the `Idempotency-Key` header. The
JSON field remains the server deduplication contract; the matching header tells
the SDK transport that replay is safe.

| High-level operation | JSON field | When the header is sent |
|---|---|---|
| Start loop run | `idempotency_key` | Nonblank key supplied |
| Invoke agent | `input.idempotency_key` | Nonblank key and session mode is not `new` |
| Invoke agent inline stream | `input.idempotency_key` | Same as invoke agent, before response start only |
| Start turn in an existing session | `idempotency_key` | Nonblank key supplied |
| Nudge session | `idempotency_key` | Nonblank key supplied |

Keys are trimmed once and blank values are omitted, so the body and header
cannot diverge. `invokeAgent` with `session.mode: new` intentionally keeps the
body key but omits the header: each replay would create a fresh session before
the server applies session-scoped turn deduplication.

For inline SSE, the transport may retry a network failure before any response,
or a retryable HTTP response. Once a successful stream response begins, it
returns the response without pre-reading it and never invokes again. Later
disconnects are handled by transcript cursor reconnection, not admission retry.

## `RateLimitError` shape

All three SDKs expose the same fields (naming follows language conventions):

| Field | Type | Source |
|-------|------|--------|
| `retry_after` | duration (or seconds) | `Retry-After` header; `0` if absent |
| `limit` | integer | `X-RateLimit-Limit` header |
| `remaining` | integer | `X-RateLimit-Remaining` header |
| `reset_at` | timestamp | `X-RateLimit-Reset` (Unix seconds) |
| `scope` | string (`"key"` / `"org"`) | `X-RateLimit-Scope` header |

The backend also emits `X-RateLimit-Policy` describing the policy
(`"<limit>;w=<seconds>"`). SDKs surface it on the error for diagnostics but
do not rely on it for control flow.

The error's message is stable:

```
mobius: rate limit exceeded (scope=<scope>, retry after <N>s)
```

### Error wrapping

Each SDK surfaces `RateLimitError` in a way idiomatic for that language:

- **Go** — `*RateLimitError` unwraps to the sentinel `ErrRateLimited`, so
  `errors.Is(err, mobius.ErrRateLimited)` keeps working for existing
  callers. New callers can `errors.As(err, &rle)` for the rich fields.
- **Python** — `RateLimitError` subclasses `Exception`. The pre-existing
  `RateLimitedError` (emitted from custom events) is kept as an alias for
  backward compatibility with the 0.0.x release line.
- **TypeScript** — `RateLimitError` extends `Error` and is thrown from the
  wrapped `fetch`.

## Defaults summary

| Knob | Default |
|------|---------|
| Retry statuses | `{429, 500, 502, 503, 504}` |
| Retry transport errors | yes (idempotent + replayable only) |
| Retry unreadable/invalid successful JSON ack | yes (replay-safe write only) |
| Retries | `3` |
| Retry non-idempotent POST on retryable status / transport error | no (surface error) |
| Max sleep per attempt | `60s` |
| Exp backoff base | `1s`, doubled each attempt |

## Disabling retries

Set the per-client knob to `0`:

- Go: `mobius.NewClient(..., mobius.WithRetry(0))`
- Python: `Client(..., retry=0)`
- TypeScript: `new Client({ ..., retry: 0 })`

With retries disabled, every `429` surfaces as `RateLimitError` immediately;
retryable `5xx` responses pass through unchanged on the first attempt.

## What the server expects

The backend assumes well-behaved clients will sleep per `Retry-After` before
the next request. A client that ignores `Retry-After` and retries
aggressively will just get more 429s — the bucket doesn't refill
mid-window. The SDK retry layer exists precisely so customers don't have to
think about this.

## Worker claim loop

The Go worker (`mobius/worker.go`) runs its own loop above the transport,
so the 60-second transport cap does not apply: when the transport
surfaces a `RateLimitError`, the worker sleeps the server-provided
`RetryAfter` before re-claiming, clamped to `MaxClaimRateLimitSleep`
(5 minutes). On any other claim error it falls back to a 2-second
backoff. The clamp keeps a runaway header from pinning a worker for
hours and bounds the time before context cancellation takes effect;
when the actual window is longer than the clamp, the next claim
returns a fresh `429` with the remaining `Retry-After`, so the worker
sleeps again — bounded polling, not a hot loop.

The Python and TypeScript worker loops should mirror this: honor
`RetryAfter` after a 429 instead of using a uniform error backoff.
