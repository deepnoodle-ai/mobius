from pathlib import Path

import pytest

from deepnoodle.mobius import (
    MOBIUS_DELIVERY_ID_HEADER,
    MOBIUS_SECRET_REF_HEADER,
    MOBIUS_SECRET_VERSION_HEADER,
    MOBIUS_SIGNATURE_HEADER,
    MOBIUS_SIGNATURE_VERSION_HEADER,
    MOBIUS_TIMESTAMP_HEADER,
    MalformedActionInvocationError,
    StaleDeliveryError,
    UnsupportedActionInvocationSchemaError,
    VerifiedDelivery,
    parse_action_invocation_v1,
    sign_delivery,
    verify_action_invocation_v1,
)

FIXTURE = Path(__file__).parents[2] / "internal/testdata/action-invocation-v1.json"
NULL_OPTIONALS_FIXTURE = (
    Path(__file__).parents[2] / "internal/testdata/action-invocation-v1-null-optionals.json"
)
KEY = b"01234567890123456789012345678901"
DELIVERY_ID = "delivery_fixture_1"
TIMESTAMP = 1710000000
SIGNATURE = "sha256=9db53d763fc7bf16d9df33322860b5f1f6fbf77c4c21cdce26c13570d97a61e7"


def _headers(signature: str = SIGNATURE) -> dict[str, str]:
    return {
        MOBIUS_SIGNATURE_VERSION_HEADER: "v1",
        MOBIUS_SIGNATURE_HEADER: signature,
        MOBIUS_TIMESTAMP_HEADER: str(TIMESTAMP),
        MOBIUS_DELIVERY_ID_HEADER: DELIVERY_ID,
        MOBIUS_SECRET_REF_HEADER: "mobius/action/act_fixture",
        MOBIUS_SECRET_VERSION_HEADER: "3",
    }


def test_verify_action_invocation_v1_golden_fixture() -> None:
    body = FIXTURE.read_bytes()
    assert sign_delivery(KEY, body, delivery_id=DELIVERY_ID, timestamp=TIMESTAMP) == SIGNATURE

    verified = verify_action_invocation_v1(
        body,
        _headers(),
        key=KEY,
        now=lambda: TIMESTAMP + 5,
    )

    assert verified.invocation.mobius.scope.org_id == "org_fixture"
    assert verified.invocation.mobius.scope.project_id == "prj_fixture"
    assert verified.invocation.mobius.action.id == "act_fixture"
    assert verified.invocation.mobius.actor.agent_id == "agt_fixture"
    assert verified.invocation.mobius.origin.kind == "agent_tool_call"
    assert verified.invocation.parameters["document_id"] == "doc_fixture"


def test_verify_action_invocation_v1_rejects_stale_delivery() -> None:
    with pytest.raises(StaleDeliveryError):
        verify_action_invocation_v1(
            FIXTURE.read_bytes(),
            _headers(),
            key=KEY,
            now=lambda: TIMESTAMP + 301,
        )


def test_parse_action_invocation_v1_accepts_null_optional_strings() -> None:
    invocation = parse_action_invocation_v1(
        VerifiedDelivery(
            "v1", "signature", 1, "delivery", "secret", 1, NULL_OPTIONALS_FIXTURE.read_bytes()
        )
    )
    assert invocation.mobius.actor.agent_id is None
    assert invocation.mobius.origin.run_id is None


@pytest.mark.parametrize(
    ("body", "error"),
    [
        (
            b'{"mobius":{"schema_version":2},"parameters":{}}',
            UnsupportedActionInvocationSchemaError,
        ),
        (
            b'{"mobius":{"schema_version":"1"},"parameters":{}}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":true},"parameters":{}}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"agent"},"origin":{"kind":"agent_tool_call"}},"parameters":{}}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human","agent_id":"agt_1"},"origin":{"kind":"direct_action_invoke"}},"parameters":{}}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}}}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}},"parameters":null}',
            MalformedActionInvocationError,
        ),
        (
            b'{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}},"parameters":[]}',
            MalformedActionInvocationError,
        ),
    ],
)
def test_parse_action_invocation_v1_rejects_invalid_envelopes(
    body: bytes, error: type[ValueError]
) -> None:
    with pytest.raises(error):
        parse_action_invocation_v1(
            VerifiedDelivery("v1", "signature", 1, "delivery", "secret", 1, body)
        )
