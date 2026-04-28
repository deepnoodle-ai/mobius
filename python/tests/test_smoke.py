"""Smoke tests for the hand-written Python Client wrapper.

Verifies the error translation layer around 409 lease-lost responses and
204 empty claims — the failure modes most likely to silently drift from the
Go and TypeScript wrappers.
"""

from __future__ import annotations

import json

import httpx
import pytest

from deepnoodle.mobius import (
    DEFAULT_BASE_URL,
    Client,
    ClientOptions,
    LeaseLostError,
    ListRunsOptions,
    ListWorkflowsOptions,
    PayloadTooLargeError,
    RateLimitedError,
    StartRunOptions,
    SyntheticWebhookDelivery,
    UpdateWorkflowOptions,
    WEBHOOK_EVENT_TYPE_HEADER,
    WEBHOOK_SIGNATURE_HEADER,
    WaitRunOptions,
    WorkerInstanceConflictError,
    WorkflowOptions,
    build_synthetic_webhook_payload,
    deliver_synthetic_webhook,
    parse_signed_webhook_request,
    parse_webhook_event,
    sign_webhook_payload,
    verify_webhook_signature,
)
from deepnoodle.mobius._api.models import JobClaimRequest, JobCompleteRequest, JobFenceRequest, Status as JobStatus, WorkflowRunStatus, WorkflowSpec


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
    job = client.claim_job(
        JobClaimRequest(
            worker_instance_id="worker-1",
            worker_session_token="test-session-token",
            concurrency_limit=1,
        )
    )
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
    job = client.claim_job(
        JobClaimRequest(
            worker_instance_id="worker-1",
            worker_session_token="test-session-token",
            concurrency_limit=1,
        )
    )
    assert job is not None
    assert job.job_id == "job_1"
    assert job.action == "print"
    assert job.parameters == {"msg": "hi"}


def test_claim_job_409_with_worker_instance_conflict_envelope_raises_typed_error() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": {
                    "code": "worker_instance_conflict",
                    "message": "worker_instance_id worker-1 is already registered",
                },
            },
        )

    client = _client_with(handler)
    with pytest.raises(WorkerInstanceConflictError) as excinfo:
        client.claim_job(
            JobClaimRequest(
                worker_instance_id="worker-1",
                worker_session_token="test-session-token",
                concurrency_limit=1,
            )
        )
    assert excinfo.value.worker_instance_id == "worker-1"


def test_heartbeat_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.heartbeat_job(
            "task_1",
            JobFenceRequest(
                worker_instance_id="w",
                worker_session_token="test-session-token",
                attempt=1,
            ),
        )


def test_complete_job_409_raises_lease_lost() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.complete_job(
            "task_1",
            JobCompleteRequest(
                worker_instance_id="w",
                worker_session_token="test-session-token",
                attempt=1,
                status=JobStatus.completed,
            ),
        )


# Session-token is the canonical fence value (the SDK stamps it on
# every claim and presents it on heartbeat / complete / events).
# The tests below mirror the worker_instance_id paths above so any
# drift in the token-bearing wire shape fails the suite.
def test_heartbeat_with_session_token_serializes_token() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = request.read().decode()
        return httpx.Response(409)

    client = _client_with(handler)
    with pytest.raises(LeaseLostError):
        client.heartbeat_job(
            "task_1",
            JobFenceRequest(worker_session_token="tok-abc", attempt=1),
        )
    assert '"worker_session_token":"tok-abc"' in str(seen["body"])


def test_complete_job_with_session_token_serializes_token() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = request.read().decode()
        return httpx.Response(204)

    client = _client_with(handler)
    client.complete_job(
        "task_1",
        JobCompleteRequest(
            worker_session_token="tok-abc",
            attempt=1,
            status=JobStatus.completed,
        ),
    )
    assert '"worker_session_token":"tok-abc"' in str(seen["body"])


def test_emit_job_event_posts_to_project_events_endpoint() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(204)

    client = _client_with(handler)
    client.emit_job_event(
        "task_1",
        worker_instance_id="worker-1",
        attempt=2,
        type="scrape.page_done",
        payload={"url": "https://example.com"},
    )

    assert seen["path"] == "/v1/projects/test-project/jobs/task_1/events"
    assert '"type":"scrape.page_done"' in str(seen["body"])
    assert '"attempt":2' in str(seen["body"])


