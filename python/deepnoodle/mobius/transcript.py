"""Session-transcript v2 view and stream option types.

``SessionTranscript`` is the live view of a session — the session-scope
analogue of Dive's ``ResponseAccumulator``. It is built by folding
session-transcript stream frames (or snapshot pages) into place. Frames are
plain decoded JSON objects (``dict``) — the same open representation the SDK
already uses for ``SessionStreamEvent.data`` — so unknown fields and enum
values round-trip untouched.
"""

from __future__ import annotations

from copy import deepcopy
from dataclasses import dataclass
from typing import Any

_TERMINAL_TURN_STATUSES = frozenset({"completed", "failed", "cancelled"})


def is_terminal_turn_status(status: str) -> bool:
    """Report whether a turn status will not transition again.

    Turn status is an open string in the contract; only these three are
    terminal.
    """
    return status in _TERMINAL_TURN_STATUSES


@dataclass
class TranscriptStreamEvent:
    """A single decoded frame from a session-transcript v2 stream.

    ``event_type`` mirrors the SSE ``event:`` name and the frame's
    ``event_type``. ``id`` is the opaque resume cursor in effect through this
    frame: the server emits an SSE ``id:`` line only on state frames that
    advance the delivered watermark, and per the SSE spec the last-event-id
    persists, so ``id`` carries that watermark and is ``None`` only before the
    connection's first ``id:`` line.
    """

    event_type: str
    frame: dict[str, Any]
    id: str | None = None


@dataclass
class GetSessionTranscriptOptions:
    # Opaque resume cursor from a prior snapshot or stream; omit for a
    # bootstrap tail.
    cursor: str | None = None
    # Opaque fixed-cut continuation (next_page_token) when draining an
    # incremental cycle.
    page_token: str | None = None
    # Max messages per page. None uses the server default.
    limit: int | None = None


@dataclass
class StreamSessionTranscriptOptions:
    # Opaque resume cursor; omit to hydrate from the live tail.
    cursor: str | None = None


@dataclass
class WatchSessionTranscriptOptions:
    # Opaque resume cursor for the first connection. Ignored if `transcript`
    # already carries one.
    cursor: str | None = None
    # Existing view to continue folding into (e.g. one bootstrapped from
    # get_session_transcript pages). None starts fresh.
    transcript: "SessionTranscript | None" = None
    # Pause before reconnecting after a dropped connection (not a clean
    # rotate), in seconds.
    reconnect_delay: float = 1.0


@dataclass(frozen=True)
class NormalizedToolUse:
    """Provider wire identity plus canonical project-action identity."""

    wire_name: str
    wire_input: dict[str, Any]
    resolved_action: dict[str, Any] | None
    toolkit_name: str | None
    command: str | None
    raw: dict[str, Any]


def normalize_tool_use(block: dict[str, Any]) -> NormalizedToolUse:
    """Normalize a tool-use block without discarding its wire representation.

    Persisted ``resolved_action`` is authoritative. The legacy
    ``{command,args}`` meta-router shape is only a compatibility heuristic.
    """
    wire_name = str(block.get("name") or "")
    wire_input = block.get("input")
    if not isinstance(wire_input, dict):
        wire_input = {}
    resolved = block.get("resolved_action")
    if not isinstance(resolved, dict) or not isinstance(resolved.get("name"), str):
        resolved = None
    command = None
    canonical = str(resolved["name"]) if resolved else ""
    legacy_command = wire_input.get("command")
    if not canonical and isinstance(legacy_command, str) and legacy_command:
        command = legacy_command
        canonical = f"{wire_name}.{legacy_command}"
        args = wire_input.get("args")
        if isinstance(args, dict):
            resolved = {"name": canonical, "input": args}
    toolkit_name = None
    if "." in canonical:
        toolkit_name, command = canonical.split(".", 1)
    return NormalizedToolUse(
        wire_name=wire_name,
        wire_input=wire_input,
        resolved_action=resolved,
        toolkit_name=toolkit_name,
        command=command,
        raw=block,
    )


