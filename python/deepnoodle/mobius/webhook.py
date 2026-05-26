from __future__ import annotations

import json
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

import httpx

from .signing import (
    MOBIUS_DELIVERY_ID_HEADER,
    MOBIUS_SECRET_REF_HEADER,
    MOBIUS_SECRET_VERSION_HEADER,
    MOBIUS_SIGNATURE_HEADER,
    MOBIUS_SIGNATURE_VERSION_HEADER,
    MOBIUS_TIMESTAMP_HEADER,
    sign_delivery,
)

WEBHOOK_EVENT_TYPE_HEADER = "X-Mobius-Event-Type"

WEBHOOK_EVENT_RUN_COMPLETED = "run.completed"
WEBHOOK_EVENT_RUN_FAILED = "run.failed"
WEBHOOK_EVENT_PING = "ping"

_SYNTHETIC_WEBHOOK_USER_AGENT = "mobius-sdk-webhook-delivery/1"


@dataclass
class SyntheticWebhookDelivery:
    url: str
    key: bytes
    secret_ref: str
    secret_version: int
    event_type: str
    data: Any
    delivery_id: str | None = None
    timestamp: int | None = None
    http_client: httpx.Client | None = None
    headers: Mapping[str, str] | None = None


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
    if not delivery.key:
        raise ValueError("mobius: synthetic webhook signing key is required")
    if not delivery.secret_ref:
        raise ValueError("mobius: synthetic webhook secret ref is required")
    if delivery.secret_version <= 0:
        raise ValueError("mobius: synthetic webhook secret version is required")

    payload = build_synthetic_webhook_payload(delivery.event_type, delivery.data)
    delivery_id = delivery.delivery_id or str(uuid.uuid4())
    timestamp = delivery.timestamp or int(time.time())
    headers = dict(delivery.headers or {})
    headers["Content-Type"] = "application/json"
    headers["User-Agent"] = _SYNTHETIC_WEBHOOK_USER_AGENT
    headers[WEBHOOK_EVENT_TYPE_HEADER] = delivery.event_type
    headers[MOBIUS_SIGNATURE_HEADER] = sign_delivery(
        delivery.key,
        payload,
        delivery_id=delivery_id,
        timestamp=timestamp,
    )
    headers[MOBIUS_SIGNATURE_VERSION_HEADER] = "v1"
    headers[MOBIUS_TIMESTAMP_HEADER] = str(timestamp)
    headers[MOBIUS_DELIVERY_ID_HEADER] = delivery_id
    headers[MOBIUS_SECRET_REF_HEADER] = delivery.secret_ref
    headers[MOBIUS_SECRET_VERSION_HEADER] = str(delivery.secret_version)
    headers["Idempotency-Key"] = delivery_id

    if delivery.http_client is not None:
        resp = delivery.http_client.post(delivery.url, content=payload, headers=headers)
    else:
        with httpx.Client(timeout=60.0) as client:
            resp = client.post(delivery.url, content=payload, headers=headers)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"mobius: synthetic webhook returned {resp.status_code}: {resp.text}"
        )