def test_emit_job_events_413_raises_payload_too_large() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(413)

    client = _client_with(handler)
    with pytest.raises(PayloadTooLargeError):
        client.emit_job_event(
            "task_1",
            worker_instance_id="worker-1",
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
            worker_instance_id="worker-1",
            attempt=1,
            type="progress",
            payload={"pct": 5},
        )
    assert exc.value.retry_after == 3


def test_start_run_posts_correlated_request() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(202, json=_run_body("run_1", "active"))

    client = _client_with(handler)
    run = client.start_run(
        WorkflowSpec(name="demo", steps=[]),
        StartRunOptions(
            queue="research",
            external_id="external-1",
            metadata={"org_id": "org_1"},
            inputs={"topic": "sdk"},
        ),
    )

    assert run.id == "run_1"
    assert seen["path"] == "/v1/projects/test-project/runs"
    assert '"mode":"inline"' in str(seen["body"])
    assert '"queue":"research"' in str(seen["body"])
    assert '"external_id":"external-1"' in str(seen["body"])


def test_start_workflow_run_uses_workflow_bound_route() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(202, json=_run_body("run_1", "active"))

    client = _client_with(handler)
    run = client.start_workflow_run(
        "wf_1",
        StartRunOptions(
            external_id="external-1",
            inputs={"topic": "sdk"},
        ),
    )

    assert run.id == "run_1"
    assert seen["path"] == "/v1/projects/test-project/workflows/wf_1/runs"
    assert '"external_id":"external-1"' in str(seen["body"])
    assert '"mode"' not in str(seen["body"])
    assert '"definition_id"' not in str(seen["body"])


def test_run_control_helpers_use_project_scoped_paths() -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        path = request.url.path
        if path.endswith("/signals"):
            return httpx.Response(
                202, json={"id": "sig_1", "run_id": "run_1", "name": "approval"}
            )
        if path.endswith("/cancellations") or path.endswith("/resumptions"):
            return httpx.Response(204)
        if path.endswith("/runs/run_1"):
            return httpx.Response(200, json=_run_detail_body("run_1", "completed"))
        return httpx.Response(
            200, json={"items": [_run_body("run_1", "completed")], "has_more": False}
        )

    client = _client_with(handler)
    assert client.get_run("run_1").status is WorkflowRunStatus.completed
    assert (
        len(
            client.list_runs(
                ListRunsOptions(
                    status=WorkflowRunStatus.completed,
                    external_id="external-1",
                )
            ).items
        )
        == 1
    )
    client.cancel_run("run_1")
    client.resume_run("run_1")
    assert client.send_run_signal("run_1", "approval", {"ok": True}).id == "sig_1"

    assert any("/runs?status=completed" in url for url in seen)
    assert any(url.endswith("/runs/run_1/cancellations") for url in seen)
    assert any(url.endswith("/runs/run_1/resumptions") for url in seen)
    assert any(url.endswith("/runs/run_1/signals") for url in seen)


def test_wait_run_fetches_after_stream_closes_before_terminal() -> None:
    calls = {"get": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/events"):
            return httpx.Response(
                200,
                text='event: run_updated\nid: 7\ndata: {"type":"run_updated","run_id":"run_1","seq":7,"timestamp":"2026-04-27T00:00:00Z","data":{"status":"active"}}\n\n',
                headers={"Content-Type": "text/event-stream"},
            )
        calls["get"] += 1
        status = "active" if calls["get"] == 1 else "completed"
        return httpx.Response(200, json=_run_detail_body("run_1", status))

    client = _client_with(handler)
    run = client.wait_run(
        "run_1",
        WaitRunOptions(reconnect_delay=0.001, timeout=1),
    )

    assert run.status is WorkflowRunStatus.completed
    assert calls["get"] == 2


def test_webhook_helpers_verify_and_parse_signed_requests() -> None:
    body = b'{"type":"run.completed","data":{"id":"run_1"}}'
    signature = sign_webhook_payload("secret", body)

    verify_webhook_signature("secret", body, signature)
    event = parse_webhook_event(body)
    assert event.type == "run.completed"
    assert event.data["id"] == "run_1"

    signed = parse_signed_webhook_request(
        body,
        {WEBHOOK_SIGNATURE_HEADER: signature},
        "secret",
    )
    assert signed.body == body
    assert signed.event.data["id"] == "run_1"

    with pytest.raises(ValueError):
        verify_webhook_signature("secret", body, "sha256=00")


def test_synthetic_webhook_delivery_posts_signed_envelope() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read()
        seen["event_type"] = request.headers[WEBHOOK_EVENT_TYPE_HEADER]
        seen["signature"] = request.headers[WEBHOOK_SIGNATURE_HEADER]
        return httpx.Response(204)

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        base_url="https://api.example.invalid",
    )

    deliver_synthetic_webhook(
        SyntheticWebhookDelivery(
            url="https://api.example.invalid/webhooks/mobius",
            secret="secret",
            event_type="run.completed",
            data={"id": "run_1"},
            http_client=client,
        )
    )

    assert seen["path"] == "/webhooks/mobius"
    assert seen["event_type"] == "run.completed"
    assert seen["signature"] == sign_webhook_payload("secret", seen["body"])
    assert seen["body"] == build_synthetic_webhook_payload(
        "run.completed",
        {"id": "run_1"},
    )


