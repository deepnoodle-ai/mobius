"""Route-level tests for the organization action admin surface."""

from __future__ import annotations

import base64
import json

import httpx
import pytest

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    CreateOrganizationActionRequest,
    ListOrganizationActionsOptions,
    MobiusAPIError,
    UpdateOrganizationActionRequest,
)


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
            retry=0,
        ),
        transport=httpx.MockTransport(handler),
    )


def _action(secret: str | None = None, statuses: tuple[str, ...] = ("active",)) -> dict:
    body = {
        "id": "act_1",
        "name": "crm.sync",
        "endpoint_url": "https://example.com/hook",
        "invocation_format": "signed_context_v1",
        "enabled": True,
        "secret_ref": "osec_abc",
        "secret_versions": [
            {
                "version": i + 1,
                "status": status,
                "created_at": "2026-07-17T00:00:00Z",
            }
            for i, status in enumerate(statuses)
        ],
        "created_at": "2026-07-17T00:00:00Z",
        "updated_at": "2026-07-17T00:00:00Z",
    }
    if secret is not None:
        body["signing_secret"] = secret
    return body


def test_create_returns_decoded_secret_material() -> None:
    key = b"super-secret-signing-key-32bytes"
    encoded = base64.b64encode(key).decode()
    seen: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "POST"
        assert request.url.path == "/v1/organization/actions"
        seen.append(json.loads(request.content))
        return httpx.Response(201, json=_action(secret=encoded))

    client = _client_with(handler)
    material = client.create_organization_action(
        CreateOrganizationActionRequest(
            name="crm.sync", endpoint_url="https://example.com/hook"
        )
    )
    client.close()

    assert seen[0]["name"] == "crm.sync"
    assert material.key_bytes == key
    assert material.secret_ref == "osec_abc"
    assert material.version == 1
    assert material.action.signing_secret is None


def test_rotate_identifies_pending_version() -> None:
    encoded = base64.b64encode(b"rotated-key").decode()

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/v1/organization/actions/act_1/secret/rotate"
        return httpx.Response(
            200, json=_action(secret=encoded, statuses=("active", "pending"))
        )

    client = _client_with(handler)
    material = client.rotate_organization_action_secret("act_1")
    client.close()

    assert material.version == 2
    assert material.key_bytes == b"rotated-key"


@pytest.mark.parametrize(
    ("body", "match"),
    [
        (_action(secret=None), "missing the one-time signing_secret"),
        (
            _action(secret=base64.b64encode(b"key").decode(), statuses=("active", "revoked")),
            "status 'revoked', want 'active'",
        ),
        (_action(secret="not_base64!!"), "not valid base64"),
    ],
)
def test_create_rejects_inconsistent_responses(body: dict, match: str) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(201, json=body)

    client = _client_with(handler)
    with pytest.raises(ValueError, match=match):
        client.create_organization_action(
            CreateOrganizationActionRequest(
                name="crm.sync", endpoint_url="https://example.com/hook"
            )
        )
    client.close()


def test_admin_routes() -> None:
    seen: list[tuple[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.method, request.url.path))
        if request.method == "GET" and request.url.path == "/v1/organization/actions":
            assert request.url.params["limit"] == "10"
            return httpx.Response(200, json={"items": [_action()], "has_more": False})
        if request.method == "GET":
            return httpx.Response(200, json=_action())
        if request.method == "PATCH":
            assert json.loads(request.content) == {"enabled": False}
            return httpx.Response(200, json=_action())
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(404)

    client = _client_with(handler)
    page = client.list_organization_actions(ListOrganizationActionsOptions(limit=10))
    assert len(page.items) == 1
    client.get_organization_action("act_1")
    client.update_organization_action(
        "act_1", UpdateOrganizationActionRequest(enabled=False)
    )
    client.delete_organization_action("act_1")
    client.close()

    assert [p for _, p in seen[1:]] == ["/v1/organization/actions/act_1"] * 3
    assert [m for m, _ in seen[1:]] == ["GET", "PATCH", "DELETE"]


def test_activate_sends_explicit_zero_overlap() -> None:
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert (
            request.url.path
            == "/v1/organization/actions/act_1/secret/versions/2/activate"
        )
        bodies.append(json.loads(request.content))
        return httpx.Response(200, json=_action(statuses=("retiring", "active")))

    client = _client_with(handler)
    client.activate_organization_action_secret_version(
        "act_1", 2, overlap_seconds=0
    )
    client.close()

    assert bodies == [{"overlap_seconds": 0}]


def test_revoke_surfaces_conflict() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert (
            request.url.path
            == "/v1/organization/actions/act_1/secret/versions/1/revoke"
        )
        return httpx.Response(
            409,
            json={
                "error": {
                    "code": "secret_version_active",
                    "message": "activate another version first",
                }
            },
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as err:
        client.revoke_organization_action_secret_version("act_1", 1)
    client.close()
    assert err.value.status == 409
    assert err.value.code == "secret_version_active"
