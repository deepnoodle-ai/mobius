"""Route- and retry-level tests for the create-or-adopt surface."""

from __future__ import annotations

import json

import httpx
import pytest

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    CreateAgentRequest,
    CreateProjectRequest,
    MobiusAPIError,
)


def _client_with(handler, *, retry: int = 0) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
            retry=retry,
        ),
        transport=httpx.MockTransport(handler),
    )


def _agent(agent_id: str = "agent_1") -> dict:
    return {
        "id": agent_id,
        "principal_id": agent_id,
        "name": "PR reviewer",
        "status": "active",
        "external_ref": "tenant-42/pr-reviewer",
        "created_at": "2026-07-17T00:00:00Z",
        "updated_at": "2026-07-17T00:00:00Z",
    }


def _project(project_id: str = "prj_1") -> dict:
    return {
        "id": project_id,
        "name": "Product Ops",
        "handle": "product-ops",
        "access_mode": "restricted",
        "external_ref": "workspace-42",
        "created_at": "2026-07-17T00:00:00Z",
        "updated_at": "2026-07-17T00:00:00Z",
    }


def test_create_agent_adopt_sends_adopt_fields() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "POST"
        assert request.url.path == "/v1/projects/test-project/agents"
        body = json.loads(request.content)
        assert body["if_exists"] == "adopt"
        assert body["external_ref"] == "tenant-42/pr-reviewer"
        # Adopt of an existing agent answers 200, not 201.
        return httpx.Response(200, json=_agent())

    client = _client_with(handler)
    agent = client.create_agent(
        CreateAgentRequest(name="PR reviewer"),
        adopt_existing=True,
        external_ref="tenant-42/pr-reviewer",
    )
    assert agent.id == "agent_1"
    client.close()


def test_create_agent_plain_create_returns_201() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        assert "external_ref" not in body
        assert "if_exists" not in body, "plain create must not send if_exists"
        return httpx.Response(201, json=_agent())

    client = _client_with(handler)
    agent = client.create_agent(CreateAgentRequest(name="PR reviewer"))
    assert agent.id == "agent_1"
    client.close()


def test_create_agent_adopt_requires_external_ref_before_http() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("no request may be sent when the options are invalid")

    client = _client_with(handler)
    with pytest.raises(ValueError, match="external_ref"):
        client.create_agent(CreateAgentRequest(name="PR reviewer"), adopt_existing=True)
    client.close()


def test_create_agent_adopt_retries_transient_503() -> None:
    requests = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal requests
        requests += 1
        if requests == 1:
            return httpx.Response(503, headers={"Retry-After": "0"})
        return httpx.Response(200, json=_agent())

    client = _client_with(handler, retry=2)
    agent = client.create_agent(
        CreateAgentRequest(name="PR reviewer"),
        adopt_existing=True,
        external_ref="tenant-42/pr-reviewer",
    )
    assert agent.id == "agent_1"
    assert requests == 2, "the transient 503 must be retried in adopt mode"
    client.close()


def test_create_agent_plain_create_is_not_retried() -> None:
    requests = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal requests
        requests += 1
        return httpx.Response(503, headers={"Retry-After": "0"})

    client = _client_with(handler, retry=2)
    # An envelope-less 503 surfaces as the underlying httpx status error.
    with pytest.raises(httpx.HTTPStatusError):
        client.create_agent(CreateAgentRequest(name="PR reviewer"))
    assert requests == 1, "a plain create POST must not be replayed"
    client.close()


def test_create_agent_adopt_conflict_code() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": {
                    "code": "external_identity_conflict",
                    "message": "external_ref is owned by a deleted agent",
                }
            },
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as exc:
        client.create_agent(
            CreateAgentRequest(name="PR reviewer"),
            adopt_existing=True,
            external_ref="tenant-42/pr-reviewer",
        )
    assert exc.value.code == MobiusAPIError.EXTERNAL_IDENTITY_CONFLICT
    assert exc.value.status == 409
    client.close()


def test_create_project_adopt_hits_projects_route() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "POST"
        assert request.url.path == "/v1/projects"
        body = json.loads(request.content)
        assert body["if_exists"] == "adopt"
        assert body["external_ref"] == "workspace-42"
        return httpx.Response(200, json=_project())

    client = _client_with(handler)
    project = client.create_project(
        CreateProjectRequest(name="Product Ops"),
        adopt_existing=True,
        external_ref="workspace-42",
    )
    assert project.id == "prj_1"
    client.close()


def test_create_project_adopt_requires_external_ref_before_http() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("no request may be sent when the options are invalid")

    client = _client_with(handler)
    with pytest.raises(ValueError, match="external_ref"):
        client.create_project(
            CreateProjectRequest(name="Product Ops"), adopt_existing=True
        )
    client.close()


def test_create_project_adopt_retries_transient_503() -> None:
    requests = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal requests
        requests += 1
        if requests == 1:
            return httpx.Response(503, headers={"Retry-After": "0"})
        return httpx.Response(200, json=_project())

    client = _client_with(handler, retry=2)
    project = client.create_project(
        CreateProjectRequest(name="Product Ops"),
        adopt_existing=True,
        external_ref="workspace-42",
    )
    assert project.id == "prj_1"
    assert requests == 2, "the transient 503 must be retried in adopt mode"
    client.close()


def test_create_project_adopt_conflict_codes() -> None:
    assert MobiusAPIError.PROJECT_ARCHIVED == "project_archived"
    assert MobiusAPIError.PROJECT_CAPACITY_REACHED == "project_capacity_reached"

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": {
                    "code": "project_archived",
                    "message": "project is archived",
                }
            },
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as exc:
        client.create_project(
            CreateProjectRequest(name="Product Ops"),
            adopt_existing=True,
            external_ref="workspace-42",
        )
    assert exc.value.code == MobiusAPIError.PROJECT_ARCHIVED
    client.close()
