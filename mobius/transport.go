package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// RetryingTransport wraps an http.RoundTripper to retry 429, transient
// 500/502/503/504 responses, and transport-level errors per the policy
// documented in docs/retries.md:
//
//   - 429, 500, 502, 503, and 504 responses are retried, as are
//     transport-level errors (connection reset, unexpected EOF, dial failure)
//     that never yield an HTTP response.
//   - GET/HEAD/PUT/DELETE are always replayable; POST/PATCH are replayed
//     only when the request is a replay-safe write — an Idempotency-Key
//     header is set, or a curated adopt-mode create marked the request
//     internally (its idempotency comes from external_ref). Non-replayable
//     requests surface the error without a retry.
//   - Sleep duration is taken from Retry-After (int seconds or HTTP-date)
//     or, absent that header, from exponential backoff (1s, 2s, 4s, ...)
//     capped at [MaxRetryBackoff]. Transport errors always use backoff.
//   - When retries are exhausted — or not allowed — a 429 surfaces as
//     [*RateLimitError] populated from the response headers, and a
//     transport error surfaces as the underlying error.
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
			// Transport-level failure (connection reset, unexpected EOF,
			// dial error, ...). These never produce an HTTP response, so the
			// HTTP-status handling below cannot see them. Retry idempotent,
			// replayable requests on the same backoff budget; give up once
			// the budget is spent, the request is not safe to replay, or the
			// context is done.
			if !canRetry || attempt >= t.MaxRetries || ctx.Err() != nil {
				return nil, err
			}
			wait := expBackoff(attempt)
			if t.Logger != nil {
				t.Logger.Warn("mobius: retrying after transport error",
					slog.String("method", req.Method),
					slog.String("url", req.URL.String()),
					slog.String("error", err.Error()),
					slog.Int("attempt", attempt+1),
					slog.Duration("wait", wait),
				)
			}
			if serr := t.sleepFn()(ctx, wait); serr != nil {
				return nil, err
			}
			continue
		}

		if !isRetryableStatus(resp.StatusCode) {
			if err := prepareReplayableJSONResponse(req, resp); err != nil {
				_ = resp.Body.Close()
				if !canRetry || attempt >= t.MaxRetries || ctx.Err() != nil {
					return nil, err
				}
				wait := expBackoff(attempt)
				if t.Logger != nil {
					t.Logger.Warn("mobius: retrying after response body error",
						slog.String("method", req.Method),
						slog.String("url", req.URL.String()),
						slog.String("error", err.Error()),
						slog.Int("attempt", attempt+1),
						slog.Duration("wait", wait),
					)
				}
				if serr := t.sleepFn()(ctx, wait); serr != nil {
					return nil, err
				}
				continue
			}
			return resp, nil
		}

		outOfBudget := attempt >= t.MaxRetries || !canRetry
		if outOfBudget {
			if resp.StatusCode == http.StatusTooManyRequests {
				rle := parseRateLimitError(resp)
				_ = resp.Body.Close()
				return nil, rle
			}
			return resp, nil
		}

		wait := retryAfterOrBackoff(resp, attempt, t.nowFn())
		if t.Logger != nil {
			t.Logger.Warn("mobius: retrying after transient HTTP response",
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

// prepareReplayableJSONResponse completes the acknowledgement boundary for
// replay-safe writes. Reading and validating the small JSON response here
// keeps response-body failures inside the transport's single retry budget.
// Event streams are intentionally excluded: once their response begins, the
// original invocation must never be sent again.
func prepareReplayableJSONResponse(req *http.Request, resp *http.Response) error {
	if resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	method := strings.ToUpper(req.Method)
	if (method != http.MethodPost && method != http.MethodPatch) || !isReplaySafeWrite(req) {
		return nil
	}
	if acceptsEventStream(req.Header.Get("Accept")) {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mobius: read replay-safe response body: %w", err)
	}
	_ = resp.Body.Close()
	if !json.Valid(body) {
		return fmt.Errorf("mobius: replay-safe response body is not valid JSON")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func acceptsEventStream(value string) bool {
	for _, mediaRange := range strings.Split(value, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(mediaRange, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// replaySafeContextKey marks a request whose idempotency is guaranteed by
// something other than an Idempotency-Key header — today the adopt-mode
// create-or-adopt calls, where the server dedupes on external_ref. Set only
// by curated methods via contextWithReplaySafe; never a public knob.
type replaySafeContextKey struct{}

func contextWithReplaySafe(ctx context.Context) context.Context {
	return context.WithValue(ctx, replaySafeContextKey{}, true)
}

// isReplaySafeWrite reports whether a POST/PATCH request opted into the
// replay-safe path: an Idempotency-Key header, or the internal per-request
// adopt-mode marker.
func isReplaySafeWrite(req *http.Request) bool {
	if req.Header.Get("Idempotency-Key") != "" {
		return true
	}
	marked, _ := req.Context().Value(replaySafeContextKey{}).(bool)
	return marked
}

// isIdempotent reports whether the method-plus-headers combination makes
// the request safe to retry on a transient HTTP response.
func isIdempotent(req *http.Request) bool {
	switch strings.ToUpper(req.Method) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	case http.MethodPost, http.MethodPatch:
		return isReplaySafeWrite(req)
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
	return expBackoff(attempt)
}

// expBackoff returns the exponential backoff for a zero-based attempt index
// (1s, 2s, 4s, ...), clamped to [MaxRetryBackoff].
func expBackoff(attempt int) time.Duration {
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
