import { randomUUID } from "node:crypto";

import {
  MOBIUS_DELIVERY_ID_HEADER,
  MOBIUS_SECRET_REF_HEADER,
  MOBIUS_SECRET_VERSION_HEADER,
  MOBIUS_SIGNATURE_HEADER,
  MOBIUS_SIGNATURE_VERSION_HEADER,
  MOBIUS_TIMESTAMP_HEADER,
  signDelivery,
} from "./signing.js";

export const WEBHOOK_EVENT_TYPE_HEADER = "X-Mobius-Event-Type";

export type WebhookEventType = "run.completed" | "run.failed" | "ping" | string;

export const WEBHOOK_EVENT_RUN_COMPLETED = "run.completed";
export const WEBHOOK_EVENT_RUN_FAILED = "run.failed";
export const WEBHOOK_EVENT_PING = "ping";

const SYNTHETIC_WEBHOOK_USER_AGENT = "mobius-sdk-webhook-delivery/1";

export interface SyntheticWebhookDelivery {
  url: string;
  key: Uint8Array;
  secretRef: string;
  secretVersion: number;
  deliveryId?: string;
  timestamp?: number;
  eventType: WebhookEventType;
  data: unknown;
  fetch?: typeof globalThis.fetch;
  headers?: HeadersInit;
  signal?: AbortSignal;
}

export function buildSyntheticWebhookPayload(
  eventType: WebhookEventType,
  data: unknown,
): string {
  if (!eventType) {
    throw new Error("mobius: synthetic webhook event type is required");
  }
  return JSON.stringify({ type: eventType, data });
}

export async function deliverSyntheticWebhook(
  delivery: SyntheticWebhookDelivery,
): Promise<void> {
  if (!delivery.url) {
    throw new Error("mobius: synthetic webhook URL is required");
  }
  if (!delivery.key?.byteLength) {
    throw new Error("mobius: synthetic webhook signing key is required");
  }
  if (!delivery.secretRef) {
    throw new Error("mobius: synthetic webhook secret ref is required");
  }
  if (!Number.isInteger(delivery.secretVersion) || delivery.secretVersion <= 0) {
    throw new Error("mobius: synthetic webhook secret version is required");
  }
  const payload = buildSyntheticWebhookPayload(delivery.eventType, delivery.data);
  const deliveryId = resolveDeliveryId(delivery.deliveryId);
  const timestamp = resolveTimestamp(delivery.timestamp);
  const headers = new Headers(delivery.headers);
  headers.set("Content-Type", "application/json");
  headers.set("User-Agent", SYNTHETIC_WEBHOOK_USER_AGENT);
  headers.set(WEBHOOK_EVENT_TYPE_HEADER, delivery.eventType);
  headers.set(
    MOBIUS_SIGNATURE_HEADER,
    signDelivery(delivery.key, Buffer.from(payload), {
      deliveryId,
      timestamp,
    }).signature,
  );
  headers.set(MOBIUS_SIGNATURE_VERSION_HEADER, "v1");
  headers.set(MOBIUS_TIMESTAMP_HEADER, String(timestamp));
  headers.set(MOBIUS_DELIVERY_ID_HEADER, deliveryId);
  headers.set(MOBIUS_SECRET_REF_HEADER, delivery.secretRef);
  headers.set(MOBIUS_SECRET_VERSION_HEADER, String(delivery.secretVersion));
  headers.set("Idempotency-Key", deliveryId);

  const fetchFn = delivery.fetch ?? globalThis.fetch;
  const resp = await fetchFn(delivery.url, {
    method: "POST",
    body: payload,
    headers,
    signal: delivery.signal,
  });
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    throw new Error(
      `mobius: synthetic webhook returned ${resp.status}: ${text}`,
    );
  }
}

function resolveDeliveryId(value: string | undefined): string {
  if (value === undefined) {
    return randomUUID();
  }
  if (typeof value !== "string" || value.length === 0) {
    throw new Error("mobius: synthetic webhook deliveryId must be a non-empty string");
  }
  return value;
}

function resolveTimestamp(value: number | undefined): number {
  if (value === undefined) {
    return Math.floor(Date.now() / 1000);
  }
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error("mobius: synthetic webhook timestamp must be a positive integer");
  }
  return value;
}
