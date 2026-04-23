"""Rich error types raised by the Mobius Python SDK.

The per-call ``RateLimitError`` defined here is raised by the retrying
transport layer (see :mod:`deepnoodle.mobius.retry`) whenever a 429
response cannot be retried — either because retries are disabled, retries
are exhausted, or the request is a non-idempotent POST/PATCH.

``RateLimitedError`` (defined in :mod:`deepnoodle.mobius.client`) predates
this class and is kept as a subclass for backward compatibility: callers
that catch ``RateLimitedError`` continue to catch their narrower
per-job errors; callers that want to handle every rate-limit signal
should catch ``RateLimitError``.
"""

from __future__ import annotations

from datetime import datetime


class AuthRevokedError(Exception):
    """Raised when the server returns HTTP 401 on a worker-loop request.

    The credential has been revoked mid-execution. Distinct from
    :class:`LeaseLostError` (409 — the lease was reclaimed by the
    scheduler) because the remedy is operational: the process needs
    to restart under a fresh credential. The worker loop raises this
    so the process supervisor (k8s, systemd) can restart with rotated
    config; the orphan job is abandoned and retried by the scheduler
    after the lease expires.
    """

    def __init__(self, job_id: str | None = None) -> None:
        msg = "mobius: credential revoked"
        if job_id:
            msg = f"{msg} (job {job_id})"
        super().__init__(msg)
        self.job_id = job_id


class RateLimitError(Exception):
    """Raised when the server returns HTTP 429 and the request cannot be retried.

    Fields are populated from the response headers:

    * ``retry_after`` — seconds to wait before retrying, from ``Retry-After``.
      ``0.0`` when the header is absent or unparseable.
    * ``limit`` — total bucket capacity, from ``X-RateLimit-Limit``.
    * ``remaining`` — remaining capacity, from ``X-RateLimit-Remaining``.
    * ``reset_at`` — datetime when the current window ends, from
      ``X-RateLimit-Reset`` (interpreted as Unix seconds).
    * ``scope`` — ``"key"`` or ``"org"``, from ``X-RateLimit-Scope``.
    * ``policy`` — policy description, from ``X-RateLimit-Policy``
      (e.g. ``"10000;w=18000"``). Surfaced for diagnostics only.
    """

    def __init__(
        self,
        *,
        retry_after: float = 0.0,
        limit: int | None = None,
        remaining: int | None = None,
        reset_at: datetime | None = None,
        scope: str | None = None,
        policy: str | None = None,
        message: str | None = None,
    ) -> None:
        self.retry_after = retry_after
        self.limit = limit
        self.remaining = remaining
        self.reset_at = reset_at
        self.scope = scope
        self.policy = policy
        if message is None:
            scope_label = scope or "unknown"
            if retry_after:
                message = (
                    f"mobius: rate limit exceeded "
                    f"(scope={scope_label}, retry after {retry_after:g}s)"
                )
            else:
                message = f"mobius: rate limit exceeded (scope={scope_label})"
        super().__init__(message)
