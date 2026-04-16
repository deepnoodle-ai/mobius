"""Smoke tests for the hand-written Python Client wrapper.

Verifies the error translation layer around 409 lease-lost responses and
204 empty claims — the failure modes most likely to silently drift from the
Go and TypeScript wrappers.
"""

from __future__ import annotations

import httpx
import pytest

from deepnoodle.mobius import DEFAULT_BASE_URL, Client, ClientOptions, LeaseLostError
from deepnoodle.mobius._api.models import (
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    Status as JobStatus,
)


def _client_with(handler) -> Client:
    opts = ClientOptions(
        api_key="mbx_test",
        base_url="https://api.example.invalid",
        namespace="test-ns",
    )
    c = Client(opts)
    c._http = httpx.Client(
        transport=httpx.MockTransport(handler),
        base_url=opts.base_url,
        headers={"Authorization": f"Bearer {opts.api_key}"},
    )
    return c


def test_claim_task_returns_none_on_204() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(204)

    client = _client_with(handler)
    task = client.claim_task(JobClaimRequest(worker_id="worker-1"))
    assert task is None


def test_client_defaults_to_production_api_host() -> None:
    client = Client("mbx_test")
    assert str(client._http.base_url).rstrip("/") == DEFAULT_BASE_URL
    client.close()


def test_claim_task_returns_task_on_200() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "data": {
                    "job_id": "task_1",
                    "run_id": "run_1",
                    "namespace_id": "ns_1",
                    "workflow_name": "hello",
                    "step_name": "greet",
                    "action": "print",
                    "parameters": {"msg": "hi"},
                    "attempt": 1,
                    "queue": "default",
                }
            },
        )

    client = _client_with(handler)
    task = client.claim_task(JobClaimRequest(worker_id="worker-1"))
    assert task is not None
    assert task.job_id == "task_1"
    assert task.action == "print"
    assert task.parameters == {"msg": "hi"}


def test_heartbeat_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.heartbeat_task(
            "task_1", JobFenceRequest(worker_id="w", attempt=1)
        )


def test_complete_task_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.complete_task(
            "task_1",
            JobCompleteRequest(
                worker_id="w", attempt=1, status=JobStatus.completed
            ),
        )
