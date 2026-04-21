// Package mobius is the Go client SDK for the Mobius work-coordination
// platform. It exposes a thin wrapper over the generated OpenAPI client
// (subpackage api) plus a long-running Worker for claiming and executing
// workflow jobs.
//
// # Rate limiting
//
// The client retries 429 Too Many Requests and 503 Service Unavailable
// responses automatically, respecting the Retry-After header and falling
// back to exponential backoff (1s, 2s, 4s, capped at 60s). Non-idempotent
// POST/PATCH requests are retried only when they carry an Idempotency-Key
// header; otherwise a 429 surfaces as *[RateLimitError] immediately.
//
// Set WithRetry(0) to disable retries entirely; 429 responses then bubble
// up as *RateLimitError on the first attempt. Callers can unwrap to
// [ErrRateLimited] (for category checks via errors.Is) or use errors.As to
// read the rich fields (retry-after, limit, remaining, reset-at, scope).
//
// The policy is documented in full at
// https://github.com/deepnoodle-ai/mobius/blob/main/docs/retries.md.
package mobius
