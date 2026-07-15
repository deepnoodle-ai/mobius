from __future__ import annotations

import hashlib
import hmac
import json
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

MOBIUS_SIGNATURE_HEADER = "X-Mobius-Signature"
MOBIUS_SIGNATURE_VERSION_HEADER = "X-Mobius-Signature-Version"
MOBIUS_TIMESTAMP_HEADER = "X-Mobius-Timestamp"
MOBIUS_DELIVERY_ID_HEADER = "X-Mobius-Delivery-Id"
MOBIUS_SECRET_REF_HEADER = "X-Mobius-Secret-Ref"
MOBIUS_SECRET_VERSION_HEADER = "X-Mobius-Secret-Version"

_SIGNATURE_PREFIX = "sha256="
_VERSION = "v1"
_DEFAULT_MAX_AGE_SECONDS = 300


class InvalidSignatureError(ValueError):
    """Raised when a Mobius signed delivery is missing or invalid."""


class StaleDeliveryError(InvalidSignatureError):
    """Raised when a validly shaped signed delivery is outside the freshness window."""


class UnsupportedActionInvocationSchemaError(ValueError):
    """Raised when an action invocation uses an unknown envelope schema."""


class MalformedActionInvocationError(ValueError):
    """Raised when a v1 action invocation is structurally invalid."""


@dataclass(frozen=True)
class DeliveryMeta:
    signature_version: str
    signature: str
    timestamp: int
    delivery_id: str
    secret_ref: str
    secret_version: int


@dataclass(frozen=True)
class VerifiedDelivery(DeliveryMeta):
    body: bytes


@dataclass(frozen=True)
class ActionInvocationScopeV1:
    org_id: str
    project_id: str


@dataclass(frozen=True)
class ActionInvocationActionV1:
    id: str
    name: str


@dataclass(frozen=True)
class ActionInvocationActorV1:
    principal_id: str
    principal_type: str
    agent_id: str | None = None


@dataclass(frozen=True)
class ActionInvocationOriginV1:
    kind: str
    run_id: str | None = None
    channel_exchange_id: str | None = None
    loop_id: str | None = None
    step_key: str | None = None
    agent_turn_id: str | None = None
    session_id: str | None = None
    tool_call_id: str | None = None


@dataclass(frozen=True)
class ActionInvocationContextV1:
    schema_version: int
    scope: ActionInvocationScopeV1
    action: ActionInvocationActionV1
    actor: ActionInvocationActorV1
    origin: ActionInvocationOriginV1


@dataclass(frozen=True)
class ActionInvocationV1:
    mobius: ActionInvocationContextV1
    parameters: dict[str, Any]


@dataclass(frozen=True)
class VerifiedActionInvocationV1(VerifiedDelivery):
    invocation: ActionInvocationV1


def read_delivery_meta(headers: Mapping[str, str]) -> DeliveryMeta:
    signature_version = _required_header(headers, MOBIUS_SIGNATURE_VERSION_HEADER)
    if signature_version != _VERSION:
        raise InvalidSignatureError("unsupported signature version")
    timestamp = _parse_positive_int(
        _required_header(headers, MOBIUS_TIMESTAMP_HEADER),
        "invalid timestamp",
    )
    secret_version = _parse_positive_int(
        _required_header(headers, MOBIUS_SECRET_VERSION_HEADER),
        "invalid secret version",
    )
    return DeliveryMeta(
        signature_version=signature_version,
        signature=_required_header(headers, MOBIUS_SIGNATURE_HEADER),
        timestamp=timestamp,
        delivery_id=_required_header(headers, MOBIUS_DELIVERY_ID_HEADER),
        secret_ref=_required_header(headers, MOBIUS_SECRET_REF_HEADER),
        secret_version=secret_version,
    )


def sign_delivery(
    key: bytes,
    body: bytes | str,
    *,
    delivery_id: str,
    timestamp: int,
) -> str:
    payload = _body_bytes(body)
    canonical = b".".join(
        [
            _VERSION.encode(),
            delivery_id.encode(),
            str(timestamp).encode(),
            payload,
        ]
    )
    signature = hmac.new(key, canonical, hashlib.sha256).hexdigest()
    return f"{_SIGNATURE_PREFIX}{signature}"


