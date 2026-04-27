from __future__ import annotations

import hashlib
import hmac
import json
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

import httpx

WEBHOOK_SIGNATURE_HEADER = "X-Mobius-Signature"
WEBHOOK_EVENT_TYPE_HEADER = "X-Mobius-Event-Type"

WEBHOOK_EVENT_RUN_COMPLETED = "run.completed"
WEBHOOK_EVENT_RUN_FAILED = "run.failed"
WEBHOOK_EVENT_PING = "ping"

_SIGNATURE_PREFIX = "sha256="
_SYNTHETIC_WEBHOOK_USER_AGENT = "mobius-sdk-webhook-delivery/1"


class InvalidWebhookSignatureError(ValueError):
    """Raised when a Mobius webhook signature is missing or invalid."""


@dataclass(frozen=True)
class WebhookEvent:
    type: str
    data: Any


@dataclass(frozen=True)
class ParsedSignedWebhookRequest:
    event: WebhookEvent
    body: bytes


@dataclass
class SyntheticWebhookDelivery:
    url: str
    secret: str
    event_type: str
    data: Any
    http_client: httpx.Client | None = None
    headers: Mapping[str, str] | None = None


def sign_webhook_payload(secret: str, body: bytes | str) -> str:
    payload = _body_bytes(body)
    signature = hmac.new(secret.encode(), payload, hashlib.sha256).hexdigest()
    return f"{_SIGNATURE_PREFIX}{signature}"


def verify_webhook_signature(
    secret: str,
    body: bytes | str,
    signature_header: str | None,
) -> None:
    if not secret:
        raise InvalidWebhookSignatureError("secret is empty")
    if not signature_header or not signature_header.startswith(_SIGNATURE_PREFIX):
        raise InvalidWebhookSignatureError("missing sha256 prefix")
    raw = signature_header[len(_SIGNATURE_PREFIX) :]
    try:
        bytes.fromhex(raw)
    except ValueError as exc:
        raise InvalidWebhookSignatureError("signature is not hex") from exc
    expected = sign_webhook_payload(secret, body)
    if not hmac.compare_digest(signature_header, expected):
        raise InvalidWebhookSignatureError("mismatch")


def parse_webhook_event(body: bytes | str) -> WebhookEvent:
    payload = json.loads(_body_bytes(body).decode())
    event_type = payload.get("type")
    if not event_type:
        raise ValueError("mobius: parse webhook event: missing type")
    return WebhookEvent(type=str(event_type), data=payload.get("data"))


def parse_signed_webhook_request(
    body: bytes | str,
    headers: Mapping[str, str],
    secret: str,
) -> ParsedSignedWebhookRequest:
    payload = _body_bytes(body)
    signature = _header_get(headers, WEBHOOK_SIGNATURE_HEADER)
    verify_webhook_signature(secret, payload, signature)
    return ParsedSignedWebhookRequest(event=parse_webhook_event(payload), body=payload)


def build_synthetic_webhook_payload(event_type: str, data: Any) -> bytes:
    if not event_type:
        raise ValueError("mobius: synthetic webhook event type is required")
    return json.dumps(
        {"type": event_type, "data": data},
        separators=(",", ":"),
    ).encode()


def deliver_synthetic_webhook(delivery: SyntheticWebhookDelivery) -> None:
    if not delivery.url:
        raise ValueError("mobius: synthetic webhook URL is required")
    if not delivery.secret:
        raise ValueError("mobius: synthetic webhook secret is required")

    payload = build_synthetic_webhook_payload(delivery.event_type, delivery.data)
    headers = dict(delivery.headers or {})
    headers["Content-Type"] = "application/json"
    headers["User-Agent"] = _SYNTHETIC_WEBHOOK_USER_AGENT
    headers[WEBHOOK_EVENT_TYPE_HEADER] = delivery.event_type
    headers[WEBHOOK_SIGNATURE_HEADER] = sign_webhook_payload(delivery.secret, payload)

    if delivery.http_client is not None:
        resp = delivery.http_client.post(delivery.url, content=payload, headers=headers)
    else:
        with httpx.Client(timeout=60.0) as client:
            resp = client.post(delivery.url, content=payload, headers=headers)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"mobius: synthetic webhook returned {resp.status_code}: {resp.text}"
        )


def _body_bytes(body: bytes | str) -> bytes:
    return body.encode() if isinstance(body, str) else body


def _header_get(headers: Mapping[str, str], key: str) -> str | None:
    for name, value in headers.items():
        if name.lower() == key.lower():
            return value
    return None
