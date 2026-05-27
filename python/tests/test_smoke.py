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
    AutomationVersionOptions,
    ListAutomationsOptions,
    UpdateAutomationOptions,
)
from deepnoodle.mobius._api.models import (
    AutomationRunSource,
    AutomationRunStatus,
    AutomationStatus,
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
        if request.method == "POST" and request.url.path.endswith("/automations"):
            payload = json.loads(body)
            return httpx.Response(201, json=_automation_body(handle=payload["handle"], name=payload["name"]))
        if request.method == "PATCH":
            payload = json.loads(body)
            return httpx.Response(200, json=_automation_body(handle="research", name=payload["name"], status=payload["status"]))
        if request.method == "GET" and request.url.path.endswith("/automations"):
            assert request.url.params["status"] == "active"
            return httpx.Response(200, json={"items": [_automation_body()], "has_more": False})
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(404)

    client = _client_with(handler)
    created = client.create_automation(
        AutomationOptions(
            handle="research",
            name="Research",
            default_inputs={"topic": "agents"},
        )
    )
    updated = client.update_automation(
        "research",
        UpdateAutomationOptions(name="Research v2", status=AutomationStatus.active),
    )
    listed = client.list_automations(ListAutomationsOptions(status=AutomationStatus.active))
    client.delete_automation("research")

    assert created.handle == "research"
    assert updated.status is AutomationStatus.active
    assert len(listed.items) == 1
    assert requests[0][0:2] == ("POST", "/v1/projects/test-project/automations")
    assert any(req[0] == "DELETE" and req[1].endswith("/automations/research") for req in requests)


def test_automation_version_helpers_create_and_publish() -> None:
    requests: list[tuple[str, str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = request.read().decode()
        requests.append((request.method, request.url.path, body))
        if request.method == "POST" and request.url.path.endswith("/versions"):
            payload = json.loads(body)
            assert payload["spec"]["steps"] == []
            assert payload["compiled_plan"]["steps"] == []
            return httpx.Response(201, json=_automation_version_body(version=2, status="draft"))
        if request.method == "POST" and request.url.path.endswith("/versions/2/publication"):
            return httpx.Response(200, json=_automation_body(published_version=2))
        if request.method == "GET":
            return httpx.Response(200, json={"items": [_automation_version_body(version=2, status="published")]})
        return httpx.Response(404)

    client = _client_with(handler)
    version = client.create_automation_version(
        "research",
        {"steps": []},
        AutomationVersionOptions(compiled_plan={"steps": []}, publish=True),
    )
    versions = client.list_automation_versions("research")

    assert version.version == 2
    assert len(versions.items) == 1
    assert any(path.endswith("/versions/2/publication") for _, path, _ in requests)


def test_start_run_posts_to_automation_bound_route() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        return httpx.Response(202, json=_run_body("run_1", "running"))

    client = _client_with(handler)
    run = client.start_automation_run(
        "research",
        StartRunOptions(
            external_id="external-1",
            inputs={"topic": "sdk"},
            source=AutomationRunSource(type="api", id="test"),
        ),
    )

    assert run.id == "run_1"
    assert seen["path"] == "/v1/projects/test-project/automations/research/runs"
    assert '"external_id":"external-1"' in str(seen["body"])
    assert '"topic":"sdk"' in str(seen["body"])


def test_run_control_helpers_use_project_scoped_paths_and_enum_query_values() -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        path = request.url.path
        if path.endswith("/runs/run_1/cancellations"):
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
    assert any(url.endswith("/runs/run_1/cancellations") for url in seen)
    assert any(url.endswith("/runs/run_1/signals") for url in seen)


def test_watch_run_parses_automation_run_event_stream() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith("/runs/run_1/events.stream")
        assert request.url.params["since_sequence"] == "7"
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
    handle: str = "research",
    name: str = "Research",
    status: str = "active",
    published_version: int | None = 1,
) -> dict[str, object]:
    return {
        "id": "auto_1",
        "org_id": "org_1",
        "project_id": "test-project",
        "handle": handle,
        "name": name,
        "status": status,
        "latest_version": 1,
        "published_version": published_version,
        "triggers": [],
        "created_at": "2026-05-27T00:00:00Z",
        "updated_at": "2026-05-27T00:00:00Z",
    }


def _automation_version_body(*, version: int, status: str) -> dict[str, object]:
    return {
        "id": f"autov_{version}",
        "org_id": "org_1",
        "project_id": "test-project",
        "automation_id": "auto_1",
        "version": version,
        "status": status,
        "spec": {"steps": []},
        "created_at": "2026-05-27T00:00:00Z",
    }


def _run_body(run_id: str, status: str) -> dict[str, object]:
    return {
        "id": run_id,
        "org_id": "org_1",
        "project_id": "test-project",
        "automation_id": "auto_1",
        "automation_version_id": "autov_1",
        "automation_version": 1,
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
        "org_id": "org_1",
        "project_id": "test-project",
        "run_id": "run_1",
        "sequence": sequence,
        "event_type": event_type,
        "payload": payload,
        "created_at": "2026-05-27T00:00:00Z",
    }
