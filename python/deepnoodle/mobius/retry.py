"""429/503-aware retrying transport for the Mobius Python SDK.

Implements the shared retry policy documented in ``../docs/retries.md``.
Wraps an underlying :class:`httpx.BaseTransport` and transparently retries
eligible responses; surfaces a typed :class:`RateLimitError` when a 429
cannot be retried.
"""

from __future__ import annotations

import email.utils
import time
from datetime import datetime, timezone
from typing import Callable

import httpx

from .errors import RateLimitError

DEFAULT_MAX_RETRIES = 3
MAX_RETRY_BACKOFF_SECONDS = 60.0
BASE_RETRY_BACKOFF_SECONDS = 1.0

_IDEMPOTENT_METHODS = frozenset({"GET", "HEAD", "PUT", "DELETE", "OPTIONS"})


class RetryingTransport(httpx.BaseTransport):
    """Wraps a transport, retrying 429 and 503 responses per the shared spec.

    Only GET/HEAD/PUT/DELETE/OPTIONS and POST/PATCH requests carrying an
    ``Idempotency-Key`` header are retried; other POST/PATCH requests
    surface ``RateLimitError`` immediately on 429.
    """

    def __init__(
        self,
        base: httpx.BaseTransport,
        max_retries: int = DEFAULT_MAX_RETRIES,
        *,
        sleep: Callable[[float], None] = time.sleep,
        now: Callable[[], datetime] | None = None,
    ) -> None:
        self._base = base
        self._max_retries = max(0, int(max_retries))
        self._sleep = sleep
        self._now = now or (lambda: datetime.now(timezone.utc))

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        attempt = 0
        while True:
            response = self._base.handle_request(request)
            status = response.status_code

            if status not in (429, 503):
                return response

            idempotent = _is_idempotent(request)
            out_of_budget = attempt >= self._max_retries or not idempotent

            if status == 429 and out_of_budget:
                _drain(response)
                raise _build_rate_limit_error(response)
            if status == 503 and out_of_budget:
                return response

            wait = _retry_after_or_backoff(response, attempt, self._now())
            _drain(response)
            if wait > 0:
                self._sleep(wait)
            attempt += 1

    def close(self) -> None:
        self._base.close()


def _is_idempotent(request: httpx.Request) -> bool:
    method = request.method.upper()
    if method in _IDEMPOTENT_METHODS:
        return True
    if method in {"POST", "PATCH"}:
        return request.headers.get("Idempotency-Key") not in (None, "")
    return False


def _drain(response: httpx.Response) -> None:
    try:
        response.read()
    except httpx.StreamError:
        pass
    finally:
        response.close()


def _retry_after_or_backoff(
    response: httpx.Response,
    attempt: int,
    now: datetime,
) -> float:
    header = response.headers.get("Retry-After")
    parsed = _parse_retry_after(header, now)
    if parsed is not None:
        return _clamp(parsed)
    return _clamp(BASE_RETRY_BACKOFF_SECONDS * (2**attempt))


def _parse_retry_after(value: str | None, now: datetime) -> float | None:
    if value is None or value == "":
        return None
    stripped = value.strip()
    try:
        return float(int(stripped))
    except ValueError:
        pass
    parsed = email.utils.parsedate_to_datetime(stripped)
    if parsed is None:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    delta = (parsed - now).total_seconds()
    return max(0.0, delta)


def _clamp(seconds: float) -> float:
    if seconds < 0:
        return 0.0
    if seconds > MAX_RETRY_BACKOFF_SECONDS:
        return MAX_RETRY_BACKOFF_SECONDS
    return seconds


def _build_rate_limit_error(response: httpx.Response) -> RateLimitError:
    h = response.headers
    retry_after = _parse_retry_after(
        h.get("Retry-After"),
        datetime.now(timezone.utc),
    ) or 0.0
    limit = _int_or_none(h.get("X-RateLimit-Limit"))
    remaining = _int_or_none(h.get("X-RateLimit-Remaining"))
    reset_at: datetime | None = None
    reset_value = h.get("X-RateLimit-Reset")
    if reset_value:
        try:
            reset_at = datetime.fromtimestamp(int(reset_value), tz=timezone.utc)
        except (ValueError, OverflowError):
            reset_at = None
    scope = h.get("X-RateLimit-Scope") or None
    policy = h.get("X-RateLimit-Policy") or None
    return RateLimitError(
        retry_after=retry_after,
        limit=limit,
        remaining=remaining,
        reset_at=reset_at,
        scope=scope,
        policy=policy,
    )


def _int_or_none(v: str | None) -> int | None:
    if v is None or v == "":
        return None
    try:
        return int(v)
    except ValueError:
        return None
