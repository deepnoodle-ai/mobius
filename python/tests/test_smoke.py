"""Smoke tests for the hand-written Python Client wrapper."""

from __future__ import annotations

import json

import httpx
import pytest

from deepnoodle.mobius import (
    DEFAULT_BASE_URL,
    MOBIUS_DELIVERY_ID_HEADER,
    MOBIUS_SECRET_REF_HEADER,
    MOBIUS_SECRET_VERSION_HEADER,
    MOBIUS_SIGNATURE_HEADER,
    MOBIUS_SIGNATURE_VERSION_HEADER,
    MOBIUS_TIMESTAMP_HEADER,
    WEBHOOK_EVENT_TYPE_HEADER,
    Client,
    ClientOptions,
    ListRunsOptions,
    StartRunOptions,
    SyntheticWebhookDelivery,
    WaitRunOptions,
    build_synthetic_webhook_payload,
    deliver_synthetic_webhook,
    is_terminal_run_status,
    parse_webhook_delivery,
    sign_delivery,
    verify_signed_delivery,
)
from deepnoodle.mobius.client import (
    AutomationOptions,
    ListAutomationsOptions,
    UpdateAutomationOptions,
)
from deepnoodle.mobius._api.models import (
    LoopRunSource as AutomationRunSource,
    LoopRunStatus as AutomationRunStatus,
    LoopStatus as AutomationStatus,
)


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
        ),
        transport=httpx.MockTransport(handler),
    )


def test_client_defaults_to_production_api_host() -> None:
    client = Client("mbx_test")
    assert client.base_url == DEFAULT_BASE_URL
    client.close()


def test_api_key_project_suffix_is_used_and_conflicts_are_rejected() -> None:
    client = Client("mbx_secret.test-project")
    assert client.project == "test-project"
    client.close()

    with pytest.raises(ValueError):
        Client("mbx_secret.test-project", project="other-project")


def test_worker_socket_url_uses_project_scoped_websocket_route() -> None:
    client = Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="http://localhost:8080/api",
            project="test-project",
        )
    )
    assert client.worker_socket_url() == "ws://localhost:8080/api/v1/projects/test-project/workers/socket"
    client.close()


