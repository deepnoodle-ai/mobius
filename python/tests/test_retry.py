"""Tests for the 429/503-aware retrying transport."""

from __future__ import annotations

from datetime import datetime, timezone
from email.utils import format_datetime

import httpx
import pytest

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    RateLimitError,
    RateLimitedError,
)
from deepnoodle.mobius.retry import (
    MAX_RETRY_BACKOFF_SECONDS,
    RetryingTransport,
)


class _Recorder:
    def __init__(self) -> None:
        self.sleeps: list[float] = []

    def sleep(self, seconds: float) -> None:
        self.sleeps.append(seconds)


def _wrap(handler, max_retries: int = 3, sleep=None, now=None) -> httpx.Client:
    base = httpx.MockTransport(handler)
    transport = RetryingTransport(
        base,
        max_retries=max_retries,
        sleep=sleep or (lambda _s: None),
        now=now,
    )
    return httpx.Client(transport=transport, base_url="https://api.example.invalid")


def test_retries_on_429_then_succeeds() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": "0"})
        return httpx.Response(200, json={"ok": True})

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.get("/x")
    assert resp.status_code == 200
    assert calls["n"] == 2
    # Retry-After: 0 means "retry now" — no sleep call required.
    assert rec.sleeps == []


def test_honors_retry_after_seconds() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": "7"})
        return httpx.Response(200)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.get("/x")
    assert resp.status_code == 200
    assert rec.sleeps == [7]


def test_honors_retry_after_http_date() -> None:
    fixed_now = datetime(2025, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    future = format_datetime(datetime(2025, 1, 1, 12, 0, 9, tzinfo=timezone.utc), usegmt=True)
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": future})
        return httpx.Response(200)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep, now=lambda: fixed_now) as client:
        resp = client.get("/x")
    assert resp.status_code == 200
    assert rec.sleeps == [9.0]


def test_clamps_retry_after() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(429, headers={"Retry-After": "9999", "X-RateLimit-Scope": "key"})

    rec = _Recorder()
    with _wrap(handler, max_retries=1, sleep=rec.sleep) as client:
        with pytest.raises(RateLimitError):
            client.get("/x")
    assert rec.sleeps == [MAX_RETRY_BACKOFF_SECONDS]


def test_post_without_idempotency_key_surfaces_immediately() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(
            429,
            headers={
                "Retry-After": "3",
                "X-RateLimit-Limit": "10000",
                "X-RateLimit-Remaining": "0",
                "X-RateLimit-Reset": "1735689600",
                "X-RateLimit-Scope": "key",
                "X-RateLimit-Policy": "10000;w=18000",
            },
        )

    rec = _Recorder()
    with _wrap(handler, max_retries=5, sleep=rec.sleep) as client:
        with pytest.raises(RateLimitError) as exc:
            client.post("/x", json={"a": 1})
    assert calls["n"] == 1
    assert rec.sleeps == []
    err = exc.value
    assert err.retry_after == 3.0
    assert err.limit == 10000
    assert err.remaining == 0
    assert err.scope == "key"
    assert err.policy == "10000;w=18000"
    assert err.reset_at == datetime.fromtimestamp(1735689600, tz=timezone.utc)


def test_post_with_idempotency_key_is_retried() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": "2"})
        return httpx.Response(200)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.post("/x", json={"a": 1}, headers={"Idempotency-Key": "k1"})
    assert resp.status_code == 200
    assert calls["n"] == 2
    assert rec.sleeps == [2]


def test_max_retries_zero_surfaces_immediately() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(429, headers={"X-RateLimit-Scope": "org"})

    rec = _Recorder()
    with _wrap(handler, max_retries=0, sleep=rec.sleep) as client:
        with pytest.raises(RateLimitError) as exc:
            client.get("/x")
    assert calls["n"] == 1
    assert rec.sleeps == []
    assert exc.value.scope == "org"


def test_exp_backoff_without_header() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(429)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        with pytest.raises(RateLimitError):
            client.get("/x")
    assert rec.sleeps == [1.0, 2.0, 4.0]
    assert calls["n"] == 4


def test_503_retried_then_passes_through() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(503)

    rec = _Recorder()
    with _wrap(handler, max_retries=2, sleep=rec.sleep) as client:
        resp = client.get("/x")
    assert resp.status_code == 503
    assert calls["n"] == 3
    assert len(rec.sleeps) == 2


def test_non_retryable_status_passes_through() -> None:
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(500)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.get("/x")
    assert resp.status_code == 500
    assert calls["n"] == 1
    assert rec.sleeps == []


def test_rate_limited_error_is_instance_of_rate_limit_error() -> None:
    # Legacy subclass relationship: except RateLimitError catches both.
    err = RateLimitedError("job_1", retry_after=5)
    assert isinstance(err, RateLimitError)


def test_client_constructor_installs_retrying_transport() -> None:
    # By default, client wraps its transport. With retry=0, a 429 surfaces
    # as RateLimitError.
    client = Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            retry=0,
        )
    )
    # Replace the inner base to avoid hitting the network but keep the
    # RetryingTransport wrapper that the constructor installed.
    assert isinstance(client._http._transport, RetryingTransport)
    client.close()
