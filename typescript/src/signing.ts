import { createHmac, timingSafeEqual } from "node:crypto";

export const MOBIUS_SIGNATURE_HEADER = "X-Mobius-Signature";
export const MOBIUS_SIGNATURE_VERSION_HEADER = "X-Mobius-Signature-Version";
export const MOBIUS_TIMESTAMP_HEADER = "X-Mobius-Timestamp";
export const MOBIUS_DELIVERY_ID_HEADER = "X-Mobius-Delivery-Id";
export const MOBIUS_SECRET_REF_HEADER = "X-Mobius-Secret-Ref";
export const MOBIUS_SECRET_VERSION_HEADER = "X-Mobius-Secret-Version";

const SIGNATURE_PREFIX = "sha256=";
const VERSION = "v1";
const DEFAULT_MAX_AGE_SECONDS = 300;

export interface DeliveryMeta {
  signatureVersion: string;
  signature: string;
  timestamp: number;
  deliveryId: string;
  secretRef: string;
  secretVersion: number;
}

export interface VerifiedDelivery extends DeliveryMeta {
  body: Uint8Array;
}

export class InvalidSignatureError extends Error {
  constructor(message: string) {
    super(`mobius: invalid signed delivery: ${message}`);
    this.name = "InvalidSignatureError";
  }
}

export function readDeliveryMeta(request: Request): DeliveryMeta {
  const signatureVersion = requiredHeader(
    request,
    MOBIUS_SIGNATURE_VERSION_HEADER,
  );
  if (signatureVersion !== VERSION) {
    throw new InvalidSignatureError("unsupported signature version");
  }
  const timestamp = Number(requiredHeader(request, MOBIUS_TIMESTAMP_HEADER));
  if (!Number.isFinite(timestamp) || timestamp <= 0) {
    throw new InvalidSignatureError("invalid timestamp");
  }
  const secretVersion = Number(
    requiredHeader(request, MOBIUS_SECRET_VERSION_HEADER),
  );
  if (!Number.isInteger(secretVersion) || secretVersion <= 0) {
    throw new InvalidSignatureError("invalid secret version");
  }
  return {
    signatureVersion,
    signature: requiredHeader(request, MOBIUS_SIGNATURE_HEADER),
    timestamp,
    deliveryId: requiredHeader(request, MOBIUS_DELIVERY_ID_HEADER),
    secretRef: requiredHeader(request, MOBIUS_SECRET_REF_HEADER),
    secretVersion,
  };
}

export async function verifySignedDelivery(
  request: Request,
  opts:
    | { key: Uint8Array; maxAge?: number; now?: () => number }
    | {
        resolveKey: (meta: DeliveryMeta) => Promise<Uint8Array> | Uint8Array;
        maxAge?: number;
        now?: () => number;
      },
): Promise<VerifiedDelivery> {
  const meta = readDeliveryMeta(request);
  verifyFreshness(meta, opts);
  const body = new Uint8Array(await request.arrayBuffer());
  const key = "key" in opts ? opts.key : await opts.resolveKey(meta);
  if (!key?.byteLength) {
    throw new InvalidSignatureError("signing key is required");
  }
  verifySignature(key, body, meta);
  return { ...meta, body };
}

export function signDelivery(
  key: Uint8Array,
  body: Uint8Array,
  opts: { deliveryId: string; timestamp: number },
): { signature: string } {
  const canonical = Buffer.concat([
    Buffer.from(`${VERSION}.${opts.deliveryId}.${opts.timestamp}.`),
    Buffer.from(body),
  ]);
  return {
    signature: `${SIGNATURE_PREFIX}${createHmac("sha256", key)
      .update(canonical)
      .digest("hex")}`,
  };
}

export function parseWebhookDelivery<T>(v: VerifiedDelivery): {
  type: string;
  data: T;
} {
  return JSON.parse(Buffer.from(v.body).toString("utf8")) as {
    type: string;
    data: T;
  };
}

export function parseActionInvocation<T>(v: VerifiedDelivery): T {
  return JSON.parse(Buffer.from(v.body).toString("utf8")) as T;
}

export function parseInteractionCallback<T>(v: VerifiedDelivery): T {
  return JSON.parse(Buffer.from(v.body).toString("utf8")) as T;
}

function verifyFreshness(
  meta: DeliveryMeta,
  opts: { maxAge?: number; now?: () => number },
): void {
  const now = opts.now?.() ?? Math.floor(Date.now() / 1000);
  const maxAge = opts.maxAge ?? DEFAULT_MAX_AGE_SECONDS;
  if (Math.abs(now - meta.timestamp) > maxAge) {
    throw new InvalidSignatureError("timestamp outside max age");
  }
}

function verifySignature(
  key: Uint8Array,
  body: Uint8Array,
  meta: DeliveryMeta,
): void {
  if (!meta.signature.startsWith(SIGNATURE_PREFIX)) {
    throw new InvalidSignatureError("missing sha256 prefix");
  }
  const raw = meta.signature.slice(SIGNATURE_PREFIX.length);
  if (!/^[0-9a-fA-F]+$/.test(raw) || raw.length % 2 !== 0) {
    throw new InvalidSignatureError("signature is not hex");
  }
  const got = Buffer.from(raw, "hex");
  const expected = Buffer.from(
    signDelivery(key, body, {
      deliveryId: meta.deliveryId,
      timestamp: meta.timestamp,
    }).signature.slice(SIGNATURE_PREFIX.length),
    "hex",
  );
  if (got.length !== expected.length || !timingSafeEqual(got, expected)) {
    throw new InvalidSignatureError("mismatch");
  }
}

function requiredHeader(request: Request, name: string): string {
  const value = request.headers.get(name);
  if (!value) {
    throw new InvalidSignatureError(`missing ${name}`);
  }
  return value;
}
