"""Session-transcript v2 reducer and stream helpers.

The reducer is the session-scope analogue of Dive's ``ResponseAccumulator``:
it folds session-transcript stream frames (or snapshot pages) into the
authoritative view. Frames are plain decoded JSON objects (``dict``) — the same
open representation the SDK already uses for ``SessionStreamEvent.data`` — so
unknown fields and enum values round-trip untouched.
"""

from __future__ import annotations

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
    # Opaque resume cursor for the first connection.
    cursor: str | None = None
    # Pause before reconnecting after a dropped connection (not a clean
    # rotate), in seconds.
    reconnect_delay: float = 1.0


class SessionTranscriptReducer:
    """Folds session-transcript v2 frames (or snapshot pages) into the view.

    The whole merge is ``rows[id] = row``: state frames carry absolute state,
    so last write wins and nothing is an increment except ``message.delta``
    text. Ignoring deltas entirely still converges. The reducer owns protocol
    logic only; the connection loop (reconnect on ``stream.end`` rotate, stop
    on idle) lives in :meth:`Client.watch_session_transcript`.

    Not safe for concurrent use; drive it from a single thread.
    """

    def __init__(self) -> None:
        # Message rows keyed by their immutable id.
        self.rows: dict[str, dict[str, Any]] = {}
        # Turns keyed by id.
        self.turns: dict[str, dict[str, Any]] = {}
        # Opaque resume cursor; never parse it.
        self.cursor: str | None = None
        # True once stream.ready has been seen — safe to render.
        self.ready: bool = False

    def apply(self, frame: dict[str, Any], sse_id: str | None = None) -> None:
        """Fold one stream frame into the view.

        ``sse_id`` is the frame's SSE ``id:`` line when present; it advances the
        cursor. Unknown ``event_type`` values are ignored so the protocol can
        grow without breaking this client.
        """
        if sse_id:
            self.cursor = sse_id
        event_type = frame.get("event_type")
        if event_type == "message.upsert":
            self.rows[frame["id"]] = frame
        elif event_type == "message.block":
            row = self.rows.get(frame["message_id"])
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
                # progress key absent preserves the existing value
        elif event_type == "message.delta":
            block = self._block_at(frame["message_id"], frame["content_index"])
            if block is not None:
                if frame.get("text"):
                    block["text"] = (block.get("text") or "") + frame["text"]
                if frame.get("thinking"):
                    block["thinking"] = (block.get("thinking") or "") + frame["thinking"]
        elif event_type == "turn.upsert":
            self.turns[frame["id"]] = frame
            if is_terminal_turn_status(frame.get("status", "")):
                self._prune_streaming_rows(frame["id"])
        elif event_type == "stream.ready":
            # Authoritative — adopt unconditionally.
            self.cursor = frame["resume_cursor"]
            self.ready = True
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
            self.rows[message["id"]] = message
        for turn in snap["turns"]:
            self.turns[turn["id"]] = turn
            if is_terminal_turn_status(turn.get("status", "")):
                self._prune_streaming_rows(turn["id"])
        if not snap.get("has_more"):
            live = {m["id"] for m in snap["messages"] if m.get("status") == "streaming"}
            stale = [
                row_id
                for row_id, row in self.rows.items()
                if row.get("status") == "streaming" and row_id not in live
            ]
            for row_id in stale:
                del self.rows[row_id]
        self.cursor = snap["resume_cursor"]

    def messages(self) -> list[dict[str, Any]]:
        """Rows in render order.

        Final rows are ordered by ``sequence``, then streaming rows by
        ``(turn.created_at, turn.id, turn_index)`` — ``turn_index`` alone is
        unique only within one turn, and turns can run concurrently.
        """
        rows = list(self.rows.values())
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

    def _live_sort_key(self, row: dict[str, Any]) -> tuple[str, str, int]:
        turn_id = row.get("turn_id")
        turn = self.turns.get(turn_id) if turn_id else None
        created_at = (turn or {}).get("created_at") or ""
        return (created_at, turn_id or "", row.get("turn_index") or 0)

    def _block_at(self, message_id: str, index: int) -> dict[str, Any] | None:
        row = self.rows.get(message_id)
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
            for row_id, row in self.rows.items()
            if row.get("status") == "streaming" and row.get("turn_id") == turn_id
        ]
        for row_id in stale:
            del self.rows[row_id]
