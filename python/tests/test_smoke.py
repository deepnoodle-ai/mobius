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
    AgentTurnOperationPolicy,
    Client,
    ClientOptions,
    InvokeAgentOptions,
    ListBlueprintBindingsOptions,
    ListInteractionsOptions,
    ListPrincipalsOptions,
    ListRoleAssignmentsOptions,
    ListRolesOptions,
    ListSessionMessagesOptions,
    ListSessionNudgesOptions,
    ListRunsOptions,
    RuntimeContextItem,
    StartRunOptions,
    StartTurnOptions,
    SyntheticWebhookDelivery,
    TurnOutputSpec,
    WaitRunOptions,
    build_synthetic_webhook_payload,
    deliver_synthetic_webhook,
    is_terminal_run_status,
    parse_webhook_delivery,
    sign_delivery,
    verify_signed_delivery,
)
from deepnoodle.mobius.client import (
    ListLoopsOptions,
    LoopOptions,
    UpdateLoopOptions,
)
from deepnoodle.mobius._api.models import (
    InlineAgentConfig,
    InlineToolkit,
    InvokeSessionSpec,
    LoopRunSource,
    LoopRunStatus,
    LoopStatus,
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


def test_loop_helpers_use_project_scoped_routes() -> None:
    requests: list[tuple[str, str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = request.read().decode()
        requests.append((request.method, request.url.path, body))
        if request.method == "POST" and request.url.path.endswith("/loops"):
            payload = json.loads(body)
            return httpx.Response(201, json=_loop_body(name=payload["name"]))
        if request.method == "PATCH":
            payload = json.loads(body)
            return httpx.Response(200, json=_loop_body(name=payload["name"], status=payload["status"]))
        if request.method == "GET" and request.url.path.endswith("/loops"):
            assert request.url.params["status"] == "active"
            return httpx.Response(200, json={"items": [_loop_body()], "has_more": False})
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(404)

    client = _client_with(handler)
    created = client.create_loop(
        LoopOptions(
            name="Research",
            default_config={"topic": "agents"},
        )
    )
    updated = client.update_loop(
        "loop_1",
        UpdateLoopOptions(name="Research v2", status=LoopStatus.active),
    )
    listed = client.list_loops(ListLoopsOptions(status=LoopStatus.active))
    client.delete_loop("loop_1")

    assert created.name == "Research"
    assert updated.status is LoopStatus.active
    assert len(listed.items) == 1
    assert requests[0][0:2] == ("POST", "/v1/projects/test-project/loops")
    assert any(req[0] == "DELETE" and req[1].endswith("/loops/loop_1") for req in requests)


def test_project_resource_list_helpers() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path.endswith("/blueprints/bindings"):
            assert request.url.params["namespace"] == "starter"
            assert request.url.params["blueprint_key"] == "support"
            return httpx.Response(200, json={"items": []})
        if path.endswith("/interactions"):
            assert request.url.params["session_id"] == "sess_1"
            assert request.url.params["status"] == "pending"
            assert request.url.params["inbox"] == "true"
            return httpx.Response(200, json={"items": [], "has_more": False})
        if path.endswith("/permissions"):
            return httpx.Response(
                200, json={"items": [], "presets": [], "action_groups": []}
            )
        if path.endswith("/principals"):
            assert request.url.params["kind"] == "service"
            assert request.url.params["include_disabled"] == "true"
            return httpx.Response(200, json={"items": []})
        if path.endswith("/roles"):
            assert request.url.params["cursor"] == "role_cursor"
            return httpx.Response(200, json={"items": [], "has_more": False})
        if path.endswith("/role-assignments"):
            assert request.url.params["principal_id"] == "principal_1"
            return httpx.Response(200, json={"items": []})
        return httpx.Response(404)

    client = _client_with(handler)
    assert client.list_blueprint_bindings(
        ListBlueprintBindingsOptions(namespace="starter", blueprint_key="support")
    ).items == []
    assert client.list_interactions(
        ListInteractionsOptions(status="pending", session_id="sess_1", inbox=True)
    ).items == []
    assert client.list_project_permissions().items == []
    assert client.list_principals(
        ListPrincipalsOptions(kind="service", include_disabled=True)
    ).items == []
    assert client.list_roles(ListRolesOptions(cursor="role_cursor")).items == []
    assert client.list_role_assignments(
        ListRoleAssignmentsOptions(principal_id="principal_1")
    ).items == []


def test_create_loop_sends_inline_spec() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and request.url.path.endswith("/loops"):
            captured["payload"] = json.loads(request.read().decode())
            return httpx.Response(201, json=_loop_body(name="Research"))
        return httpx.Response(404)

    client = _client_with(handler)
    created = client.create_loop(
        LoopOptions(
            name="Research",
            agent_id="agent_1",
            spec={
                "schema_version": "1",
                "steps": [{"kind": "agent", "config": {"instructions": "do research"}}],
            },
        )
    )

    payload = captured["payload"]
    assert created.name == "Research"
    # Explicit fields and the inline spec are both flattened onto the request.
    assert payload["name"] == "Research"
    assert payload["agent_id"] == "agent_1"
    assert payload["schema_version"] == "1"
    assert payload["steps"] == [{"kind": "agent", "config": {"instructions": "do research"}}]


def test_start_run_posts_to_loop_bound_route() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        seen["idempotency_key"] = request.headers.get("Idempotency-Key")
        return httpx.Response(202, json=_run_body("run_1", "running"))

    client = _client_with(handler)
    run = client.start_run(
        "loop_1",
        StartRunOptions(
            idempotency_key="run-request-1",
            event={"topic": "sdk"},
            config={"priority": "normal"},
            source=LoopRunSource(type="api", id="test"),
        ),
    )

    assert run.id == "run_1"
    assert seen["path"] == "/v1/projects/test-project/loops/loop_1/runs"
    assert seen["idempotency_key"] == "run-request-1"
    assert '"idempotency_key":"run-request-1"' in str(seen["body"])
    assert '"event":{"topic":"sdk"}' in str(seen["body"])
    assert '"config":{"priority":"normal"}' in str(seen["body"])


def test_start_run_keeps_external_id_as_deprecated_alias() -> None:
    seen: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = request.read().decode()
        return httpx.Response(202, json=_run_body("run_1", "running"))

    client = _client_with(handler)
    client.start_run("loop_1", StartRunOptions(external_id="legacy-1"))
    assert '"idempotency_key":"legacy-1"' in seen["body"]

    with pytest.raises(ValueError, match="must match"):
        client.start_run(
            "loop_1",
            StartRunOptions(idempotency_key="canonical", external_id="legacy"),
        )


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
    assert client.get_run("run_1").status is LoopRunStatus.completed
    assert len(client.list_runs(ListRunsOptions(status=LoopRunStatus.completed)).items) == 1
    assert client.cancel_run("run_1", reason="test").status is LoopRunStatus.cancelled
    assert client.signal_run("run_1", "approval", {"ok": True}).id == "run_1"

    assert any("/runs?status=completed" in url for url in seen)
    assert any(url.endswith("/runs/run_1/cancel") for url in seen)
    assert any(url.endswith("/runs/run_1/signals") for url in seen)


def test_invoke_agent_posts_the_compound_invoke_request_shape() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = request.read().decode()
        seen["idempotency_key"] = request.headers.get("Idempotency-Key")
        return httpx.Response(202, json=_turn_ack_body("sess_1", "turn_1", 7))

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(
            agent_id="agent_1",
            content=[{"type": "text", "text": "hi"}],
            context=[RuntimeContextItem(name="naming-board", content="Chosen: none")],
            idempotency_key="evt_1",
            session=InvokeSessionSpec(session_key="app:acct_1:user_2"),
            config=InlineAgentConfig(
                instructions="Be concise.",
                model="claude-sonnet-4-6",
                effort="medium",
                toolkits=[InlineToolkit(name="tickets", actions=["tickets.search"])],
            ),
            operation=AgentTurnOperationPolicy(timeout_seconds=90),
            output=TurnOutputSpec(schema={"type": "object"}),
        )
    )

    assert turn.after_sequence == 7
    assert turn.session_id == "sess_1"
    assert turn.id == "turn_1"
    assert turn.deduped is False
    assert seen["path"] == "/v1/projects/test-project/agents/invoke"
    assert seen["idempotency_key"] == "evt_1"
    assert '"agent_ref":{"id":"agent_1"}' in str(seen["body"])
    assert '"idempotency_key":"evt_1"' in str(seen["body"])
    assert '"context":[{"name":"naming-board","content":"Chosen: none"}]' in str(
        seen["body"]
    )
    assert '"session_key":"app:acct_1:user_2"' in str(seen["body"])
    assert '"instructions":"Be concise."' in str(seen["body"])
    assert '"model":"claude-sonnet-4-6"' in str(seen["body"])
    assert '"effort":"medium"' in str(seen["body"])
    assert '"toolkits":[{"name":"tickets","actions":["tickets.search"]}]' in str(seen["body"])
    assert '"operation":{"timeout_seconds":90}' in str(seen["body"])
    # The schema field is aliased off the python-reserved name; it must
    # serialize under its wire name "schema", not "schema_".
    assert '"output":{"schema":{"type":"object"}}' in str(seen["body"])


def test_start_turn_passes_runtime_context_to_existing_session() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = json.loads(request.read())
        seen["idempotency_key"] = request.headers.get("Idempotency-Key")
        return httpx.Response(202, json=_turn_ack_body("sess_1", "turn_1", 7))

    client = _client_with(handler)
    turn = client.start_turn(
        "sess_1",
        StartTurnOptions(
            content=[{"type": "text", "text": "hi"}],
            context=[RuntimeContextItem(name="naming-board", content="Chosen: none")],
            idempotency_key="evt_1",
            operation=AgentTurnOperationPolicy(timeout_seconds=45),
            output=TurnOutputSpec(schema={"type": "object"}),
            metadata={"source": "app"},
        ),
    )

    assert turn.id == "turn_1"
    assert seen["path"] == "/v1/projects/test-project/sessions/sess_1/turns"
    assert seen["idempotency_key"] == "evt_1"
    assert seen["body"] == {
        "role": "user",
        "content": [{"type": "text", "text": "hi"}],
        "context": [{"name": "naming-board", "content": "Chosen: none"}],
        "idempotency_key": "evt_1",
        "operation": {"timeout_seconds": 45},
        "output": {"schema": {"type": "object"}},
        "metadata": {"source": "app"},
    }


def test_list_session_messages_can_include_runtime_context() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith("/sessions/sess_1/messages")
        assert request.url.params["include"] == "context"
        return httpx.Response(
            200,
            json={
                "items": [
                    {
                        "id": "msg_1",
                        "session_id": "sess_1",
                        "agent_id": "agent_1",
                        "role": "system",
                        "content": [
                            {
                                "type": "reminder",
                                "name": "app-board",
                                "tier": "contextual",
                                "content": "Chosen: none",
                            }
                        ],
                        "entry_type": "message",
                        "sequence": 1,
                        "created_at": "2026-07-14T12:00:00Z",
                    }
                ]
            },
        )

    client = _client_with(handler)
    messages = client.list_session_messages(
        "sess_1", ListSessionMessagesOptions(include="context")
    )

    reminder = messages.items[0].content[0].root
    assert reminder.name == "app-board"
    assert reminder.content == "Chosen: none"


def test_invoke_agent_mode_new_is_not_marked_replay_safe() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.read())
        seen["idempotency_key"] = request.headers.get("Idempotency-Key")
        return httpx.Response(202, json=_turn_ack_body("sess_1", "turn_1", 7))

    client = _client_with(handler)
    client.invoke_agent(
        InvokeAgentOptions(
            agent_name="support",
            content=[{"type": "text", "text": "hi"}],
            idempotency_key="evt_1",
            session=InvokeSessionSpec(mode="new"),
        )
    )

    assert seen["body"]["input"]["idempotency_key"] == "evt_1"
    assert seen["idempotency_key"] is None


def test_invoke_agent_whitespace_idempotency_key_is_omitted() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.read())
        seen["idempotency_key"] = request.headers.get("Idempotency-Key")
        return httpx.Response(202, json=_turn_ack_body("sess_1", "turn_1", 7))

    client = _client_with(handler)
    client.invoke_agent(
        InvokeAgentOptions(
            agent_name="support",
            content=[{"type": "text", "text": "hi"}],
            idempotency_key="  \t  ",
        )
    )

    assert "idempotency_key" not in seen["body"]["input"]
    assert seen["idempotency_key"] is None


