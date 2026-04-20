"""Smoke tests for the hand-written Python Client wrapper.

Verifies the error translation layer around 409 lease-lost responses and
204 empty claims — the failure modes most likely to silently drift from the
Go and TypeScript wrappers.
"""

from __future__ import annotations

import httpx
import pytest

from deepnoodle.mobius import (
    DEFAULT_BASE_URL,
    Client,
    ClientOptions,
    LeaseLostError,
    PayloadTooLargeError,
    RateLimitedError,
)
from deepnoodle.mobius._api.models import JobClaimRequest, JobCompleteRequest, JobFenceRequest, Status as JobStatus


def _client_with(handler) -> Client:
    opts = ClientOptions(
        api_key="mbx_test",
        base_url="https://api.example.invalid",
        project="test-project",
    )
    c = Client(opts)
    c._http = httpx.Client(
        transport=httpx.MockTransport(handler),
        base_url=opts.base_url,
        headers={"Authorization": f"Bearer {opts.api_key}"},
    )
    return c


def test_claim_job_returns_none_on_204() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(204)

    client = _client_with(handler)
    job = client.claim_job(JobClaimRequest(worker_id="worker-1"))
    assert job is None


def test_client_defaults_to_production_api_host() -> None:
    client = Client("mbx_test")
    assert str(client._http.base_url).rstrip("/") == DEFAULT_BASE_URL
    client.close()


def test_claim_job_returns_job_on_200() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "job_id": "job_1",
                "run_id": "run_1",
                "workflow_name": "hello",
                "step_name": "greet",
                "action": "print",
                "parameters": {"msg": "hi"},
                "attempt": 1,
                "queue": "default",
            },
        )

    client = _client_with(handler)
    job = client.claim_job(JobClaimRequest(worker_id="worker-1"))
    assert job is not None
    assert job.job_id == "job_1"
    assert job.action == "print"
    assert job.parameters == {"msg": "hi"}


def test_heartbeat_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.heartbeat_job(
            "task_1", JobFenceRequest(worker_id="w", attempt=1)
        )


def test_complete_job_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.complete_job(
            "task_1",
            JobCompleteRequest(
                worker_id="w", attempt=1, status=JobStatus.completed
            ),
        )


def test_emit_job_event_posts_to_project_events_endpoint() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(204)

    client = _client_with(handler)
    client.emit_job_event(
        "task_1",
        worker_id="worker-1",
        attempt=2,
        type="scrape.page_done",
        payload={"url": "https://example.com"},
    )

    assert seen["path"] == "/projects/test-project/jobs/task_1/events"
    assert '"type":"scrape.page_done"' in str(seen["body"])
    assert '"attempt":2' in str(seen["body"])


def test_emit_job_events_413_raises_payload_too_large() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(413)

    client = _client_with(handler)
    with pytest.raises(PayloadTooLargeError):
        client.emit_job_event(
            "task_1",
            worker_id="worker-1",
            attempt=1,
            type="too.big",
            payload={"size": "x"},
        )


def test_emit_job_events_429_raises_rate_limited() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(429, headers={"Retry-After": "3"})

    client = _client_with(handler)
    with pytest.raises(RateLimitedError) as exc:
        client.emit_job_event(
            "task_1",
            worker_id="worker-1",
            attempt=1,
            type="progress",
            payload={"pct": 5},
        )
    assert exc.value.retry_after == 3
