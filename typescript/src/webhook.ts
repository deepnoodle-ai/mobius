import { createHmac, timingSafeEqual } from "node:crypto";

export const WEBHOOK_SIGNATURE_HEADER = "X-Mobius-Signature";
export const WEBHOOK_EVENT_TYPE_HEADER = "X-Mobius-Event-Type";

export type WebhookEventType = "run.completed" | "run.failed" | "ping" | string;

export const WEBHOOK_EVENT_RUN_COMPLETED = "run.completed";
export const WEBHOOK_EVENT_RUN_FAILED = "run.failed";
export const WEBHOOK_EVENT_PING = "ping";

const SIGNATURE_PREFIX = "sha256=";
const SYNTHETIC_WEBHOOK_USER_AGENT = "mobius-sdk-webhook-delivery/1";

export class InvalidWebhookSignatureError extends Error {
  constructor(message: string) {
    super(`mobius: invalid webhook signature: ${message}`);
    this.name = "InvalidWebhookSignatureError";
  }
}

export interface WebhookEvent<T = unknown> {
  type: WebhookEventType;
  data: T;
}

export interface ParsedSignedWebhookRequest<T = unknown> {
  event: WebhookEvent<T>;
  body: Uint8Array;
}

export interface SyntheticWebhookDelivery {
  url: string;
  secret: string;
  eventType: WebhookEventType;
  data: unknown;
  fetch?: typeof globalThis.fetch;
  headers?: HeadersInit;
  signal?: AbortSignal;
}

export function signWebhookPayload(
  secret: string,
  body: string | Uint8Array,
): string {
  return `${SIGNATURE_PREFIX}${createHmac("sha256", secret)
    .update(bodyBytes(body))
    .digest("hex")}`;
}

export function verifyWebhookSignature(
  secret: string,
  body: string | Uint8Array,
  signatureHeader: string | null | undefined,
): void {
  if (!secret) {
    throw new InvalidWebhookSignatureError("secret is empty");
  }
  if (!signatureHeader?.startsWith(SIGNATURE_PREFIX)) {
    throw new InvalidWebhookSignatureError("missing sha256 prefix");
  }
  const raw = signatureHeader.slice(SIGNATURE_PREFIX.length);
  if (!/^[0-9a-fA-F]+$/.test(raw) || raw.length % 2 !== 0) {
    throw new InvalidWebhookSignatureError("signature is not hex");
  }
  const got = Buffer.from(raw, "hex");
  const expected = Buffer.from(
    signWebhookPayload(secret, body).slice(SIGNATURE_PREFIX.length),
    "hex",
  );
  if (got.length !== expected.length || !timingSafeEqual(got, expected)) {
    throw new InvalidWebhookSignatureError("mismatch");
  }
}

export function parseWebhookEvent<T = unknown>(
  body: string | Uint8Array,
): WebhookEvent<T> {
  const event = JSON.parse(bodyText(body)) as WebhookEvent<T>;
  if (!event.type) {
    throw new Error("mobius: parse webhook event: missing type");
  }
  return event;
}

export async function parseSignedWebhookRequest<T = unknown>(
  request: Request,
  secret: string,
): Promise<ParsedSignedWebhookRequest<T>> {
  const body = new Uint8Array(await request.arrayBuffer());
  verifyWebhookSignature(
    secret,
    body,
    request.headers.get(WEBHOOK_SIGNATURE_HEADER),
  );
  return { event: parseWebhookEvent<T>(body), body };
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
  if (!delivery.secret) {
    throw new Error("mobius: synthetic webhook secret is required");
  }
  const payload = buildSyntheticWebhookPayload(delivery.eventType, delivery.data);
  const headers = new Headers(delivery.headers);
  headers.set("Content-Type", "application/json");
  headers.set("User-Agent", SYNTHETIC_WEBHOOK_USER_AGENT);
  headers.set(WEBHOOK_EVENT_TYPE_HEADER, delivery.eventType);
  headers.set(WEBHOOK_SIGNATURE_HEADER, signWebhookPayload(delivery.secret, payload));

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

function bodyBytes(body: string | Uint8Array): Uint8Array {
  return typeof body === "string" ? Buffer.from(body) : body;
}

function bodyText(body: string | Uint8Array): string {
  return typeof body === "string" ? body : Buffer.from(body).toString("utf8");
}