def test_session_nudge_lifecycle_routes() -> None:
    seen: list[str] = []
    queued = {
        "id": "nudge_1",
        "status": "pending",
        "delivery": "current_turn",
        "content": "Use the shorter name",
        "turn": {"id": "turn_1", "status": "waiting"},
        "sender_principal_id": "principal_1",
        "created_at": "2026-07-14T12:00:00Z",
    }

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(f"{request.method} {request.url.path}")
        if request.url.path.endswith("/nudges"):
            assert request.url.params["status"] == "pending"
            assert request.url.params["order"] == "desc"
            return httpx.Response(200, json={"items": [queued], "has_more": False})
        if request.url.path.endswith("/cancel"):
            return httpx.Response(
                200,
                json={
                    **queued,
                    "status": "cancelled",
                    "cancelled_at": "2026-07-14T12:01:00Z",
                },
            )
        return httpx.Response(200, json=queued)

    client = _client_with(handler)
    page = client.list_session_nudges(
        "s1", ListSessionNudgesOptions(status=["pending"], order="desc")
    )
    assert page.items[0].id == "nudge_1"
    assert client.get_session_nudge("s1", "nudge_1").status == "pending"
    assert client.cancel_nudge("s1", "nudge_1").status == "cancelled"
    assert seen == [
        "GET /v1/projects/test-project/sessions/s1/nudges",
        "GET /v1/projects/test-project/sessions/s1/nudges/nudge_1",
        "POST /v1/projects/test-project/sessions/s1/nudges/nudge_1/cancel",
    ]


