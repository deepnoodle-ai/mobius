package mobius

import (
	"errors"
	"fmt"
	"time"
)

// ErrPayloadTooLarge is returned when the server rejects a custom event
// payload for exceeding the size limit (HTTP 413).
var ErrPayloadTooLarge = errors.New("mobius: custom event payload too large")

// ErrAuthRevoked is returned when the server rejects a worker-loop
// request with 401. Distinct from [ErrLeaseLost] (409 — the lease was
// reclaimed) because the remedy is operational, not workflow-level:
// the credential has been revoked mid-execution, the process needs to
// restart under a fresh credential, and the orphan job will be retried
// by the scheduler after the lease expires. Bubbles up out of the
// worker run loop with a non-zero exit code so the process supervisor
// can restart under a rotated credential.
var ErrAuthRevoked = errors.New("mobius: credential revoked")

// ErrWorkerInstanceConflict is returned when the server rejects a worker
// claim because another live process has already registered the same
// worker_instance_id in the project. Surfaces from the run loop as a
// hard error: the operator either configured the same instance ID in
// two processes, or two replicas auto-detected the same identifier.
// The message returned by [InstanceConflictError] names the offending
// project and instance ID so the operator can resolve.
var ErrWorkerInstanceConflict = errors.New("mobius: worker instance conflict")

// InstanceConflictError carries the human-readable remediation message
// for an [ErrWorkerInstanceConflict]. errors.Is(err, ErrWorkerInstanceConflict)
// keeps working; errors.As(err, &ic) reads the fields.
type InstanceConflictError struct {
	WorkerInstanceID string
	ProjectHandle    string
	Message          string
}

func (e *InstanceConflictError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.WorkerInstanceID != "" {
		return fmt.Sprintf(
			"mobius: worker_instance_id %q is already registered in project %q by another live process; configure a unique instance ID per process or rely on auto-detection",
			e.WorkerInstanceID, e.ProjectHandle,
		)
	}
	return ErrWorkerInstanceConflict.Error()
}

func (e *InstanceConflictError) Unwrap() error { return ErrWorkerInstanceConflict }

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
