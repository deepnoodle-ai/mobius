"""Route-level tests for the agent memory client surface."""

from __future__ import annotations

import httpx
import pytest

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    ListAgentMemoryEntriesOptions,
    MobiusAPIError,
    SaveAgentMemoryEntryRequest,
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


def _entry(entry_id: str, key: str) -> dict:
    return {
        "key": key,
        "kind": "fact",
        "entry_id": entry_id,
        "importance": 50,
        "pinned": False,
        "version": 1,
        "created_at": "2026-07-17T00:00:00Z",
        "updated_at": "2026-07-17T00:00:00Z",
    }


def _change(change_id: str, version: int) -> dict:
    return {
        "id": change_id,
        "agent_id": "agent_1",
        "memory_entry_id": "mem_1",
        "memory_key": "prefs",
        "operation": "updated",
        "version": version,
        "reason": "remembered",
        "created_at": "2026-07-17T00:00:00Z",
    }


def test_memory_summary_search_save_delete_routes() -> None:
    seen: list[tuple[str, str, dict]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.method, request.url.path, dict(request.url.params)))
        if request.method == "GET" and request.url.path.endswith("/memory"):
            return httpx.Response(
                200,
                json={"agent_id": "agent_1", "entry_count": 1, "counts_by_kind": {"fact": 1}},
            )
        if request.method == "GET" and request.url.path.endswith("/memory/entries"):
            assert request.url.params["query"] == "preferences"
            assert request.url.params["search_mode"] == "hybrid"
            assert request.url.params["kind"] == "fact"
            assert request.url.params["limit"] == "25"
            return httpx.Response(
                200,
                json={
                    "items": [_entry("mem_1", "prefs")],
                    "has_more": False,
                    "search_coverage": {
                        "indexed_entries": 9,
                        "total_entries": 10,
                        "complete": False,
                    },
                },
            )
        if request.method == "PUT":
            return httpx.Response(201, json=_entry("mem_1", "prefs"))
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(404)

    client = _client_with(handler)
    summary = client.get_agent_memory("agent_1")
    assert summary.entry_count == 1

    page = client.list_agent_memory_entries(
        "agent_1",
        ListAgentMemoryEntriesOptions(
            query="preferences", search_mode="hybrid", kind="fact", limit=25
        ),
    )
    assert page.items[0].key == "prefs"
    assert page.search_coverage is not None
    assert page.search_coverage.complete is False

    entry = client.save_agent_memory_entry(
        "agent_1", "prefs", SaveAgentMemoryEntryRequest(content="dark mode")
    )
    assert entry.entry_id == "mem_1"

    client.delete_agent_memory_entry("agent_1", "prefs")
    client.close()

    assert seen[0][1] == "/v1/projects/test-project/agents/agent_1/memory"
    assert seen[2][1] == "/v1/projects/test-project/agents/agent_1/memory/entries/prefs"
    assert seen[3][0] == "DELETE"


def test_semantic_search_unavailable_is_not_downgraded() -> None:
    modes: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        modes.append(request.url.params.get("search_mode", ""))
        return httpx.Response(
            503,
            json={
                "error": {
                    "code": "memory_semantic_search_unavailable",
                    "message": "index offline",
                }
            },
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as err:
        client.list_agent_memory_entries(
            "agent_1", ListAgentMemoryEntriesOptions(query="x", search_mode="semantic")
        )
    client.close()

    assert err.value.status == 503
    assert err.value.code == "memory_semantic_search_unavailable"
    assert modes == ["semantic"]


def test_sync_agent_memory_drains_change_pages() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith("/memory/changes")
        after = request.url.params.get("after")
        if after == "cur_0":
            return httpx.Response(
                200,
                json={"items": [_change("chg_1", 1)], "has_more": True, "next_cursor": "cur_1"},
            )
        assert after == "cur_1"
        return httpx.Response(
            200,
            json={"items": [_change("chg_2", 2)], "has_more": False, "next_cursor": "cur_2"},
        )

    client = _client_with(handler)
    result = client.sync_agent_memory("agent_1", "cur_0")
    client.close()

    assert result.reset is False
    assert [c.id for c in result.changes] == ["chg_1", "chg_2"]
    assert result.next_cursor == "cur_2"
    assert result.entries == []


def test_sync_agent_memory_recovers_from_expired_cursor() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/memory/changes"):
            if request.url.params.get("after") == "cur_stale":
                return httpx.Response(
                    410,
                    json={
                        "error": {
                            "code": "memory_cursor_expired",
                            "message": "cursor predates retained history",
                        }
                    },
                )
            return httpx.Response(
                200,
                json={"items": [_change("chg_9", 9)], "has_more": False, "next_cursor": "cur_fresh"},
            )
        assert request.url.path.endswith("/memory/entries")
        if request.url.params.get("cursor") == "ecur_1":
            return httpx.Response(
                200, json={"items": [_entry("mem_2", "style")], "has_more": False}
            )
        return httpx.Response(
            200,
            json={"items": [_entry("mem_1", "prefs")], "has_more": True, "next_cursor": "ecur_1"},
        )

    client = _client_with(handler)
    result = client.sync_agent_memory("agent_1", "cur_stale")
    client.close()

    assert result.reset is True
    assert result.changes == []
    assert [e.key for e in result.entries] == ["prefs", "style"]
    assert result.next_cursor == "cur_fresh"


def test_sync_agent_memory_propagates_other_errors() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            404, json={"error": {"code": "not_found", "message": "no such agent"}}
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as err:
        client.sync_agent_memory("agent_missing")
    client.close()
    assert err.value.status == 404