def test_workflow_helpers_create_update_and_ensure_definitions() -> None:
    requests: list[tuple[str, str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = request.read().decode()
        requests.append((request.method, request.url.path, body))
        if request.method == "POST":
            payload = json.loads(body)
            return httpx.Response(
                201,
                json=_workflow_definition_body(
                    "wf_1",
                    payload["name"],
                    payload["handle"],
                    payload["spec"],
                ),
            )
        if request.method == "PATCH":
            payload = json.loads(body)
            return httpx.Response(
                200,
                json=_workflow_definition_body(
                    "wf_1",
                    payload.get("name", "research"),
                    "research",
                    payload.get("spec", _workflow_spec("research")),
                ),
            )
        if request.url.path.endswith("/workflows/wf_1"):
            return httpx.Response(
                200,
                json=_workflow_definition_body(
                    "wf_1",
                    "research",
                    "research",
                    _workflow_spec("old"),
                ),
            )
        return httpx.Response(
            200,
            json={
                "items": [_workflow_definition_summary_body("wf_1", "research", "research")],
                "has_more": False,
            },
        )

    client = _client_with(handler)
    assert (
        client.create_workflow(
            WorkflowSpec(name="research", steps=[]),
            WorkflowOptions(handle="research"),
        ).id
        == "wf_1"
    )
    assert (
        client.update_workflow(
            "wf_1",
            UpdateWorkflowOptions(
                name="research v2",
                spec=WorkflowSpec(name="research v2", steps=[]),
            ),
        ).name
        == "research v2"
    )
    assert len(client.list_workflows(ListWorkflowsOptions(limit=10)).items) == 1
    result = client.ensure_workflow(
        WorkflowSpec(name="research", steps=[]),
        WorkflowOptions(handle="research"),
    )

    assert result.created is False
    assert result.updated is True
    assert any(method == "PATCH" for method, _, _ in requests)


def _run_body(run_id: str, status: str) -> dict[str, object]:
    return {
        "id": run_id,
        "ephemeral": True,
        "workflow_name": "demo",
        "status": status,
        "path_counts": {
            "total": 1,
            "active": 1 if status == "active" else 0,
            "working": 1 if status == "active" else 0,
            "waiting": 0,
            "completed": 1 if status == "completed" else 0,
            "failed": 1 if status == "failed" else 0,
        },
        "job_counts": {"ready": 0, "scheduled": 0, "claimed": 0},
        "step_counts": {
            "pending": 0,
            "running": 1 if status == "active" else 0,
            "completed": 1 if status == "completed" else 0,
            "failed": 1 if status == "failed" else 0,
            "cancelled": 0,
        },
        "wait_summary": {
            "waiting_paths": 0,
            "kind_counts": {},
            "next_wake_at": None,
            "waiting_on_signal_names": [],
            "interaction_ids": [],
        },
        "errors": [],
        "attempt": 1,
        "created_at": "2026-04-27T00:00:00Z",
        "updated_at": "2026-04-27T00:00:00Z",
    }


def _run_detail_body(run_id: str, status: str) -> dict[str, object]:
    body = _run_body(run_id, status)
    body["paths"] = []
    return body


def _workflow_spec(name: str) -> dict[str, object]:
    return {"name": name, "steps": []}


def _workflow_definition_summary_body(
    workflow_id: str,
    name: str,
    handle: str,
) -> dict[str, object]:
    return {
        "id": workflow_id,
        "name": name,
        "handle": handle,
        "latest_version": 1,
        "created_by": "user_1",
        "created_at": "2026-04-27T00:00:00Z",
        "updated_at": "2026-04-27T00:00:00Z",
    }


def _workflow_definition_body(
    workflow_id: str,
    name: str,
    handle: str,
    spec: dict[str, object],
) -> dict[str, object]:
    return {
        **_workflow_definition_summary_body(workflow_id, name, handle),
        "spec": spec,
    }
