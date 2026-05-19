from __future__ import annotations

import deepnoodle.mobius as mobius


def test_interaction_kind_constants_cover_canonical_kinds() -> None:
    assert mobius.INTERACTION_KIND_APPROVAL == mobius.InteractionKind.approval
    assert mobius.INTERACTION_KIND_REVIEW == mobius.InteractionKind.review
    assert mobius.INTERACTION_KIND_REQUEST == mobius.InteractionKind.request
    assert mobius.INTERACTION_KIND_VOTE == mobius.InteractionKind.vote
    assert mobius.INTERACTION_KIND_HANDOFF == mobius.InteractionKind.handoff
    assert mobius.INTERACTION_KIND_INPUT == mobius.InteractionKind.input
