"""Smoke tests for the hand-written Python Client wrapper."""

from __future__ import annotations

import json

import httpx
import pytest

from deepnoodle.mobius import (
    DEFAULT_BASE_URL,
    MOBIUS_DELIVERY_ID_HEADER,
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
    RuntimeContextItem,
    StartTurnOptions,
    SyntheticWebhookDelivery,
    TurnOutputSpec,
    build_synthetic_webhook_payload,
    deliver_synthetic_webhook,
    parse_webhook_delivery,
    sign_delivery,
    verify_signed_delivery,
)
from deepnoodle.mobius._api.models import (
    InvokeSessionSpec,
)


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
        ),
        transport=httpx.MockTransport(handler),
    )


def test_client_defaults_to_production_api_host() -> None:
    client = Client("mbx_test")
    assert client.base_url == DEFAULT_BASE_URL
    client.close()


def test_worker_socket_url_uses_websocket_route() -> None:
    client = Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="http://localhost:8080/api",
        )
    )
    assert client.worker_socket_url() == "ws://localhost:8080/api/v1/workers/socket"
    client.close()


def test_org_resource_list_helpers() -> None:
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
    assert client.list_org_permissions().items == []
    assert client.list_principals(
        ListPrincipalsOptions(kind="service", include_disabled=True)
    ).items == []
    assert client.list_roles(ListRolesOptions(cursor="role_cursor")).items == []
    assert client.list_role_assignments(
        ListRoleAssignmentsOptions(principal_id="principal_1")
    ).items == []


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
            session=InvokeSessionSpec(
                session_key="app:acct_1:user_2", model_override="claude-sonnet-5"
            ),
            operation=AgentTurnOperationPolicy(timeout_seconds=90),
            output=TurnOutputSpec(schema={"type": "object"}),
        )
    )

    assert turn.after_sequence == 7
    assert turn.session_id == "sess_1"
    assert turn.id == "turn_1"
    assert turn.deduped is False
    assert seen["path"] == "/v1/agents/invoke"
    assert seen["idempotency_key"] == "evt_1"
    assert '"agent_ref":{"id":"agent_1"}' in str(seen["body"])
    assert '"idempotency_key":"evt_1"' in str(seen["body"])
    assert '"context":[{"name":"naming-board","content":"Chosen: none"}]' in str(
        seen["body"]
    )
    assert '"session_key":"app:acct_1:user_2"' in str(seen["body"])
    assert '"model_override":"claude-sonnet-5"' in str(seen["body"])
    # The stored agent is the sole definition authority: model_override above is
    # the one execution override an invocation carries, so there is no inline
    # config alongside it.
    assert '"config"' not in str(seen["body"])
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
    assert seen["path"] == "/v1/sessions/sess_1/turns"
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
        "GET /v1/sessions/s1/nudges",
        "GET /v1/sessions/s1/nudges/nudge_1",
        "POST /v1/sessions/s1/nudges/nudge_1/cancel",
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


def test_signing_helpers_verify_and_parse_webhook_deliveries() -> None:
    body = b'{"type":"ping","data":{"id":"run_1"}}'
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
            MOBIUS_SECRET_VERSION_HEADER: "2",
        },
        key=key,
        now=lambda: 1710000005,
    )
    event = parse_webhook_delivery(signed)
    assert event["type"] == "ping"
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
            secret_version=2,
            delivery_id="delivery_2",
            timestamp=1710000000,
            event_type="ping",
            data={"id": "run_1"},
            http_client=client,
        )
    )

    assert seen["path"] == "/webhooks/mobius"
    assert seen["event_type"] == "ping"
    assert seen["version"] == "v1"
    assert seen["delivery_id"] == "delivery_2"
    assert seen["signature"] == sign_delivery(
        key,
        seen["body"],
        delivery_id="delivery_2",
        timestamp=1710000000,
    )
    assert seen["body"] == build_synthetic_webhook_payload(
        "ping",
        {"id": "run_1"},
    )


def _turn_ack_body(session_id: str, turn_id: str, after_sequence: int) -> dict[str, object]:
    return {
        "after_sequence": after_sequence,
        "resume_cursor": "41.6",
        "session": {
            "id": session_id,
            "owner": {"kind": "team"},
            "agent_id": "agent_1",
            "origin": "api",
            "scope": "agent",
            "scope_name": "app:acct_1:user_2",
            "scope_ref_id": "agent_1",
            "session_key": "app:acct_1:user_2",
            "status": "active",
            "title": "",
            "visibility": "private",
            "posture": "team",
            "version": 1,
            "owner": {"kind": "person", "id": "user_1"},
            "posture": "only_you",
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