def test_automation_helpers_use_project_scoped_routes() -> None:
    requests: list[tuple[str, str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = request.read().decode()
        requests.append((request.method, request.url.path, body))
        if request.method == "POST" and request.url.path.endswith("/loops"):
            payload = json.loads(body)
            return httpx.Response(201, json=_automation_body(name=payload["name"]))
        if request.method == "PATCH":
            payload = json.loads(body)
            return httpx.Response(200, json=_automation_body(name=payload["name"], status=payload["status"]))
        if request.method == "GET" and request.url.path.endswith("/loops"):
            assert request.url.params["status"] == "active"
            return httpx.Response(200, json={"items": [_automation_body()], "has_more": False})
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(404)

    client = _client_with(handler)
    created = client.create_automation(
        AutomationOptions(
            name="Research",
            default_inputs={"topic": "agents"},
        )
    )
    updated = client.update_automation(
        "loop_1",
        UpdateAutomationOptions(name="Research v2", status=AutomationStatus.active),
    )
    listed = client.list_automations(ListAutomationsOptions(status=AutomationStatus.active))
    client.delete_automation("loop_1")

    assert created.name == "Research"
    assert updated.status is AutomationStatus.active
    assert len(listed.items) == 1
    assert requests[0][0:2] == ("POST", "/v1/projects/test-project/loops")
    assert any(req[0] == "DELETE" and req[1].endswith("/loops/loop_1") for req in requests)


def test_create_automation_sends_inline_spec() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and request.url.path.endswith("/loops"):
            captured["payload"] = json.loads(request.read().decode())
            return httpx.Response(201, json=_automation_body(name="Research"))
        return httpx.Response(404)

    client = _client_with(handler)
    created = client.create_automation(
        AutomationOptions(
            name="Research",
            agent_id="agent_1",
            spec={
                "schema_version": "2",
                "steps": [{"kind": "agent", "config": {"instructions": "do research"}}],
            },
        )
    )

    payload = captured["payload"]
    assert created.name == "Research"
    # Explicit fields and the inline spec are both flattened onto the request.
    assert payload["name"] == "Research"
    assert payload["agent_id"] == "agent_1"
    assert payload["schema_version"] == "2"
    assert payload["steps"] == [{"kind": "agent", "config": {"instructions": "do research"}}]


def test_start_run_posts_to_automation_bound_route() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(202, json=_run_body("run_1", "running"))

    client = _client_with(handler)
    run = client.start_automation_run(
        "loop_1",
        StartRunOptions(
            external_id="external-1",
            inputs={"topic": "sdk"},
            source=AutomationRunSource(type="api", id="test"),
        ),
    )

    assert run.id == "run_1"
    assert seen["path"] == "/v1/projects/test-project/loops/loop_1/runs"
    assert '"idempotency_key":"external-1"' in str(seen["body"])
    assert '"topic":"sdk"' in str(seen["body"])


def test_run_control_helpers_use_project_scoped_paths_and_enum_query_values() -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        path = request.url.path
        if path.endswith("/runs/run_1/cancel"):
            return httpx.Response(200, json=_run_body("run_1", "cancelled"))
        if path.endswith("/runs/run_1/signals"):
            return httpx.Response(200, json=_run_body("run_1", "running"))
        if path.endswith("/runs/run_1"):
            return httpx.Response(200, json=_run_body("run_1", "completed"))
        return httpx.Response(200, json={"items": [_run_body("run_1", "completed")], "has_more": False})

    client = _client_with(handler)
    assert client.get_run("run_1").status is AutomationRunStatus.completed
    assert len(client.list_runs(ListRunsOptions(status=AutomationRunStatus.completed)).items) == 1
    assert client.cancel_run("run_1", reason="test").status is AutomationRunStatus.cancelled
    assert client.signal_run("run_1", "approval", {"ok": True}).id == "run_1"

    assert any("/runs?status=completed" in url for url in seen)
    assert any(url.endswith("/runs/run_1/cancel") for url in seen)
    assert any(url.endswith("/runs/run_1/signals") for url in seen)


def test_watch_run_parses_automation_run_event_stream() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith("/runs/run_1/events.stream")
        assert request.url.params["after_sequence"] == "7"
        event = _run_event_body("evt_8", 8, "run.completed", {"status": "completed"})
        return httpx.Response(
            200,
            text=f"event: run.completed\ndata: {json.dumps(event)}\n\n",
            headers={"Content-Type": "text/event-stream"},
        )

    client = _client_with(handler)
    events = list(client.watch_run("run_1", since=7))

    assert len(events) == 1
    assert events[0].sequence == 8
    assert events[0].event_type == "run.completed"


def test_wait_run_returns_when_initial_fetch_is_terminal() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json=_run_body("run_1", "completed"))

    client = _client_with(handler)
    run = client.wait_run("run_1", WaitRunOptions(timeout=1))

    assert run.status is AutomationRunStatus.completed
    assert is_terminal_run_status(run.status)


def test_signing_helpers_verify_and_parse_webhook_deliveries() -> None:
    body = b'{"type":"run.completed","data":{"id":"run_1"}}'
    key = b"01234567890123456789012345678901"
    signature = sign_delivery(
        key,
        body,
        delivery_id="delivery_1",
        timestamp=1710000000,
    )

    signed = verify_signed_delivery(
        body,
        {
            MOBIUS_SIGNATURE_HEADER: signature,
            MOBIUS_SIGNATURE_VERSION_HEADER: "v1",
            MOBIUS_TIMESTAMP_HEADER: "1710000000",
            MOBIUS_DELIVERY_ID_HEADER: "delivery_1",
            MOBIUS_SECRET_REF_HEADER: "mobius/webhook/test",
            MOBIUS_SECRET_VERSION_HEADER: "2",
        },
        key=key,
        now=lambda: 1710000005,
    )
    event = parse_webhook_delivery(signed)
    assert event["type"] == "run.completed"
    assert event["data"]["id"] == "run_1"
    assert signed.body == body

    with pytest.raises(ValueError):
        verify_signed_delivery(
            body,
            {
                MOBIUS_SIGNATURE_HEADER: "sha256=00",
                MOBIUS_SIGNATURE_VERSION_HEADER: "v1",
                MOBIUS_TIMESTAMP_HEADER: "1710000000",
                MOBIUS_DELIVERY_ID_HEADER: "delivery_1",
                MOBIUS_SECRET_REF_HEADER: "mobius/webhook/test",
                MOBIUS_SECRET_VERSION_HEADER: "2",
            },
            key=key,
            now=lambda: 1710000005,
        )


def test_synthetic_webhook_delivery_posts_signed_envelope() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read()
        seen["event_type"] = request.headers[WEBHOOK_EVENT_TYPE_HEADER]
        seen["signature"] = request.headers[MOBIUS_SIGNATURE_HEADER]
        seen["version"] = request.headers[MOBIUS_SIGNATURE_VERSION_HEADER]
        seen["delivery_id"] = request.headers[MOBIUS_DELIVERY_ID_HEADER]
        return httpx.Response(204)

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        base_url="https://api.example.invalid",
    )

    key = b"01234567890123456789012345678901"
    deliver_synthetic_webhook(
        SyntheticWebhookDelivery(
            url="https://api.example.invalid/webhooks/mobius",
            key=key,
            secret_ref="mobius/webhook/test",
            secret_version=2,
            delivery_id="delivery_2",
            timestamp=1710000000,
            event_type="run.completed",
            data={"id": "run_1"},
            http_client=client,
        )
    )

    assert seen["path"] == "/webhooks/mobius"
    assert seen["event_type"] == "run.completed"
    assert seen["version"] == "v1"
    assert seen["delivery_id"] == "delivery_2"
    assert seen["signature"] == sign_delivery(
        key,
        seen["body"],
        delivery_id="delivery_2",
        timestamp=1710000000,
    )
    assert seen["body"] == build_synthetic_webhook_payload(
        "run.completed",
        {"id": "run_1"},
    )


def _automation_body(
    *,
    name: str = "Research",
    status: str = "active",
) -> dict[str, object]:
    return {
        "id": "loop_1",
        "name": name,
        "status": status,
        "triggers": [],
        "created_at": "2026-05-27T00:00:00Z",
        "updated_at": "2026-05-27T00:00:00Z",
    }


def _run_body(run_id: str, status: str) -> dict[str, object]:
    return {
        "id": run_id,
        "loop_id": "loop_1",
        "loop_version_id": "lver_1",
        "loop_version": 1,
        "status": status,
        "inputs": {"topic": "sdk"},
        "created_at": "2026-05-27T00:00:00Z",
        "updated_at": "2026-05-27T00:00:00Z",
    }


def _run_event_body(
    event_id: str,
    sequence: int,
    event_type: str,
    payload: dict[str, object],
) -> dict[str, object]:
    return {
        "id": event_id,
        "run_id": "run_1",
        "sequence": sequence,
        "event_type": event_type,
        "payload": payload,
        "created_at": "2026-05-27T00:00:00Z",
    }
