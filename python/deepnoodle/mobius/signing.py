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


def parse_webhook_delivery(v: VerifiedDelivery) -> dict[str, Any]:
    return json.loads(v.body.decode())


def parse_action_invocation(v: VerifiedDelivery) -> dict[str, Any]:
    return json.loads(v.body.decode())


def parse_interaction_callback(v: VerifiedDelivery) -> dict[str, Any]:
    return json.loads(v.body.decode())


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
        raise InvalidSignatureError("timestamp outside max age")


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
