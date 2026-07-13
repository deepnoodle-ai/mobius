"""Tests for the session-transcript v2 view and client stream helpers."""

from __future__ import annotations

import httpx
import pytest

from deepnoodle.mobius import (
    AuthRevokedError,
    Client,
    ClientOptions,
    GetSessionTranscriptOptions,
    InvokeAgentOptions,
    MobiusAPIError,
    NudgeSessionOptions,
    SessionTranscript,
    TranscriptStreamEvent,
    is_terminal_turn_status,
    normalize_tool_use,
    text_of,
    tool_result_text,
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


def _apply(t: SessionTranscript, frame: dict, sse_id: str | None = None) -> None:
    t.apply(
        TranscriptStreamEvent(
            event_type=frame.get("event_type", ""), frame=frame, id=sse_id
        )
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


def test_transcript_upsert_block_delta_converge() -> None:
    t = SessionTranscript()
    _apply(t, _upsert())
    _apply(t, {"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 0, "block": {"type": "text", "text": ""}})
    _apply(t, {"event_type": "message.delta", "session_id": "s1", "message_id": "m_a", "content_index": 0, "text": "hel"})
    _apply(t, {"event_type": "message.delta", "session_id": "s1", "message_id": "m_a", "content_index": 0, "text": "lo"})
    assert t.message("m_a")["content"][0]["text"] == "hello"
    # Completing block replaces whatever deltas built.
    _apply(t, {"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 0, "block": {"type": "text", "text": "hello world"}})
    assert t.message("m_a")["content"][0]["text"] == "hello world"


def test_transcript_block_at_gap_index_pads_content() -> None:
    t = SessionTranscript()
    _apply(t, _upsert())
    # Opening index 2 on empty content pads indexes 0 and 1 with empty blocks.
    _apply(t, {"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 2, "block": {"type": "text", "text": "third"}})
    content = t.message("m_a")["content"]
    assert len(content) == 3
    assert content[2]["text"] == "third"
    # Deltas target the padded blocks, not a hole.
    _apply(t, {"event_type": "message.delta", "session_id": "s1", "message_id": "m_a", "content_index": 0, "text": "pad"})
    assert t.message("m_a")["content"][0]["text"] == "pad"
    # A late message.block fills a padded index in place.
    _apply(t, {"event_type": "message.block", "session_id": "s1", "message_id": "m_a", "content_index": 1, "block": {"type": "text", "text": "second"}})
    assert t.message("m_a")["content"][1]["text"] == "second"


def test_transcript_block_patch_merge_and_null_clear() -> None:
    t = SessionTranscript()
    _apply(t, _upsert(content=[{"type": "tool_use", "id": "toolu_1", "name": "fetch", "input": {}, "status": "pending"}]))
    _apply(t, {"event_type": "message.block.patch", "session_id": "s1", "message_id": "m_a", "content_index": 0, "status": "running", "progress": {"display": "scanned 1400 lines"}})
    block = t.message("m_a")["content"][0]
    assert block["status"] == "running"
    assert block["progress"] == {"display": "scanned 1400 lines"}
    # progress: null clears; status still updates.
    _apply(t, {"event_type": "message.block.patch", "session_id": "s1", "message_id": "m_a", "content_index": 0, "status": "ok", "progress": None})
    block = t.message("m_a")["content"][0]
    assert block["status"] == "ok"
    assert "progress" not in block


def test_transcript_normalizes_null_content_at_every_ingress() -> None:
    t = SessionTranscript()
    _apply(t, _upsert(content=None))
    assert t.message("m_a")["content"] == []

    snapshot_message = _upsert(id="m_snapshot", content=None)
    snapshot_message.pop("event_type")
    t.apply_snapshot(
        {
            "messages": [snapshot_message],
            "turns": [],
            "has_more": True,
            "resume_cursor": "1.1",
        }
    )
    assert t.message("m_snapshot")["content"] == []

    ack = _ack_body()
    ack["user_message"]["content"] = None
    t.seed(ack)
    assert t.message("m_user")["content"] == []


def test_renderable_messages_and_tool_helpers() -> None:
    t = SessionTranscript()
    _apply(t, _turn())
    _apply(t, _upsert(id="empty_1", turn_index=1, content=[]))
    _apply(t, _upsert(id="empty_2", turn_index=2, content=[]))
    _apply(
        t,
        _upsert(
            id="preview",
            turn_index=3,
            content=[
                {
                    "type": "tool_use",
                    "id": "call_1",
                    "name": "catalog",
                    "input": {"command": "check", "args": {"domain": "x.test"}},
                }
            ],
        ),
    )
    _apply(
        t,
        _upsert(
            id="final",
            status="final",
            turn_index=3,
            sequence=4,
            content=[
                {
                    "type": "tool_use",
                    "id": "call_1",
                    "name": "catalog",
                    "input": {"command": "check", "args": {"domain": "x.test"}},
                    "resolved_action": {
                        "name": "naming.domain.check",
                        "input": {"domain": "x.test"},
                    },
                },
                {"type": "text", "text": "done"},
            ],
        ),
    )
    visible = t.renderable_messages()
    assert [row["id"] for row in visible] == ["final", "empty_2"]
    normalized = normalize_tool_use(visible[0]["content"][0])
    assert normalized.wire_name == "catalog"
    assert normalized.resolved_action["name"] == "naming.domain.check"
    assert normalized.wire_input["command"] == "check"
    assert text_of(visible[0]) == "done"
    assert tool_result_text({"content": "plain"}) == "plain"
    assert tool_result_text({"content": [{"type": "text", "text": "blocks"}]}) == "blocks"


def test_transcript_terminal_turn_prunes_streaming_rows() -> None:
    t = SessionTranscript()
    _apply(t, _upsert())
    _apply(t, _upsert(id="m_final", role="user", status="final", turn_index=0, sequence=42))
    _apply(t, _turn(status="cancelled"))
    assert t.message("m_a") is None  # pruned
    assert t.message("m_final") is not None  # durable row survives


def test_transcript_cursor_and_ready() -> None:
    t = SessionTranscript()
    _apply(t, _turn(), sse_id="42.7")
    assert t.cursor == "42.7"
    assert t.ready is False
    _apply(t, {"event_type": "stream.ready", "session_id": "s1", "resume_cursor": "99.9"})
    assert t.cursor == "99.9"
    assert t.ready is True


def test_transcript_ordering_and_unknown_frame() -> None:
    t = SessionTranscript()
    _apply(t, {"event_type": "future.frame", "whatever": True})  # ignored
    _apply(t, _upsert(id="m2", status="final", turn_index=1, sequence=43))
    _apply(t, _upsert(id="m1", role="user", status="final", turn_index=0, sequence=42))
    _apply(t, _upsert(id="m3", status="streaming", turn_index=2, sequence=None))
    assert [m["id"] for m in t.messages()] == ["m1", "m2", "m3"]


def test_transcript_apply_snapshot_prunes_streaming() -> None:
    t = SessionTranscript()
    _apply(t, _upsert(id="m_stale", turn_id="t0", turn_index=9))
    snap = {
        "messages": [_upsert(id="m1", role="user", status="final", turn_index=0, sequence=42)],
        "turns": [],
        "has_more": False,
        "resume_cursor": "42.6",
    }
    # Strip event_type off the snapshot message (snapshots are not frames).
    snap["messages"][0].pop("event_type", None)
    t.apply_snapshot(snap)
    assert t.message("m_stale") is None  # dropped: absent from final page
    assert t.message("m1") is not None
    assert t.cursor == "42.6"


def test_transcript_seed_folds_ack_state() -> None:
    t = SessionTranscript()
    t.seed(_ack_body())
    assert t.message("m_user")["role"] == "user"
    assert t.turn("t1")["status"] == "queued"
    assert t.cursor == "41.6"


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


def test_stream_session_transcript_handles_crlf_framing() -> None:
    # An SSE stream framed with CRLF (\r\n\r\n) is valid per the spec and must
    # decode identically to an LF-framed one.
    def handler(request: httpx.Request) -> httpx.Response:
        body = (
            'id: 42.7\r\nevent: turn.upsert\r\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"%s","updated_at":"%s"}\r\n\r\n'
            % (AT, AT)
            + 'event: stream.ready\r\ndata: {"event_type":"stream.ready","session_id":"s1","resume_cursor":"42.7"}\r\n\r\n'
        )
        return httpx.Response(200, text=body, headers={"Content-Type": "text/event-stream"})

    client = _client_with(handler)
    events = list(client.stream_session_transcript("sess_1"))
    assert len(events) == 2
    assert events[0].id == "42.7"
    assert events[0].event_type == "turn.upsert"
    assert events[0].frame["status"] == "running"
    # The ready frame has no id: line; last-event-id persists as the cursor.
    assert events[1].id == "42.7"


def test_watch_session_transcript_propagates_permanent_error() -> None:
    # A permanent status (here 404) must surface immediately, not trigger an
    # endless reconnect loop.
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(404, json={"error": {"code": "not_found", "message": "no such session"}})

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as caught:
        list(client.watch_session_transcript("sess_1"))
    assert caught.value.code == "not_found"
    assert caught.value.status == 404
    assert calls == 1  # surfaced on the first attempt, no reconnect


def test_ordinary_request_exposes_structured_mobius_api_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": {
                    "code": "session_turn_active",
                    "message": "another direct turn is active",
                    "details": {"turn_id": "turn_blocking", "status": "running"},
                }
            },
            headers={"X-Request-ID": "req_1", "Retry-After": "2"},
        )

    client = _client_with(handler)
    with pytest.raises(MobiusAPIError) as caught:
        client.invoke_agent(
            InvokeAgentOptions(
                agent_name="support", content=[{"type": "text", "text": "next"}]
            )
        )
    assert caught.value.status == 409
    assert caught.value.code == "session_turn_active"
    assert caught.value.details == {"turn_id": "turn_blocking", "status": "running"}
    assert caught.value.request_id == "req_1"
    assert caught.value.retry_after == 2


def test_nudge_session_is_a_thin_typed_wrapper() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = __import__("json").loads(request.content)
        return httpx.Response(
            202,
            json={
                "nudge_id": "nudge_1",
                "delivery": "current_turn",
                "session": _ack_body()["session"],
                "turn": _ack_body()["turn"],
                "after_sequence": 2,
                "deduped": False,
                "woke_turn": True,
            },
        )

    client = _client_with(handler)
    ack = client.nudge_session(
        "s1",
        NudgeSessionOptions(
            content="Use the shorter name", idempotency_key="event_2", wake=True
        ),
    )
    assert ack.nudge_id == "nudge_1"
    assert seen["path"] == "/v1/projects/test-project/sessions/s1/nudges"
    assert seen["body"] == {
        "content": "Use the shorter name",
        "idempotency_key": "event_2",
        "wake": True,
    }


def test_watch_session_transcript_raises_auth_revoked_on_401() -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(401, json={"error": {"code": "unauthorized", "message": "revoked"}})

    client = _client_with(handler)
    with pytest.raises(AuthRevokedError):
        list(client.watch_session_transcript("sess_1"))
    assert calls == 1


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
    watch = client.watch_session_transcript("sess_1")
    steps = sum(1 for _ in watch)

    assert steps == 2  # one turn.upsert per connection
    assert len(calls) == 2  # reconnected once on rotate, stopped on idle
    assert calls[0] is None  # first connect: no cursor
    assert calls[1] == "42.7"  # reconnect carries the advanced cursor
    assert watch.transcript.turn("t1")["status"] == "completed"
    assert watch.transcript.cursor == "43.9"


def test_invoke_agent_streams_turn_to_terminal() -> None:
    stream_calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal stream_calls
        if request.url.path.endswith("/agents/invoke"):
            return httpx.Response(202, json=_ack_body())
        if request.url.path.endswith("/sessions/s1/transcript"):
            assert request.url.params.get("cursor") == "43.9"
            return httpx.Response(
                200,
                json={
                    "messages": [],
                    "turns": [],
                    "has_more": False,
                    "resume_cursor": "43.9",
                },
            )
        assert request.url.path.endswith("/sessions/s1/transcript/stream")
        stream_calls += 1
        assert request.url.params.get("cursor") == "41.6"  # opened from the seeded cursor
        body = (
            'event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"%s"}\n\n'
            % AT
            + 'id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"%s","updated_at":"%s"}\n\n'
            % (AT, AT)
        )
        return httpx.Response(200, text=body, headers={"Content-Type": "text/event-stream"})

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(agent_name="support", content=[{"type": "text", "text": "hi"}])
    )
    assert turn.id == "t1"
    assert turn.session_id == "s1"
    assert turn.status == "queued"  # seeded from the invoke response
    assert turn.deduped is False
    assert stream_calls == 0  # lazy: no stream until iteration
    # The caller's message row is seeded before any streaming.
    assert [m["id"] for m in turn.messages()] == ["m_user"]

    steps = sum(1 for _ in turn)

    assert steps == 2  # message.upsert + terminal turn.upsert
    assert turn.status == "completed"
    assert [m["id"] for m in turn.messages()] == ["m_user", "m_a"]
    assert turn.transcript.cursor == "43.9"


def test_invoke_agent_reconciles_terminal_snapshot_before_final_update() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/agents/invoke"):
            return httpx.Response(202, json=_ack_body())
        if request.url.path.endswith("/sessions/s1/transcript/stream"):
            body = (
                'event: message.upsert\ndata: {"event_type":"message.upsert","id":"m_preview","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}],"created_at":"%s"}\n\n'
                % AT
                + 'id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"%s","updated_at":"%s"}\n\n'
                % (AT, AT)
            )
            return httpx.Response(
                200, text=body, headers={"Content-Type": "text/event-stream"}
            )
        if request.url.path.endswith("/sessions/s1/transcript"):
            assert request.url.params.get("cursor") == "43.9"
            return httpx.Response(
                200,
                json={
                    "messages": [
                        {
                            "id": "m_final",
                            "session_id": "s1",
                            "agent_id": "a1",
                            "role": "assistant",
                            "status": "final",
                            "turn_id": "t1",
                            "turn_index": 1,
                            "sequence": 43,
                            "entry_type": "message",
                            "content": [
                                {
                                    "type": "tool_use",
                                    "id": "call_1",
                                    "name": "lookup",
                                    "input": {},
                                },
                                {
                                    "type": "tool_result",
                                    "tool_use_id": "call_1",
                                    "content": "ok",
                                },
                                {"type": "text", "text": "done"},
                            ],
                            "created_at": AT,
                        }
                    ],
                    "turns": [],
                    "has_more": False,
                    "resume_cursor": "43.9",
                },
            )
        raise AssertionError(f"unexpected request: {request.url}")

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(agent_name="support", content=[{"type": "text", "text": "hi"}])
    )
    updates = list(turn.updates())

    assert len(updates) == 2
    assert updates[-1].connection == "ended"
    assert updates[-1].cursor == "43.9"
    assert [m["id"] for m in updates[-1].transcript.renderable_messages_for_turn("t1")] == [
        "m_user",
        "m_final",
    ]


def test_invoke_agent_surfaces_terminal_reconciliation_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/agents/invoke"):
            return httpx.Response(202, json=_ack_body())
        if request.url.path.endswith("/sessions/s1/transcript/stream"):
            body = (
                'id: 43.9\nevent: turn.upsert\ndata: {"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"%s","updated_at":"%s"}\n\n'
                % (AT, AT)
            )
            return httpx.Response(
                200, text=body, headers={"Content-Type": "text/event-stream"}
            )
        if request.url.path.endswith("/sessions/s1/transcript"):
            return httpx.Response(
                500, json={"error": {"code": "snapshot_unavailable"}}
            )
        raise AssertionError(f"unexpected request: {request.url}")

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(agent_name="support", content=[{"type": "text", "text": "hi"}])
    )
    with pytest.raises(MobiusAPIError) as exc_info:
        list(turn.updates())

    assert exc_info.value.status == 500
    assert turn.status == "completed"


def test_turn_transcript_exposes_live_failed_turn_errors() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(202, json=_ack_body())

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(agent_name="support", content=[{"type": "text", "text": "hi"}])
    )
    _apply(
        turn.transcript,
        _turn(
            status="failed",
            error_type="invalid_conversation_state",
            error_message="history ended with assistant content",
        ),
    )
    assert turn.error_type == "invalid_conversation_state"
    assert turn.error_message == "history ended with assistant content"
    assert str(turn.error) == (
        "invalid_conversation_state: history ended with assistant content"
    )


def test_invoke_agent_hydrates_terminal_turn_from_snapshot() -> None:
    stream_calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal stream_calls
        if request.url.path.endswith("/agents/invoke"):
            ack = _ack_body()
            ack["deduped"] = True
            ack.pop("user_message")
            ack["turn"]["status"] = "completed"
            return httpx.Response(202, json=ack)
        if request.url.path.endswith("/sessions/s1/transcript"):
            # Two pages: hydration must follow next_page_token until has_more
            # is false so messages() includes the older page.
            if request.url.params.get("page_token") == "pt_2":
                return httpx.Response(
                    200,
                    json={
                        "messages": [
                            {"id": "m_a", "session_id": "s1", "agent_id": "a1", "role": "assistant", "status": "final", "turn_id": "t1", "turn_index": 1, "sequence": 43, "entry_type": "message", "content": [{"type": "text", "text": "done"}], "created_at": AT},
                        ],
                        "turns": [{"id": "t1", "session_id": "s1", "agent_id": "a1", "attempt": 1, "status": "completed", "created_at": AT, "updated_at": AT}],
                        "has_more": False,
                        "resume_cursor": "43.9",
                    },
                )
            return httpx.Response(
                200,
                json={
                    "messages": [
                        {"id": "m_user", "session_id": "s1", "agent_id": "a1", "role": "user", "status": "final", "turn_id": "t1", "turn_index": 0, "sequence": 42, "entry_type": "message", "content": [], "created_at": AT},
                    ],
                    "turns": [],
                    "has_more": True,
                    "next_page_token": "pt_2",
                    "resume_cursor": "42.1",
                },
            )
        stream_calls += 1
        return httpx.Response(404)

    client = _client_with(handler)
    turn = client.invoke_agent(
        InvokeAgentOptions(
            agent_name="support",
            content=[{"type": "text", "text": "hi"}],
            idempotency_key="evt_1",
        )
    )
    assert turn.deduped is True
    assert turn.status == "completed"

    steps = sum(1 for _ in turn)

    assert steps == 1  # one snapshot hydration
    assert stream_calls == 0  # no SSE connection for a finished turn
    assert [m["id"] for m in turn.messages()] == ["m_user", "m_a"]


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
