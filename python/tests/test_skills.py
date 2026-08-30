"""Route-level tests for the skill and skill-assignment surface."""

from __future__ import annotations

import json

import httpx

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    SkillRequest,
)

SKILL_DOC = (
    "---\n"
    "allowed_tools:\n"
    "  - github.create_review_comment\n"
    "---\n"
    "Check the diff and leave concise findings.\n"
)


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            retry=0,
        ),
        transport=httpx.MockTransport(handler),
    )


def _skill(skill_id: str = "skill_1", source: str = "custom") -> dict:
    return {
        "id": skill_id,
        "owner": {"kind": "person", "id": "user_1"},
        "visibility": "private",
        "container": None,
        "posture": "only_you",
        "name": "Pull request review",
        "source": source,
        "instructions": "Check the diff and leave concise findings.",
        "allowed_tools": ["github.create_review_comment"],
        "created_at": "2026-07-17T00:00:00Z",
        "updated_at": "2026-07-17T00:00:00Z",
    }


def test_skill_lifecycle_routes() -> None:
    seen: list[tuple[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.method, request.url.path))
        if request.method == "GET" and request.url.path == "/v1/skills":
            assert request.url.params["include_system"] == "false"
            return httpx.Response(200, json={"items": [_skill()]})
        if request.method == "POST":
            assert json.loads(request.content)["name"] == "Pull request review"
            return httpx.Response(201, json=_skill())
        if request.method == "PUT":
            body = json.loads(request.content)
            assert body["instructions"], "update must send the full body"
            return httpx.Response(200, json=_skill())
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(200, json=_skill())

    client = _client_with(handler)
    page = client.list_skills(include_system=False)
    assert page.items[0].id == "skill_1"
    req = SkillRequest(
        name="Pull request review",
        instructions="Check the diff and leave concise findings.",
    )
    client.create_skill(req)
    client.get_skill("skill_1")
    client.update_skill("skill_1", req)
    client.delete_skill("skill_1")
    client.close()

    assert [p for _, p in seen[2:]] == ["/v1/skills/skill_1"] * 3
    assert [m for m, _ in seen[2:]] == ["GET", "PUT", "DELETE"]


def test_import_sends_document_verbatim() -> None:
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/v1/skills/import"
        bodies.append(json.loads(request.content))
        return httpx.Response(201, json=_skill())

    client = _client_with(handler)
    skill = client.import_skill(SKILL_DOC, name="Pull request review")
    client.close()

    assert bodies == [{"content": SKILL_DOC, "name": "Pull request review"}]
    assert skill.source == "custom"


def test_import_omits_name_when_not_given() -> None:
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        return httpx.Response(201, json=_skill())

    client = _client_with(handler)
    client.import_skill("Just instructions.")
    client.close()

    assert bodies == [{"content": "Just instructions."}]


def test_replace_agent_skill_assignments_preserves_order() -> None:
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert (
            request.url.path
            == "/v1/agents/agent_1/skill-assignments"
        )
        if request.method == "GET":
            return httpx.Response(200, json={"items": []})
        bodies.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "items": [
                    {
                        "agent_id": "agent_1",
                        "skill_id": "skill_2",
                        "enabled": True,
                        "position": 0,
                        "created_at": "2026-07-17T00:00:00Z",
                    },
                    {
                        "agent_id": "agent_1",
                        "skill_id": "skill_1",
                        "enabled": True,
                        "position": 1,
                        "created_at": "2026-07-17T00:00:00Z",
                    },
                ]
            },
        )

    client = _client_with(handler)
    client.list_agent_skill_assignments("agent_1")
    page = client.replace_agent_skill_assignments("agent_1", ["skill_2", "skill_1"])
    client.close()

    assert bodies == [{"skill_ids": ["skill_2", "skill_1"]}]
    assert [a.skill_id for a in page.items] == ["skill_2", "skill_1"]


def test_replace_agent_skill_assignments_sends_empty_set() -> None:
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        return httpx.Response(200, json={"items": []})

    client = _client_with(handler)
    client.replace_agent_skill_assignments("agent_1", [])
    client.close()

    assert bodies == [{"skill_ids": []}]