def verify_signed_delivery(
    body: bytes | str,
    headers: Mapping[str, str],
    *,
    key: bytes | None = None,
    resolve_key: Callable[[DeliveryMeta], bytes] | None = None,
    max_age: int = _DEFAULT_MAX_AGE_SECONDS,
    now: Callable[[], int] | None = None,
) -> VerifiedDelivery:
    payload = _body_bytes(body)
    meta = read_delivery_meta(headers)
    _verify_freshness(meta.timestamp, max_age=max_age, now=now)
    signing_key = key
    if signing_key is None and resolve_key is not None:
        signing_key = resolve_key(meta)
    if not signing_key:
        raise InvalidSignatureError("signing key is required")
    _verify_signature(signing_key, payload, meta)
    return VerifiedDelivery(
        signature_version=meta.signature_version,
        signature=meta.signature,
        timestamp=meta.timestamp,
        delivery_id=meta.delivery_id,
        secret_ref=meta.secret_ref,
        secret_version=meta.secret_version,
        body=payload,
    )


def verify_action_invocation_v1(
    body: bytes | str,
    headers: Mapping[str, str],
    *,
    key: bytes | None = None,
    resolve_key: Callable[[DeliveryMeta], bytes] | None = None,
    max_age: int = _DEFAULT_MAX_AGE_SECONDS,
    now: Callable[[], int] | None = None,
) -> VerifiedActionInvocationV1:
    """Verify exact raw bytes, then parse and validate the signed v1 envelope."""
    verified = verify_signed_delivery(
        body,
        headers,
        key=key,
        resolve_key=resolve_key,
        max_age=max_age,
        now=now,
    )
    invocation = parse_action_invocation_v1(verified)
    return VerifiedActionInvocationV1(
        signature_version=verified.signature_version,
        signature=verified.signature,
        timestamp=verified.timestamp,
        delivery_id=verified.delivery_id,
        secret_ref=verified.secret_ref,
        secret_version=verified.secret_version,
        body=verified.body,
        invocation=invocation,
    )


def parse_webhook_delivery(v: VerifiedDelivery) -> dict[str, Any]:
    payload = _parse_json_object(v, "webhook delivery")
    event_type = payload.get("type")
    if not isinstance(event_type, str) or not event_type:
        raise ValueError("mobius: webhook delivery missing 'type'")
    return payload


def parse_action_invocation(v: VerifiedDelivery) -> dict[str, Any]:
    return _parse_json_object(v, "action invocation")


def parse_action_invocation_v1(v: VerifiedDelivery) -> ActionInvocationV1:
    try:
        payload = _parse_json_object(v, "action invocation")
    except (UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError) as exc:
        if isinstance(exc, MalformedActionInvocationError):
            raise
        raise MalformedActionInvocationError("mobius: malformed action invocation: invalid JSON object") from exc

    mobius = _required_object(payload, "mobius")
    schema_version = mobius.get("schema_version")
    if schema_version is None:
        raise MalformedActionInvocationError("mobius: malformed action invocation: mobius.schema_version is required")
    if not isinstance(schema_version, int) or isinstance(schema_version, bool):
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.schema_version must be an integer"
        )
    if schema_version != 1:
        raise UnsupportedActionInvocationSchemaError(
            f"mobius: unsupported action invocation schema: {schema_version}"
        )

    scope = _required_object(mobius, "scope")
    action = _required_object(mobius, "action")
    actor = _required_object(mobius, "actor")
    origin = _required_object(mobius, "origin")
    parameters = _required_object(payload, "parameters")

    principal_type = _required_string(actor, "principal_type", "mobius.actor")
    if principal_type not in {"human", "agent", "service", "system"}:
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.actor.principal_type is invalid"
        )
    agent_id = actor.get("agent_id")
    if agent_id is not None and (not isinstance(agent_id, str) or not agent_id.strip()):
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.actor.agent_id must be a non-empty string"
        )
    if principal_type == "agent" and not agent_id:
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.actor.agent_id is required for agent actors"
        )
    if principal_type != "agent" and agent_id is not None:
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.actor.agent_id is only valid for agent actors"
        )

    origin_kind = _required_string(origin, "kind", "mobius.origin")
    if origin_kind not in {
        "agent_tool_call",
        "loop_action_step",
        "direct_action_invoke",
        "server_internal",
    }:
        raise MalformedActionInvocationError(
            "mobius: malformed action invocation: mobius.origin.kind is invalid"
        )

    return ActionInvocationV1(
        mobius=ActionInvocationContextV1(
            schema_version=1,
            scope=ActionInvocationScopeV1(
                org_id=_required_string(scope, "org_id", "mobius.scope"),
                project_id=_required_string(scope, "project_id", "mobius.scope"),
            ),
            action=ActionInvocationActionV1(
                id=_required_string(action, "id", "mobius.action"),
                name=_required_string(action, "name", "mobius.action"),
            ),
            actor=ActionInvocationActorV1(
                principal_id=_required_string(actor, "principal_id", "mobius.actor"),
                principal_type=principal_type,
                agent_id=agent_id,
            ),
            origin=ActionInvocationOriginV1(
                kind=origin_kind,
                run_id=_optional_string(origin, "run_id", "mobius.origin"),
                channel_exchange_id=_optional_string(origin, "channel_exchange_id", "mobius.origin"),
                loop_id=_optional_string(origin, "loop_id", "mobius.origin"),
                step_key=_optional_string(origin, "step_key", "mobius.origin"),
                agent_turn_id=_optional_string(origin, "agent_turn_id", "mobius.origin"),
                session_id=_optional_string(origin, "session_id", "mobius.origin"),
                tool_call_id=_optional_string(origin, "tool_call_id", "mobius.origin"),
            ),
        ),
        parameters=dict(parameters),
    )


