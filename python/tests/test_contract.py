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
    JobClaim,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    JobHeartbeat,
)

from .conftest import canonicalize, load_fixture, load_manifest

# Map OpenAPI schema name to the Pydantic model. Missing entries fail the test
# so new fixtures must come with a Python binding.
SCHEMA_BINDINGS: dict[str, type[BaseModel]] = {
    "JobClaimRequest": JobClaimRequest,
    "JobClaim": JobClaim,
    "JobFenceRequest": JobFenceRequest,
    "JobHeartbeat": JobHeartbeat,
    "JobCompleteRequest": JobCompleteRequest,
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
