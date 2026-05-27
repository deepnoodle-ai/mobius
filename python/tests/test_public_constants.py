from __future__ import annotations

import deepnoodle.mobius as mobius


def test_interaction_kind_constants_cover_canonical_kinds() -> None:
    assert mobius.INTERACTION_KIND_APPROVAL == mobius.InteractionKind.request_approval
    assert mobius.INTERACTION_KIND_REVIEW == mobius.InteractionKind.request_review
    assert mobius.INTERACTION_KIND_REQUEST == mobius.InteractionKind.request_information
