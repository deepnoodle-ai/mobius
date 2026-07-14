"""Cross-language contract tests.

Each fixture in <repo>/testdata/contract is round-tripped through the
corresponding Pydantic model and compared to the original as a generic JSON
value. Parity with the Go and TypeScript SDKs is guaranteed when every
language's contract test passes against the same fixture set.
"""

from __future__ import annotations

from typing import Any

import pytest
from pydantic import BaseModel

from deepnoodle.mobius._api.models import (
    WorkerSocketGenerationDeltaFrame,
    WorkerSocketJobCancelFrame,
    WorkerSocketJobHeartbeatAckFrame,
    WorkerSocketJobHeartbeatFrame,
    WorkerSocketJobReportFrame,
    WorkerSocketJobsClaimFrame,
    WorkerSocketJobsClaimedFrame,
    WorkerSocketRegisterFrame,
    WorkerSocketRegisteredFrame,
)
from deepnoodle.mobius import (
    SessionTranscript,
    TranscriptStreamEvent,
    normalize_tool_use,
    tool_result_text,
)

from .conftest import canonicalize, load_fixture, load_manifest

# Map OpenAPI schema name to the Pydantic model. Missing entries fail the test
# so new fixtures must come with a Python binding.
SCHEMA_BINDINGS: dict[str, type[BaseModel]] = {
    "WorkerSocketRegisterFrame": WorkerSocketRegisterFrame,
    "WorkerSocketRegisteredFrame": WorkerSocketRegisteredFrame,
    "WorkerSocketJobsClaimFrame": WorkerSocketJobsClaimFrame,
    "WorkerSocketJobsClaimedFrame": WorkerSocketJobsClaimedFrame,
    "WorkerSocketJobHeartbeatFrame": WorkerSocketJobHeartbeatFrame,
    "WorkerSocketJobHeartbeatAckFrame": WorkerSocketJobHeartbeatAckFrame,
    "WorkerSocketJobReportFrame": WorkerSocketJobReportFrame,
    "WorkerSocketGenerationDeltaFrame": WorkerSocketGenerationDeltaFrame,
    "WorkerSocketJobCancelFrame": WorkerSocketJobCancelFrame,
}


def _round_trip(model: type[BaseModel], raw: Any) -> Any:
    """Parse raw JSON into the model and dump it back, mirroring Client._dump.

    exclude_none=True matches how mobius.client.Client serializes outbound
    bodies, so the round-trip reflects what actually goes on the wire.
    """
    parsed = model.model_validate(raw)
    return parsed.model_dump(mode="json", exclude_none=True)


@pytest.mark.parametrize(
    "fixture",
    load_manifest(),
    ids=lambda f: f["file"],
)
def test_contract_round_trip(fixture: dict[str, str]) -> None:
    if fixture["kind"] != "websocket_frame":
        pytest.skip("covered by the transcript contract driver")
    model = SCHEMA_BINDINGS.get(fixture["schema"])
    assert model is not None, (
        f"no Python binding for schema {fixture['schema']!r} "
        f"(update SCHEMA_BINDINGS in test_contract.py)"
    )

    raw = load_fixture(fixture["file"])
    actual = _round_trip(model, raw)

    assert canonicalize(actual) == canonicalize(raw), (
        f"{fixture['schema']} round-trip mismatch\n"
        f"original:   {raw}\n"
        f"roundtrip:  {actual}"
    )


def test_transcript_frame_contract() -> None:
    fixture = load_fixture("transcript_frames.json")
    transcript = SessionTranscript()
    for event in fixture["events"]:
        frame = event["frame"]
        transcript.apply(
            TranscriptStreamEvent(
                event_type=frame["event_type"], frame=frame, id=event.get("id")
            )
        )

    expected = fixture["expected"]
    assert transcript.cursor == expected["cursor"]
    assert [row["id"] for row in transcript.messages()] == expected["message_ids"]
    visible = transcript.renderable_messages()
    assert [row["id"] for row in visible] == expected["renderable_ids"]
    assert [
        row["id"]
        for row in transcript.renderable_messages_for_turn(expected["renderable_turn_id"])
    ] == expected["renderable_turn_ids"]
    assert transcript.message(expected["null_content_id"])["content"] == []

    resolved_message = transcript.message(expected["resolved_action_message_id"])
    normalized = normalize_tool_use(resolved_message["content"][0])
    assert normalized.resolved_action["name"] == expected["resolved_action_name"]
    shape_message = transcript.message(expected["tool_shape_message_id"])
    flat = normalize_tool_use(shape_message["content"][0])
    meta = normalize_tool_use(shape_message["content"][1])
    help_call = normalize_tool_use(shape_message["content"][2])
    assert flat.resolved_action["name"] == expected["flat_resolved_action_name"]
    assert meta.resolved_action["name"] == expected["meta_resolved_action_name"]
    assert help_call.wire_name == expected["help_wire_name"]
    assert help_call.resolved_action is None
    final = next(row for row in visible if row["id"] == "m_final")
    assert len(final["content"]) == expected["deduped_tool_block_count"]
    result_message = transcript.message(expected["tool_result_text_message_id"])
    assert tool_result_text(result_message["content"][0]) == expected["tool_result_text"]

    waiting = transcript.turn(expected["waiting_turn_id"])
    assert waiting["wait"]["interaction_id"] == expected["wait_interaction_id"]
    assert waiting["wait"]["tool_call_id"] == expected["wait_tool_call_id"]

    failed = transcript.turn(expected["failed_turn_id"])
    assert failed["error_type"] == expected["failed_turn_error_type"]
    assert failed["error_message"] == expected["failed_turn_error_message"]


def test_transcript_snapshot_contract() -> None:
    fixture = load_fixture("transcript_snapshot.json")
    transcript = SessionTranscript()
    for page in fixture["pages"]:
        transcript.apply_snapshot(page)

    expected = fixture["expected"]
    assert transcript.cursor == expected["cursor"]
    assert [row["id"] for row in transcript.messages()] == expected["message_ids"]
    assert [row["id"] for row in transcript.renderable_messages()] == expected[
        "renderable_ids"
    ]
    assert transcript.turn(expected["turn_id"])["status"] == expected["turn_status"]
