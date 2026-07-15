# SDK admission retry boundary

**Status:** Accepted

**Author:** Codex

**Date:** 2026-07-15

**Workflow:** Spec, then build

## Context

The Go, Python, and TypeScript SDK transports retry `POST` requests only when
they carry `Idempotency-Key`. High-level turn admission methods already send a
server-authoritative idempotency key in JSON, but they do not mirror that key
into the header, so transient failures are not retried even when the server can
deduplicate the request. The same mismatch exists on loop-run admission and
session nudges.

Transport success also currently ends when response headers arrive. A reset or
truncated JSON body after a successful status can therefore leave the caller
without a usable acknowledgement even though the server admitted the work.
Inline SSE has the opposite requirement: once its response starts, the SDK must
never re-invoke the turn.

## Goals

- Give every high-level body-idempotent mutation equivalent retry behavior in
  Go, Python, and TypeScript.
- Derive the JSON field and `Idempotency-Key` header from one normalized value.
- Retry transport errors, `429`, transient `500`/`502`/`503`/`504` responses,
  and unreadable or invalid successful JSON acknowledgements within one bounded
  transport budget.
- Never re-invoke an inline stream after its response begins.
- Keep calls without an idempotency key non-retryable.
- Avoid marking compound `invokeAgent` requests with `session.mode: new` as
  replay-safe because each replay resolves a fresh session before turn-level
  deduplication.

## Non-goals

- Making `Idempotency-Key` a server-side alias for the JSON field.
- Changing server deduplication scope or persistence.
- Retrying model generation inside an admitted turn.
- Changing transcript cursor reconnection behavior.
- Automatically retrying every semantically idempotent `POST` that has no
  caller-supplied key.

## Proposal

Each SDK will normalize a caller key by trimming surrounding whitespace and
omitting blank values. High-level request construction will write that
normalized value into the existing JSON field, then derive the transport header
from the constructed request body rather than from a second caller value.

The following methods become replayable when a nonblank key is present:

| Operation | JSON key | Replay condition |
|---|---|---|
| Start loop run | `idempotency_key` | Key present |
| Invoke agent | `input.idempotency_key` | Key present and session mode is not `new` |
| Invoke agent inline stream | `input.idempotency_key` | Same condition, only until response start |
| Start existing-session turn | `idempotency_key` | Key present |
| Nudge session | `idempotency_key` | Key present |

The existing transport remains the single retry-budget owner. Its transient
HTTP allowlist is `429`, `500`, `502`, `503`, and `504`; exhausted `429`
responses retain the typed `RateLimitError`, while exhausted retryable `5xx`
responses pass through unchanged. For a replayable `POST` or `PATCH` that
receives a successful JSON response, the transport will fully read and
syntactically validate a copy of the response before returning it. A read
failure or invalid JSON is treated like a transient transport failure and
consumes the same exponential-backoff budget. The response remains readable by
the generated or high-level decoder.

Requests advertising `Accept: text/event-stream` are never pre-read. The
transport may retry a failure before it receives a response, plus any retryable
HTTP response, but it returns immediately once the successful stream response
starts. Later SSE read failures remain the cursor-reconnection layer's concern.

Cancellation remains terminal. TypeScript will treat an aborted signal,
`AbortError`, and `TimeoutError` as non-retryable so a timed-out request does not
loop on an already-aborted signal.

## Alternatives considered

### Parse JSON bodies in the high-level clients and run a second retry loop

This covers acknowledgement decoding but creates nested retry budgets: one at
the transport and another around the high-level method. A request could consume
the full transport budget repeatedly, making attempt counts and latency diverge
across languages.

### Teach transports to inspect request JSON for idempotency fields

This would avoid the header, but it couples a generic transport to endpoint-
specific body shapes, requires buffering outbound bodies, and contradicts the
documented header-gated retry contract.

### Send the header for every compound invoke with a key

This is unsafe for `session.mode: new`: the server creates a fresh session on
each request, while the body key is scoped only within the newly resolved
session. The SDK will still send the body key for compatibility but will not
mark that request replayable.

## Tradeoffs and consequences

- Successful replayable JSON writes are eagerly buffered and JSON-validated.
  These acknowledgements are small, but the transport now does slightly more
  work before returning them.
- A server that repeatedly emits invalid JSON will consume the configured retry
  budget before surfacing the final decoding error.
- `mode: new` remains non-retryable until the server introduces a pre-session
  idempotency scope or rejects that option combination.
- The header is an SDK transport signal for these endpoints, not a declared
  server alias. Raw HTTP callers must continue to use the documented JSON field.

## Rollout

This is an additive SDK behavior change with no wire-schema regeneration. Ship
the three implementations and their regression tests together. Customers may
remove application-owned pre-ack retry only after adopting a release containing
the full response-body retry behavior; transcript stream reconnection remains
unchanged.
