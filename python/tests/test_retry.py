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


class _FailingStream(httpx.SyncByteStream):
    def __init__(self, request: httpx.Request) -> None:
        self._request = request

    def __iter__(self):
        raise httpx.ReadError("stream disconnected", request=self._request)
        yield b""  # pragma: no cover


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


def test_replay_safe_post_reuses_exact_body_and_key_after_network_and_503_failures() -> None:
    calls = 0
    bodies: list[bytes] = []
    keys: list[str | None] = []

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        bodies.append(request.read())
        keys.append(request.headers.get("Idempotency-Key"))
        if calls == 1:
            raise httpx.ConnectError("connection reset by peer", request=request)
        if calls == 2:
            return httpx.Response(503, headers={"Retry-After": "0"})
        return httpx.Response(202, json={"accepted": True})

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.post(
            "/x",
            content=b'{"message":"same"}',
            headers={"Idempotency-Key": "k1", "Content-Type": "application/json"},
        )

    assert resp.status_code == 202
    assert calls == 3
    assert bodies == [b'{"message":"same"}'] * 3
    assert keys == ["k1"] * 3
    assert rec.sleeps == [1.0]


def test_unreadable_and_invalid_replay_safe_json_acknowledgements_are_retried() -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            return httpx.Response(
                202,
                headers={"Content-Type": "application/json"},
                stream=_FailingStream(request),
            )
        if calls == 2:
            return httpx.Response(
                202,
                content=b"{",
                headers={"Content-Type": "application/json"},
            )
        return httpx.Response(202, json={"accepted": True})

    rec = _Recorder()
    with _wrap(handler, max_retries=2, sleep=rec.sleep) as client:
        resp = client.post("/x", json={}, headers={"Idempotency-Key": "k1"})

    assert resp.json() == {"accepted": True}
    assert calls == 3
    assert rec.sleeps == [1.0, 2.0]


def test_sse_failure_after_response_start_never_reinvokes() -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(
            200,
            headers={"Content-Type": "text/event-stream"},
            stream=_FailingStream(request),
        )

    with _wrap(handler, max_retries=3) as client:
        with client.stream(
            "POST",
            "/x",
            content=b"{}",
            headers={
                "Accept": "text/event-stream",
                "Idempotency-Key": "k1",
            },
        ) as response:
            with pytest.raises(httpx.ReadError, match="stream disconnected"):
                response.read()

    assert calls == 1


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


def test_retries_on_transport_error_then_succeeds() -> None:
    calls = {"n": 0}

    def handler(req: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] <= 2:
            raise httpx.ConnectError("connection reset by peer", request=req)
        return httpx.Response(200, json={"ok": True})

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        resp = client.get("/x")
    assert resp.status_code == 200
    assert calls["n"] == 3
    assert rec.sleeps == [1.0, 2.0]


def test_transport_error_exhausts_budget_reraises() -> None:
    calls = {"n": 0}

    def handler(req: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        raise httpx.ReadError("dial tcp: connection refused", request=req)

    rec = _Recorder()
    with _wrap(handler, max_retries=2, sleep=rec.sleep) as client:
        with pytest.raises(httpx.TransportError):
            client.get("/x")
    # attempts 0 and 1 back off; attempt 2 is out of budget and re-raises.
    assert calls["n"] == 3
    assert rec.sleeps == [1.0, 2.0]


def test_transport_error_not_retried_for_non_idempotent() -> None:
    calls = {"n": 0}

    def handler(req: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        raise httpx.ConnectError("unexpected EOF", request=req)

    rec = _Recorder()
    with _wrap(handler, max_retries=3, sleep=rec.sleep) as client:
        with pytest.raises(httpx.TransportError):
            client.post("/x", json={"a": 1})
    assert calls["n"] == 1
    assert rec.sleeps == []


def test_transport_error_max_retries_zero_reraises() -> None:
    calls = {"n": 0}

    def handler(req: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        raise httpx.ConnectError("connection reset by peer", request=req)

    rec = _Recorder()
    with _wrap(handler, max_retries=0, sleep=rec.sleep) as client:
        with pytest.raises(httpx.TransportError):
            client.get("/x")
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
    assert isinstance(client._client._transport, RetryingTransport)
    client.close()
