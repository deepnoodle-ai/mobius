from deepnoodle.mobius import (
    MOBIUS_ACTION_CONTENT_TYPE,
    ActionResponseEnvelope,
    RuntimeContextItem,
)


def test_action_response_envelope_uses_canonical_content_type_and_shape() -> None:
    response: ActionResponseEnvelope = {
        "output": {"ok": True},
        "context": [RuntimeContextItem(name="board", content="fresh")],
    }

    assert MOBIUS_ACTION_CONTENT_TYPE == "application/vnd.mobius.action+json"
    assert response["output"] == {"ok": True}
    assert response["context"][0].model_dump() == {
        "name": "board",
        "content": "fresh",
    }