def test_invoke_agent_requires_agent_ref_and_content() -> None:
    client = _client_with(lambda _: httpx.Response(404))

    with pytest.raises(ValueError):
        client.invoke_agent(InvokeAgentOptions(content=[{"type": "text", "text": "hi"}]))
    with pytest.raises(ValueError):
        client.invoke_agent(InvokeAgentOptions(agent_id="agent_1", content=[]))


def test_invoke_agent_stream_streams_session_frames_inline() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers["accept"] == "text/event-stream"
        assert request.headers["idempotency-key"] == "evt_stream_1"
        return httpx.Response(
            200,
            text='event: turn.completed\ndata: {"usage":{"input_tokens":42}}\n\n',
            headers={"Content-Type": "text/event-stream"},
        )

    client = _client_with(handler)
    events = list(
        client.invoke_agent_stream(
            InvokeAgentOptions(
                agent_name="support",
                content=[{"type": "text", "text": "hi"}],
                idempotency_key="evt_stream_1",
            )
        )
    )

    assert len(events) == 1
    assert events[0].event_type == "turn.completed"
    assert events[0].data == {"usage": {"input_tokens": 42}}


def test_watch_run_parses_loop_run_event_stream() -> None:
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

    assert run.status is LoopRunStatus.completed
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


def _loop_body(
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
        "event": {"topic": "sdk"},
        "created_at": "2026-05-27T00:00:00Z",
        "updated_at": "2026-05-27T00:00:00Z",
    }


def _turn_ack_body(session_id: str, turn_id: str, after_sequence: int) -> dict[str, object]:
    return {
        "after_sequence": after_sequence,
        "resume_cursor": "41.6",
        "session": {
            "id": session_id,
            "agent_id": "agent_1",
            "origin": "api",
            "scope": "agent",
            "scope_name": "app:acct_1:user_2",
            "scope_ref_id": "agent_1",
            "session_key": "app:acct_1:user_2",
            "status": "active",
            "title": "",
            "visibility": "private",
            "version": 1,
            "message_count": 1,
            "token_input_total": 0,
            "cache_read_input_total": 0,
            "cache_creation_input_total": 0,
            "token_output_total": 0,
            "created_at": "2026-05-27T00:00:00Z",
            "updated_at": "2026-05-27T00:00:00Z",
        },
        "turn": {
            "id": turn_id,
            "agent_id": "agent_1",
            "session_id": session_id,
            "attempt": 1,
            "status": "running",
            "created_at": "2026-05-27T00:00:00Z",
            "updated_at": "2026-05-27T00:00:00Z",
        },
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
