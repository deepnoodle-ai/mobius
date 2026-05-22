"""Tests for the high-level DiscussionsClient helper.

Mirrors the Go test ``TestStartDiscussionCreatesChannelInteractionAndOpeningMessage``:
exercises the network sequence end-to-end against an ``httpx.MockTransport``,
locking the call order and the channel-create body so the three SDKs stay in
sync.
"""

from __future__ import annotations

import json
from typing import Any

import httpx
import pytest

from deepnoodle.mobius.client import (
    Client,
    ClientOptions,
    StartDiscussionOptions,
    WaitDiscussionOptions,
)
from deepnoodle.mobius._api.models import (
    CreateStandaloneInteractionRequest,
)


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


_NOW = "2026-05-17T00:00:00Z"


def _interaction_json(id_: str, status: str) -> dict[str, Any]:
    return {
        "id": id_,
        "kind": "review",
        "status": status,
        "title": "Review",
        "target_user_ids": ["usr_1"],
        "created_at": _NOW,
        "updated_at": _NOW,
    }


def _channel_json(id_: str) -> dict[str, Any]:
    return {
        "id": id_,
        "name": "incident-review",
        "display_name": "Incident Review",
        "kind": "channel",
        "private": True,
        "created_by": "usr_creator",
        "purpose": "resolve_interactions",
        "completion_behavior": "none",
        "agent_instructions": "",
        "agent_instructions_version": 0,
        "default_notification_level": "mentions",
        "created_at": _NOW,
        "updated_at": _NOW,
    }


def _message_json(id_: str, channel_id: str) -> dict[str, Any]:
    return {
        "id": id_,
        "channel_id": channel_id,
        "sender_type": "service_account",
        "sender_id": "svc_1",
        "display": "message",
        "type": "user.message",
        "content": "Please resolve this together.",
        "created_at": _NOW,
    }


def test_start_discussion_creates_channel_interaction_and_opening_message() -> None:
    calls: list[tuple[str, str, dict[str, Any]]] = []

    def handler(req: httpx.Request) -> httpx.Response:
        body: dict[str, Any] = {}
        if req.content:
            body = json.loads(req.content)
        calls.append((req.method, req.url.path, body))
        if req.method == "POST" and req.url.path == "/v1/projects/test-project/interactions":
            return httpx.Response(201, json=_interaction_json("iact_created", "pending"))
        if req.method == "POST" and req.url.path == "/v1/projects/test-project/channels":
            return httpx.Response(201, json=_channel_json("chn_1"))
        if (
            req.method == "POST"
            and req.url.path == "/v1/projects/test-project/channels/chn_1/messages"
        ):
            return httpx.Response(201, json=_message_json("msg_1", "chn_1"))
        if req.method == "GET" and req.url.path.startswith(
            "/v1/projects/test-project/interactions/"
        ):
            interaction_id = req.url.path.rsplit("/", 1)[-1]
            return httpx.Response(200, json=_interaction_json(interaction_id, "completed"))
        return httpx.Response(404)

    client = _client_with(handler)
    result = client.start_discussion(
        StartDiscussionOptions(
            name="incident-review",
            display_name="Incident Review",
            opening_message="Please resolve this together.",
            associated_interaction_ids=["iact_existing"],
            interactions=[
                CreateStandaloneInteractionRequest(
                    kind="review",
                    title="Review the incident notes",
                    target_user_ids=["usr_1"],
                )
            ],
            wait=WaitDiscussionOptions(timeout=1.0, poll_interval=0.001),
        )
    )

    assert result.channel_id == "chn_1"
    assert result.opening_message_id == "msg_1"
    assert result.interaction_ids == ["iact_existing", "iact_created"]
    assert result.created_interaction_ids == ["iact_created"]
    assert result.outcomes is not None and len(result.outcomes) == 2

    # 1 createInteraction + 1 createChannel (with embedded
    # associated_interaction_ids) + 1 sendMessage + 2 GET interaction polls.
    assert len(calls) == 5
    methods_paths = [(m, p) for m, p, _ in calls]
    assert methods_paths[0] == ("POST", "/v1/projects/test-project/interactions")
    assert methods_paths[1] == ("POST", "/v1/projects/test-project/channels")
    assert methods_paths[2] == (
        "POST",
        "/v1/projects/test-project/channels/chn_1/messages",
    )
    channel_body = calls[1][2]
    assert channel_body["purpose"] == "resolve_interactions"
    assert channel_body["associated_interaction_ids"] == ["iact_existing", "iact_created"]
    message_body = calls[2][2]
    assert message_body["references"][0]["entity_type"] == "interaction"


def test_start_discussion_cancels_created_interactions_when_setup_fails() -> None:
    cancel_body: dict[str, Any] = {}

    def handler(req: httpx.Request) -> httpx.Response:
        nonlocal cancel_body
        if req.method == "POST" and req.url.path == "/v1/projects/test-project/interactions":
            return httpx.Response(201, json=_interaction_json("iact_created", "pending"))
        if req.method == "POST" and req.url.path == "/v1/projects/test-project/channels":
            return httpx.Response(201, json=_channel_json("chn_1"))
        if (
            req.method == "POST"
            and req.url.path == "/v1/projects/test-project/channels/chn_1/messages"
        ):
            return httpx.Response(500, json={"error": {"message": "boom"}})
        if (
            req.method == "POST"
            and req.url.path
            == "/v1/projects/test-project/interactions/iact_created/cancel"
        ):
            cancel_body = json.loads(req.content) if req.content else {}
            return httpx.Response(200, json=_interaction_json("iact_created", "cancelled"))
        return httpx.Response(404)

    client = _client_with(handler)
    with pytest.raises(httpx.HTTPStatusError):
        client.start_discussion(
            StartDiscussionOptions(
                name="rollback-review",
                opening_message="This will fail.",
                interactions=[
                    CreateStandaloneInteractionRequest(
                        kind="review",
                        title="Review the setup",
                        target_user_ids=["usr_1"],
                    )
                ],
            )
        )
    assert cancel_body.get("reason") == "discussion_start_failed"


def test_start_discussion_requires_opening_message() -> None:
    client = _client_with(lambda _: httpx.Response(404))
    with pytest.raises(ValueError):
        client.start_discussion(
            StartDiscussionOptions(
                name="x",
                opening_message="",
                associated_interaction_ids=["iact_existing"],
            )
        )


def test_start_discussion_requires_at_least_one_interaction() -> None:
    client = _client_with(lambda _: httpx.Response(404))
    with pytest.raises(ValueError):
        client.start_discussion(
            StartDiscussionOptions(name="x", opening_message="hello")
        )
