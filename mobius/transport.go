package mobius

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryBackoff caps the per-attempt sleep. A server that asks for a
// longer wait is ignored beyond this cap; callers that want to honor very
// long delays should disable retries and implement their own policy.
const MaxRetryBackoff = 60 * time.Second

const baseRetryBackoff = 1 * time.Second

// RetryingTransport wraps an http.RoundTripper to retry 429 and 503
// responses per the policy documented in docs/retries.md:
//
//   - Only 429 and 503 are retried.
//   - GET/HEAD/PUT/DELETE are always replayable; POST/PATCH are replayed
//     only when an Idempotency-Key header is set.
//   - Sleep duration is taken from Retry-After (int seconds or HTTP-date)
//     or, absent that header, from exponential backoff (1s, 2s, 4s, ...)
//     capped at [MaxRetryBackoff].
//   - When retries are exhausted — or not allowed — a 429 surfaces as
//     [*RateLimitError] populated from the response headers.
type RetryingTransport struct {
	// Base is the underlying transport. Defaults to [http.DefaultTransport].
	Base http.RoundTripper
	// MaxRetries is the number of retry attempts; 0 disables retries.
	MaxRetries int
	// Logger receives warn-level messages when a retry is scheduled. Nil
	// is allowed.
	Logger *slog.Logger

	// sleep is pluggable for tests.
	sleep func(ctx context.Context, d time.Duration) error
	// now is pluggable for tests that parse HTTP-date Retry-After values.
	now func() time.Time
}

func (t *RetryingTransport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

func (t *RetryingTransport) sleepFn() func(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep
	}
	return contextSleep
}

func (t *RetryingTransport) nowFn() func() time.Time {
	if t.now != nil {
		return t.now
	}
	return time.Now
}

// RoundTrip implements http.RoundTripper.
func (t *RetryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	canRetry := t.MaxRetries > 0 && isReplayable(req) && isIdempotent(req)

	for attempt := 0; ; attempt++ {
		thisReq := req
		if attempt > 0 {
			cloned, err := cloneForRetry(req)
			if err != nil {
				// Body unavailable for replay; give up.
				return t.base().RoundTrip(req)
			}
			thisReq = cloned
		}

		resp, err := t.base().RoundTrip(thisReq)
		if err != nil {
			return nil, err
		}

		if !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		outOfBudget := attempt >= t.MaxRetries || !canRetry
		if resp.StatusCode == http.StatusTooManyRequests && outOfBudget {
			rle := parseRateLimitError(resp)
			_ = resp.Body.Close()
			return nil, rle
		}
		if resp.StatusCode == http.StatusServiceUnavailable && outOfBudget {
			return resp, nil
		}

		wait := retryAfterOrBackoff(resp, attempt, t.nowFn())
		if t.Logger != nil {
			t.Logger.Warn("mobius: retrying after rate-limit response",
				slog.String("method", req.Method),
				slog.String("url", req.URL.String()),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempt", attempt+1),
				slog.Duration("wait", wait),
			)
		}
		drainAndClose(resp)

		if err := t.sleepFn()(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// isIdempotent reports whether the method-plus-headers combination makes
// the request safe to retry on a 429/503.
func isIdempotent(req *http.Request) bool {
	switch strings.ToUpper(req.Method) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	case http.MethodPost, http.MethodPatch:
		return req.Header.Get("Idempotency-Key") != ""
	default:
		return false
	}
}

// isReplayable reports whether the request body can be re-sent. Requests
// constructed via [http.NewRequestWithContext] with a *bytes.Reader,
// *bytes.Buffer, or *strings.Reader automatically get a GetBody func —
// the common path for both this SDK and the generated client.
func isReplayable(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	return req.GetBody != nil
}

func cloneForRetry(req *http.Request) (*http.Request, error) {
	c := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		c.Body = body
	}
	return c, nil
}

func retryAfterOrBackoff(resp *http.Response, attempt int, now func() time.Time) time.Duration {
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), now); ok {
		return clampBackoff(d)
	}
	multiplier := math.Pow(2, float64(attempt))
	return clampBackoff(time.Duration(float64(baseRetryBackoff) * multiplier))
}

func clampBackoff(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > MaxRetryBackoff {
		return MaxRetryBackoff
	}
	return d
}

func parseRetryAfter(v string, now func() time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	trimmed := strings.TrimSpace(v)
	if secs, err := strconv.Atoi(trimmed); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if ts, err := http.ParseTime(trimmed); err == nil {
		d := ts.Sub(now())
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// parseRateLimitError builds a RateLimitError from the given response's
// headers. The caller is responsible for closing resp.Body.
func parseRateLimitError(resp *http.Response) *RateLimitError {
	h := resp.Header
	retryAfter, _ := parseRetryAfter(h.Get("Retry-After"), time.Now)

	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, _ := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	var resetAt time.Time
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			resetAt = time.Unix(n, 0)
		}
	}
	return &RateLimitError{
		RetryAfter: retryAfter,
		Limit:      limit,
		Remaining:  remaining,
		ResetAt:    resetAt,
		Scope:      h.Get("X-RateLimit-Scope"),
		Policy:     h.Get("X-RateLimit-Policy"),
	}
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