def parse_interaction_callback(v: VerifiedDelivery) -> dict[str, Any]:
    return _parse_json_object(v, "interaction callback")


def _parse_json_object(v: VerifiedDelivery, kind: str) -> dict[str, Any]:
    payload = json.loads(v.body.decode())
    if not isinstance(payload, dict):
        raise ValueError(f"mobius: {kind} body must be a JSON object")
    return payload


def _verify_signature(key: bytes, body: bytes, meta: DeliveryMeta) -> None:
    if not meta.signature.startswith(_SIGNATURE_PREFIX):
        raise InvalidSignatureError("missing sha256 prefix")
    raw = meta.signature[len(_SIGNATURE_PREFIX) :]
    try:
        bytes.fromhex(raw)
    except ValueError as exc:
        raise InvalidSignatureError("signature is not hex") from exc
    expected = sign_delivery(
        key,
        body,
        delivery_id=meta.delivery_id,
        timestamp=meta.timestamp,
    )
    if not hmac.compare_digest(meta.signature, expected):
        raise InvalidSignatureError("mismatch")


def _verify_freshness(
    timestamp: int,
    *,
    max_age: int,
    now: Callable[[], int] | None,
) -> None:
    current = now() if now is not None else int(time.time())
    if abs(current - timestamp) > max_age:
        raise StaleDeliveryError("timestamp outside max age")


def _required_object(value: Mapping[str, Any], key: str) -> dict[str, Any]:
    candidate = value.get(key)
    if not isinstance(candidate, dict):
        raise MalformedActionInvocationError(
            f"mobius: malformed action invocation: {key} must be an object"
        )
    return candidate


def _required_string(value: Mapping[str, Any], key: str, path: str) -> str:
    candidate = value.get(key)
    if not isinstance(candidate, str) or not candidate.strip():
        raise MalformedActionInvocationError(
            f"mobius: malformed action invocation: {path}.{key} is required"
        )
    return candidate


def _optional_string(value: Mapping[str, Any], key: str, path: str) -> str | None:
    candidate = value.get(key)
    if candidate is None:
        return None
    if not isinstance(candidate, str) or not candidate.strip():
        raise MalformedActionInvocationError(
            f"mobius: malformed action invocation: {path}.{key} must be a non-empty string"
        )
    return candidate


def _required_header(headers: Mapping[str, str], key: str) -> str:
    for candidate, value in headers.items():
        if candidate.lower() == key.lower() and value:
            return value
    raise InvalidSignatureError(f"missing {key}")


def _parse_positive_int(raw: str, message: str) -> int:
    try:
        value = int(raw)
    except ValueError as exc:
        raise InvalidSignatureError(message) from exc
    if value <= 0:
        raise InvalidSignatureError(message)
    return value


def _body_bytes(body: bytes | str) -> bytes:
    return body.encode() if isinstance(body, str) else body
