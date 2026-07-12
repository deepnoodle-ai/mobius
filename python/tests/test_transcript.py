"""Tests for the session-transcript v2 reducer and client stream helpers."""

from __future__ import annotations

import httpx

from deepnoodle.mobius import (
    Client,
    ClientOptions,
    GetSessionTranscriptOptions,
    InvokeAgentOptions,
    SessionTranscriptReducer,
    is_terminal_turn_status,
)

AT = "2026-07-11T17:03:21Z"


def _client_with(handler) -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
        ),
        transport=httpx.MockTransport(handler),
    )


def _upsert(**over):
    frame = {
        "event_type": "message.upsert",
        "id": "m_a",
        "session_id": "s1",
        "agent_id": "a1",
        "role": "assistant",
        "status": "streaming",
        "turn_id": "t1",
        "turn_index": 1,
        "sequence": None,
        "entry_type": "message",
        "content": [],
        "created_at": AT,
    }
    frame.update(over)
    return frame


def _turn(status="running", **over):
    frame = {
        "event_type": "turn.upsert",
        "id": "t1",
        "session_id": "s1",
        "agent_id": "a1",
        "attempt": 1,
        "status": status,
        "created_at": AT,
        "updated_at": AT,
    }
    frame.update(over)
    return frame


def test_reducer_upsert_block_delta_converge() -> None:
    r = SessionTranscriptReducer()
    r.apply(_upsert())
    r.apply({"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 0, "block": {"type": "text", "text": ""}})
    r.apply({"event_type": "message.delta", "session_id": "s1", "message_id": "m_a", "content_index": 0, "text": "hel"})
    r.apply({"event_type": "message.delta", "session_id": "s1", "message_id": "m_a", "content_index": 0, "text": "lo"})
    assert r.rows["m_a"]["content"][0]["text"] == "hello"
    # Completing block replaces whatever deltas built.
    r.apply({"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 0, "block": {"type": "text", "text": "hello world"}})
    assert r.rows["m_a"]["content"][0]["text"] == "hello world"


def test_reducer_block_patch_merge_and_null_clear() -> None:
    r = SessionTranscriptReducer()
    r.apply(_upsert(content=[{"type": "tool_use", "id": "toolu_1", "name": "fetch", "input": {}, "status": "pending"}]))
    r.apply({"event_type": "message.block.patch", "session_id": "s1", "message_id": "m_a", "content_index": 0, "status": "running", "progress": {"display": "scanned 1400 lines"}})
    block = r.rows["m_a"]["content"][0]
    assert block["status"] == "running"
    assert block["progress"] == {"display": "scanned 1400 lines"}
    # progress: null clears; status still updates.
    r.apply({"event_type": "message.block.patch", "session_id": "s1", "message_id": "m_a", "content_index": 0, "status": "ok", "progress": None})
    block = r.rows["m_a"]["content"][0]
    assert block["status"] == "ok"
    assert "progress" not in block


def test_reducer_terminal_turn_prunes_streaming_rows() -> None:
    r = SessionTranscriptReducer()
    r.apply(_upsert())
    r.apply(_upsert(id="m_final", role="user", status="final", turn_index=0, sequence=42))
    r.apply(_turn(status="cancelled"))
    assert "m_a" not in r.rows  # pruned
    assert "m_final" in r.rows  # durable row survives


def test_reducer_cursor_and_ready() -> None:
    r = SessionTranscriptReducer()
    r.apply(_turn(), sse_id="42.7")
    assert r.cursor == "42.7"
    assert r.ready is False
    r.apply({"event_type": "stream.ready", "session_id": "s1", "resume_cursor": "99.9"})
    assert r.cursor == "99.9"
    assert r.ready is True


def test_reducer_ordering_and_unknown_frame() -> None:
    r = SessionTranscriptReducer()
    r.apply({"event_type": "future.frame", "whatever": True})  # ignored
    r.apply(_upsert(id="m2", status="final", turn_index=1, sequence=43))
    r.apply(_upsert(id="m1", role="user", status="final", turn_index=0, sequence=42))
    r.apply(_upsert(id="m3", status="streaming", turn_index=2, sequence=None))
    assert [m["id"] for m in r.messages()] == ["m1", "m2", "m3"]


def test_reducer_apply_snapshot_prunes_streaming() -> None:
    r = SessionTranscriptReducer()
    r.apply(_upsert(id="m_stale", turn_id="t0", turn_index=9))
    snap = {
        "messages": [_upsert(id="m1", role="user", status="final", turn_index=0, sequence=42)],
        "turns": [],
        "has_more": False,
        "resume_cursor": "42.6",
    }
    # Strip event_type off the snapshot message (snapshots are not frames).
    snap["messages"][0].pop("event_type", None)
    r.apply_snapshot(snap)
    assert "m_stale" not in r.rows  # dropped: absent from final page
    assert "m1" in r.rows
    assert r.cursor == "42.6"


def test_get_session_transcript_builds_query() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["cursor"] = request.url.params.get("cursor")
        seen["limit"] = request.url.params.get("limit")
        return httpx.Response(200, json={"messages": [], "turns": [], "has_more": False, "resume_cursor": "1.1"})

    client = _client_with(handler)
    snap = client.get_session_transcript("sess_1", GetSessionTranscriptOptions(cursor="10.2", limit=50))
    assert snap.resume_cursor == "1.1"
    assert seen["path"] == "/v1/projects/test-project/sessions/sess_1/transcript"
    assert seen["cursor"] == "10.2"
    assert seen["limit"] == "50"


def test_stream_session_transcript_decodes_frames_with_id() -> None:
    accept: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        accept["value"] = request.headers["accept"]
        body = (
            'id: 42.7\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"%s","updated_at":"%s"}\n\n'
            % (AT, AT)
            + 'event: stream.ready\ndata: {"event_type":"stream.ready","session_id":"s1","resume_cursor":"42.7"}\n\n'
        )
        return httpx.Response(200, text=body, headers={"Content-Type": "text/event-stream"})

    client = _client_with(handler)
    events = list(client.stream_session_transcript("sess_1"))
    assert accept["value"] == "text/event-stream"
    assert len(events) == 2
    assert events[0].id == "42.7"
    assert events[0].event_type == "turn.upsert"
    # The ready frame has no id: line; last-event-id persists as the cursor.
    assert events[1].id == "42.7"


def test_watch_session_transcript_reconnects_on_rotate_stops_on_idle() -> None:
    calls: list[str | None] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.params.get("cursor"))
        if len(calls) == 1:
            body = (
                'id: 42.7\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"%s","updated_at":"%s"}\n\n'
                % (AT, AT)
                + 'event: stream.end\ndata: {"event_type":"stream.end","session_id":"s1","reason":"rotate"}\n\n'
            )
        else:
            body = (
                'id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"%s","updated_at":"%s"}\n\n'
                % (AT, AT)
                + 'event: stream.end\ndata: {"event_type":"stream.end","session_id":"s1","reason":"idle"}\n\n'
            )
        return httpx.Response(200, text=body, headers={"Content-Type": "text/event-stream"})

    client = _client_with(handler)
    r = SessionTranscriptReducer()
    for event in client.watch_session_transcript("sess_1"):
        r.apply(event.frame, event.id)

    assert len(calls) == 2  # reconnected once on rotate, stopped on idle
    assert calls[0] is None  # first connect: no cursor
    assert calls[1] == "42.7"  # reconnect carries the advanced cursor
    assert r.turns["t1"]["status"] == "completed"


def test_invoke_agent_transcript_streams_turn_to_terminal() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/agents/invoke"):
            return httpx.Response(202, json=_ack_body())
        assert request.url.path.endswith("/sessions/s1/transcript/stream")
        assert request.url.params.get("cursor") == "41.6"
        body = (
            'event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"%s"}\n\n'
            % AT
            + 'id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"%s","updated_at":"%s"}\n\n'
            % (AT, AT)
        )
        return httpx.Response(200, text=body, headers={"Content-Type": "text/event-stream"})

    client = _client_with(handler)
    ack, stream = client.invoke_agent_transcript(
        InvokeAgentOptions(agent_name="support", content=[{"type": "text", "text": "hi"}])
    )
    assert ack.turn.id == "t1"
    assert ack.user_message is not None

    r = SessionTranscriptReducer()
    r.rows[ack.user_message.id] = ack.user_message.model_dump(mode="json")
    for event in stream:
        r.apply(event.frame, event.id)

    assert r.turns["t1"]["status"] == "completed"
    assert [m["id"] for m in r.messages_for_turn("t1")] == ["m_user", "m_a"]


def test_is_terminal_turn_status() -> None:
    assert is_terminal_turn_status("completed") is True
    assert is_terminal_turn_status("failed") is True
    assert is_terminal_turn_status("cancelled") is True
    assert is_terminal_turn_status("running") is False
    assert is_terminal_turn_status("queued") is False


def _ack_body() -> dict[str, object]:
    return {
        "after_sequence": 42,
        "resume_cursor": "41.6",
        "user_message": {
            "id": "m_user",
            "session_id": "s1",
            "agent_id": "a1",
            "role": "user",
            "status": "final",
            "turn_id": "t1",
            "turn_index": 0,
            "sequence": 42,
            "entry_type": "message",
            "content": [{"type": "text", "text": "hi"}],
            "created_at": AT,
        },
        "session": {
            "id": "s1",
            "agent_id": "a1",
            "origin": "api",
            "scope": "agent",
            "scope_name": "app",
            "scope_ref_id": "a1",
            "session_key": "app",
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
            "id": "t1",
            "agent_id": "a1",
            "session_id": "s1",
            "attempt": 1,
            "status": "queued",
            "created_at": "2026-05-27T00:00:00Z",
            "updated_at": "2026-05-27T00:00:00Z",
        },
    }