def text_of(message: dict[str, Any]) -> str:
    """Concatenate text blocks in a transcript message."""
    content = message.get("content")
    if not isinstance(content, list):
        return ""
    return "".join(
        str(block.get("text") or "")
        for block in content
        if isinstance(block, dict) and block.get("type") == "text"
    )


def tool_result_text(block: dict[str, Any]) -> str:
    """Return text from either tool-result content wire form."""
    content = block.get("content")
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    return "".join(
        str(child.get("text") or "")
        for child in content
        if isinstance(child, dict) and child.get("type") == "text"
    )


class SessionTranscript:
    """The live view of a session: message rows, turns, cursor, readiness.

    The whole merge is set-by-id: state frames carry absolute state, so last
    write wins and nothing is an increment except ``message.delta`` text.
    Ignoring deltas entirely still converges.

    The streaming client methods drive one for you: ``Client.invoke_agent``
    returns a ``TurnTranscript`` and ``Client.watch_session_transcript``
    returns a ``TranscriptWatcher``, both folding frames into an embedded view
    as you iterate. Construct one directly only for the escape hatches:
    polling ``get_session_transcript`` into :meth:`apply_snapshot`, or feeding
    ``stream_session_transcript`` events into :meth:`apply`.

    Not safe for concurrent use; drive it from a single thread.
    """

    def __init__(self) -> None:
        self._rows: dict[str, dict[str, Any]] = {}
        self._turns: dict[str, dict[str, Any]] = {}
        self._cursor: str | None = None
        self._ready: bool = False

    @property
    def cursor(self) -> str | None:
        """Opaque resume cursor in effect through everything folded in so far.

        Never parse it. Assign it only to resume a fresh view from a persisted
        position — applied frames and snapshots overwrite it.
        """
        return self._cursor

    @cursor.setter
    def cursor(self, value: str | None) -> None:
        self._cursor = value

    @property
    def ready(self) -> bool:
        """True once stream.ready has been seen on the current connection."""
        return self._ready

    def _reset_ready(self) -> None:
        # Ready is per-connection; the watch loop re-arms it on reconnect.
        self._ready = False

    def message(self, message_id: str) -> dict[str, Any] | None:
        """The message row with the given id, if present."""
        return self._rows.get(message_id)

    def turn(self, turn_id: str) -> dict[str, Any] | None:
        """The turn with the given id, if present."""
        return self._turns.get(turn_id)

    def turns(self) -> list[dict[str, Any]]:
        """Turns ordered by ``(created_at, id)``."""
        return sorted(
            self._turns.values(),
            key=lambda t: (t.get("created_at") or "", t.get("id") or ""),
        )

    def messages(self) -> list[dict[str, Any]]:
        """Rows in render order.

        Final rows are ordered by ``sequence``, then streaming rows by
        ``(turn.created_at, turn.id, turn_index)`` — ``turn_index`` alone is
        unique only within one turn, and turns can run concurrently.
        """
        rows = list(self._rows.values())
        final = sorted(
            (r for r in rows if r.get("status") == "final"),
            key=lambda r: r.get("sequence") or 0,
        )
        live = sorted(
            (r for r in rows if r.get("status") == "streaming"),
            key=self._live_sort_key,
        )
        return final + live

    def messages_for_turn(self, turn_id: str) -> list[dict[str, Any]]:
        """Rows belonging to one turn, in render order."""
        return [r for r in self.messages() if r.get("turn_id") == turn_id]

    def renderable_messages(self) -> list[dict[str, Any]]:
        """Return a UI-oriented projection while preserving lossless rows.

        Preview/final versions of the same response segment collapse to one,
        superseded empty assistant previews are hidden, and duplicate tool
        blocks are removed by tool-call id.
        """
        chosen: dict[str, dict[str, Any]] = {}
        order: list[str] = []
        for original in self.messages():
            row = _normalize_message(deepcopy(original))
            key = _render_logical_key(row)
            if key not in chosen:
                order.append(key)
            current = chosen.get(key)
            if (
                current is None
                or (row.get("status") == "final" and current.get("status") != "final")
                or (
                    row.get("status") == current.get("status")
                    and _message_completeness(row) >= _message_completeness(current)
                )
            ):
                chosen[key] = row

        newest_empty: dict[str, str] = {}
        for key in order:
            row = chosen[key]
            turn_id = str(row.get("turn_id") or "")
            turn = self._turns.get(turn_id)
            if not _is_empty_assistant_preview(row) or not turn or is_terminal_turn_status(
                str(turn.get("status") or "")
            ):
                continue
            previous = newest_empty.get(turn_id)
            if previous is None or int(chosen[previous].get("turn_index") or 0) <= int(
                row.get("turn_index") or 0
            ):
                newest_empty[turn_id] = key

        rows: list[dict[str, Any]] = []
        for key in order:
            row = chosen[key]
            turn_id = str(row.get("turn_id") or "")
            turn = self._turns.get(turn_id)
            active = bool(turn) and not is_terminal_turn_status(str(turn.get("status") or ""))
            if _is_empty_assistant_preview(row) and active and newest_empty.get(turn_id) != key:
                continue
            rows.append(row)
        _deduplicate_tool_blocks(rows)
        return rows

    def renderable_messages_for_turn(self, turn_id: str) -> list[dict[str, Any]]:
        """Renderable rows belonging to one turn."""
        return [r for r in self.renderable_messages() if r.get("turn_id") == turn_id]

    def apply(self, event: TranscriptStreamEvent) -> None:
        """Fold one stream event into the view.

        Unknown ``event_type`` values are ignored so the protocol can grow
        without breaking this client. This is the escape hatch for events
        obtained from ``stream_session_transcript`` or a custom transport; the
        streaming client methods call it for you.
        """
        if event.id:
            self._cursor = event.id
        frame = event.frame
        event_type = frame.get("event_type")
        if event_type == "message.upsert":
            self._rows[frame["id"]] = _normalize_message(frame)
        elif event_type == "message.block":
            row = self._rows.get(frame["message_id"])
            if row is not None:
                content = row.setdefault("content", [])
                index = frame["content_index"]
                if index >= 0:
                    # message.block opens (or completes) a block, so it may
                    # extend the content list — unlike patch/delta.
                    while len(content) <= index:
                        content.append({})
                    content[index] = frame["block"]
        elif event_type == "message.block.patch":
            block = self._block_at(frame["message_id"], frame["content_index"])
            if block is not None:
                if frame.get("status") is not None:
                    block["status"] = frame["status"]
                if "progress" in frame:
                    if frame["progress"] is None:
                        block.pop("progress", None)  # null clears
                    else:
                        block["progress"] = frame["progress"]
                if isinstance(frame.get("resolved_action"), dict):
                    block["resolved_action"] = frame["resolved_action"]
                # progress key absent preserves the existing value
        elif event_type == "message.delta":
            block = self._block_at(frame["message_id"], frame["content_index"])
            if block is not None:
                if frame.get("text"):
                    block["text"] = (block.get("text") or "") + frame["text"]
                if frame.get("thinking"):
                    block["thinking"] = (block.get("thinking") or "") + frame["thinking"]
        elif event_type == "turn.upsert":
            self._turns[frame["id"]] = frame
            if is_terminal_turn_status(frame.get("status", "")):
                self._prune_streaming_rows(frame["id"])
        elif event_type == "stream.ready":
            # Authoritative — adopt unconditionally.
            self._cursor = frame["resume_cursor"]
            self._ready = True
        elif event_type == "stream.end":
            pass  # control frame; the connection loop acts on it
        # unknown event types are ignored (forward-compatible)

    def apply_snapshot(self, snapshot: Any) -> None:
        """Fold a transcript snapshot page (from ``get_session_transcript``).

        Accepts the ``SessionTranscriptSnapshot`` model or an equivalent dict.
        Each message folds in as a ``message.upsert``, each turn as a
        ``turn.upsert``. On the final page (``has_more`` false) the snapshot's
        streaming rows are the complete live set, so any local streaming row
        absent from it is pruned.
        """
        snap = snapshot if isinstance(snapshot, dict) else snapshot.model_dump(mode="json")
        for message in snap["messages"]:
            self._rows[message["id"]] = _normalize_message(message)
        for turn in snap["turns"]:
            self._turns[turn["id"]] = turn
            if is_terminal_turn_status(turn.get("status", "")):
                self._prune_streaming_rows(turn["id"])
        if not snap.get("has_more"):
            live = {m["id"] for m in snap["messages"] if m.get("status") == "streaming"}
            stale = [
                row_id
                for row_id, row in self._rows.items()
                if row.get("status") == "streaming" and row_id not in live
            ]
            for row_id in stale:
                del self._rows[row_id]
        self._cursor = snap["resume_cursor"]

    def seed(self, ack: Any) -> None:
        """Fold a turn-start response into the view.

        Accepts the ``TurnAck`` model or an equivalent dict, folding in the
        caller's message row, the acked turn, and the resume cursor.
        ``Client.invoke_agent`` calls it for you; it is public for callers
        wiring their own transport around a raw invoke.
        """
        data = ack if isinstance(ack, dict) else ack.model_dump(mode="json")
        user_message = data.get("user_message")
        if user_message:
            self._rows[user_message["id"]] = _normalize_message(user_message)
        turn = data.get("turn")
        if turn and turn.get("id"):
            self._turns[turn["id"]] = turn
        if data.get("resume_cursor"):
            self._cursor = data["resume_cursor"]

    def _live_sort_key(self, row: dict[str, Any]) -> tuple[str, str, int]:
        turn_id = row.get("turn_id")
        turn = self._turns.get(turn_id) if turn_id else None
        created_at = (turn or {}).get("created_at") or ""
        return (created_at, turn_id or "", row.get("turn_index") or 0)

    def _block_at(self, message_id: str, index: int) -> dict[str, Any] | None:
        row = self._rows.get(message_id)
        if row is None:
            return None
        content = row.get("content")
        if not isinstance(content, list) or index < 0 or index >= len(content):
            return None
        block = content[index]
        return block if isinstance(block, dict) else None

    def _prune_streaming_rows(self, turn_id: str) -> None:
        stale = [
            row_id
            for row_id, row in self._rows.items()
            if row.get("status") == "streaming" and row.get("turn_id") == turn_id
        ]
        for row_id in stale:
            del self._rows[row_id]


