package mobius

import (
	"errors"
	"fmt"
	"time"
)

// ErrPayloadTooLarge is returned when the server rejects a custom event
// payload for exceeding the size limit (HTTP 413).
var ErrPayloadTooLarge = errors.New("mobius: custom event payload too large")

// ErrRateLimited is the sentinel returned for rate-limited requests (HTTP
// 429). Rich details live on [RateLimitError]; use errors.Is to detect the
// category and errors.As to read the fields.
var ErrRateLimited = errors.New("mobius: rate limited")

// RateLimitError carries the parsed rate-limit response headers emitted by
// the Mobius API alongside a 429 response. Returned by the retrying
// transport after retries are exhausted, or immediately when retries are
// disabled or the request is a non-idempotent POST/PATCH.
//
// Callers doing errors.Is(err, ErrRateLimited) keep working; callers that
// want the rich fields use errors.As(err, &rle).
type RateLimitError struct {
	// RetryAfter is the server-recommended wait before the next request,
	// parsed from the Retry-After header. Zero when the header is absent or
	// unparseable.
	RetryAfter time.Duration
	// Limit is the bucket's total capacity (X-RateLimit-Limit).
	Limit int
	// Remaining is the bucket's remaining capacity (X-RateLimit-Remaining).
	// Zero when the response is a 429.
	Remaining int
	// ResetAt is when the current window ends (X-RateLimit-Reset, Unix
	// seconds).
	ResetAt time.Time
	// Scope is the bucket scope, "key" or "org" (X-RateLimit-Scope).
	Scope string
	// Policy is the bucket policy description (X-RateLimit-Policy), e.g.
	// "10000;w=18000". Surfaced for diagnostics; not used for control flow.
	Policy string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf(
			"mobius: rate limit exceeded (scope=%s, retry after %s)",
			scopeForMessage(e.Scope), e.RetryAfter.Round(time.Second),
		)
	}
	return fmt.Sprintf("mobius: rate limit exceeded (scope=%s)", scopeForMessage(e.Scope))
}

// Unwrap returns ErrRateLimited so errors.Is(err, ErrRateLimited) keeps
// working for callers that used the sentinel before RateLimitError existed.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

func scopeForMessage(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
