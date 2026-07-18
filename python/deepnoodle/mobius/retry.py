"""Transient-response-aware retrying transport for the Mobius Python SDK.

Implements the shared retry policy documented in ``../docs/retries.md``.
Wraps an underlying :class:`httpx.BaseTransport` and transparently retries
eligible responses; surfaces a typed :class:`RateLimitError` when a 429
cannot be retried.
"""

from __future__ import annotations

import email.utils
import json
import time
from datetime import datetime, timezone
from typing import Callable

import httpx

from .errors import RateLimitError

DEFAULT_MAX_RETRIES = 3
MAX_RETRY_BACKOFF_SECONDS = 60.0
BASE_RETRY_BACKOFF_SECONDS = 1.0

_IDEMPOTENT_METHODS = frozenset({"GET", "HEAD", "PUT", "DELETE", "OPTIONS"})
_RETRYABLE_STATUS_CODES = frozenset({429, 500, 502, 503, 504})


class RetryingTransport(httpx.BaseTransport):
    """Retries transient responses and transport errors per the shared spec.

    Only GET/HEAD/PUT/DELETE/OPTIONS and replay-safe POST/PATCH requests —
    those carrying an ``Idempotency-Key`` header, or marked internally via
    ``request.extensions["replay_safe"]`` by the curated adopt-mode creates
    (whose idempotency comes from ``external_ref``) — are retried; other
    POST/PATCH requests surface ``RateLimitError`` immediately on 429 and
    re-raise transport errors immediately.
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
        idempotent = _is_idempotent(request)
        while True:
            response: httpx.Response | None = None
            try:
                response = self._base.handle_request(request)
                _prepare_replayable_json_response(request, response)
            except (httpx.TransportError, httpx.DecodingError):
                # No HTTP response was produced (connection reset, EOF,
                # I/O timeout, ...) or the acknowledgement body could not be
                # read and validated. Retry replayable, idempotent requests on
                # the same exponential-backoff budget; otherwise re-raise.
                if response is not None:
                    response.close()
                if not idempotent or attempt >= self._max_retries:
                    raise
                wait = _clamp(BASE_RETRY_BACKOFF_SECONDS * (2**attempt))
                if wait > 0:
                    self._sleep(wait)
                attempt += 1
                continue

            status = response.status_code

            if status not in _RETRYABLE_STATUS_CODES:
                return response

            out_of_budget = attempt >= self._max_retries or not idempotent

            if out_of_budget:
                if status == 429:
                    _drain(response)
                    raise _build_rate_limit_error(response)
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
        return _is_replay_safe_write(request)
    return False


def _is_replay_safe_write(request: httpx.Request) -> bool:
    """A POST/PATCH that opted into the replay-safe path.

    Either an ``Idempotency-Key`` header, or the internal per-request
    adopt-mode marker set by the curated create-or-adopt methods — never a
    public knob.
    """

    if request.headers.get("Idempotency-Key") not in (None, ""):
        return True
    return bool(request.extensions.get("replay_safe"))


def _prepare_replayable_json_response(
    request: httpx.Request,
    response: httpx.Response,
) -> None:
    """Complete replay-safe JSON acknowledgements inside the retry budget.

    SSE responses are deliberately left unread: once the response starts, a
    later stream failure must be handled by transcript reconnection rather
    than reinvoking the admission request.
    """

    content_type = response.headers.get("Content-Type", "").lower()
    if (
        request.method.upper() not in {"POST", "PATCH"}
        or not _is_replay_safe_write(request)
        or _accepts_event_stream(request.headers.get("Accept", ""))
        or not 200 <= response.status_code < 300
        or response.status_code == 204
        or "json" not in content_type
    ):
        return

    try:
        content = response.read()
        json.loads(content)
    except httpx.TransportError:
        raise
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise httpx.DecodingError(
            "mobius: replay-safe response body is not valid JSON",
            request=request,
        ) from exc


def _accepts_event_stream(value: str) -> bool:
    return any(
        media_range.split(";", 1)[0].strip().lower() == "text/event-stream"
        for media_range in value.split(",")
    )


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