def _normalize_message(message: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(message.get("content"), list):
        message["content"] = []
    return message


def _render_logical_key(message: dict[str, Any]) -> str:
    turn_id = message.get("turn_id")
    if turn_id:
        if message.get("turn_index") is not None:
            return f"logical:{turn_id}:{message.get('role')}:{message['turn_index']}"
        metadata = message.get("metadata")
        if isinstance(metadata, dict) and metadata.get("response_message_index") is not None:
            return (
                f"legacy:{turn_id}:{message.get('role')}:"
                f"{metadata['response_message_index']}"
            )
    return f"id:{message.get('id')}"


def _message_completeness(message: dict[str, Any]) -> int:
    return len(repr(message.get("content") or []))


def _is_empty_assistant_preview(message: dict[str, Any]) -> bool:
    return (
        message.get("status") == "streaming"
        and message.get("role") == "assistant"
        and not message.get("content")
    )


def _deduplicate_tool_blocks(messages: list[dict[str, Any]]) -> None:
    best: dict[str, tuple[int, int, int]] = {}
    for message_index, message in enumerate(messages):
        for block_index, block in enumerate(message.get("content") or []):
            key = _tool_block_key(block)
            if not key:
                continue
            size = len(repr(block))
            if key not in best or size >= best[key][2]:
                best[key] = (message_index, block_index, size)
    for message_index, message in enumerate(messages):
        message["content"] = [
            block
            for block_index, block in enumerate(message.get("content") or [])
            if not (key := _tool_block_key(block))
            or best[key][:2] == (message_index, block_index)
        ]


def _tool_block_key(block: Any) -> str | None:
    if not isinstance(block, dict):
        return None
    if block.get("type") == "tool_use" and block.get("id"):
        return f"use:{block['id']}"
    if block.get("type") == "tool_result" and block.get("tool_use_id"):
        return f"result:{block['tool_use_id']}"
    return None
