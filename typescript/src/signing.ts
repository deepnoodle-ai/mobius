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

export type ActionPrincipalType = "human" | "agent" | "service" | "system";
export type ActionInvocationOriginKind =
  | "agent_tool_call"
  | "loop_action_step"
  | "direct_action_invoke"
  | "server_internal";

export interface ActionInvocationScopeV1 {
  orgId: string;
  projectId: string;
}

export interface ActionInvocationActionV1 {
  id: string;
  name: string;
}

export interface ActionInvocationActorV1 {
  principalId: string;
  principalType: ActionPrincipalType;
  agentId?: string;
}

export interface ActionInvocationOriginV1 {
  kind: ActionInvocationOriginKind;
  runId?: string;
  channelExchangeId?: string;
  loopId?: string;
  stepKey?: string;
  agentTurnId?: string;
  sessionId?: string;
  toolCallId?: string;
}

export interface ActionInvocationContextV1 {
  schemaVersion: 1;
  scope: ActionInvocationScopeV1;
  action: ActionInvocationActionV1;
  actor: ActionInvocationActorV1;
  origin: ActionInvocationOriginV1;
}

export interface ActionInvocationV1 {
  mobius: ActionInvocationContextV1;
  parameters: Record<string, unknown>;
}

export interface VerifiedActionInvocationV1 extends VerifiedDelivery {
  invocation: ActionInvocationV1;
}

export type VerifySignedDeliveryOptions =
  | { key: Uint8Array; maxAge?: number; now?: () => number }
  | {
      resolveKey: (meta: DeliveryMeta) => Promise<Uint8Array> | Uint8Array;
      maxAge?: number;
      now?: () => number;
    };

export class InvalidSignatureError extends Error {
  constructor(message: string) {
    super(`mobius: invalid signed delivery: ${message}`);
    this.name = "InvalidSignatureError";
  }
}

export class StaleDeliveryError extends InvalidSignatureError {
  constructor(message: string) {
    super(message);
    this.name = "StaleDeliveryError";
  }
}

export class UnsupportedActionInvocationSchemaError extends Error {
  constructor(schemaVersion: unknown) {
    super(
      `mobius: unsupported action invocation schema: ${String(schemaVersion)}`,
    );
    this.name = "UnsupportedActionInvocationSchemaError";
  }
}

export class MalformedActionInvocationError extends Error {
  constructor(message: string) {
    super(`mobius: malformed action invocation: ${message}`);
    this.name = "MalformedActionInvocationError";
  }
}

export function readDeliveryMeta(request: Request): DeliveryMeta {
  return readDeliveryMetaFromHeaders(request.headers);
}

export function readDeliveryMetaFromHeaders(input: HeadersInit): DeliveryMeta {
  const headers = new Headers(input);
  const signatureVersion = requiredHeader(
    headers,
    MOBIUS_SIGNATURE_VERSION_HEADER,
  );
  if (signatureVersion !== VERSION) {
    throw new InvalidSignatureError("unsupported signature version");
  }
  const timestamp = Number(requiredHeader(headers, MOBIUS_TIMESTAMP_HEADER));
  if (!Number.isInteger(timestamp) || timestamp <= 0) {
    throw new InvalidSignatureError("invalid timestamp");
  }
  const secretVersion = Number(
    requiredHeader(headers, MOBIUS_SECRET_VERSION_HEADER),
  );
  if (!Number.isInteger(secretVersion) || secretVersion <= 0) {
    throw new InvalidSignatureError("invalid secret version");
  }
  return {
    signatureVersion,
    signature: requiredHeader(headers, MOBIUS_SIGNATURE_HEADER),
    timestamp,
    deliveryId: requiredHeader(headers, MOBIUS_DELIVERY_ID_HEADER),
    secretRef: requiredHeader(headers, MOBIUS_SECRET_REF_HEADER),
    secretVersion,
  };
}

export async function verifySignedDelivery(
  request: Request,
  opts: VerifySignedDeliveryOptions,
): Promise<VerifiedDelivery> {
  const body = new Uint8Array(await request.arrayBuffer());
  return verifySignedDeliveryBytes(body, request.headers, opts);
}

export async function verifySignedDeliveryBytes(
  body: Uint8Array,
  headers: HeadersInit,
  opts: VerifySignedDeliveryOptions,
): Promise<VerifiedDelivery> {
  const meta = readDeliveryMetaFromHeaders(headers);
  verifyFreshness(meta, opts);
  const key = "key" in opts ? opts.key : await opts.resolveKey(meta);
  if (!key?.byteLength) {
    throw new InvalidSignatureError("signing key is required");
  }
  verifySignature(key, body, meta);
  return { ...meta, body };
}

export async function verifyActionInvocationV1(
  body: Uint8Array,
  headers: HeadersInit,
  opts: VerifySignedDeliveryOptions,
): Promise<VerifiedActionInvocationV1> {
  const verified = await verifySignedDeliveryBytes(body, headers, opts);
  return { ...verified, invocation: parseActionInvocationV1(verified) };
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

export function parseActionInvocationV1(
  v: VerifiedDelivery,
): ActionInvocationV1 {
  let payload: unknown;
  try {
    payload = JSON.parse(Buffer.from(v.body).toString("utf8"));
  } catch (error) {
    throw new MalformedActionInvocationError(
      `invalid JSON: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  const root = requiredObject(payload, "body");
  const mobius = requiredObject(root.mobius, "mobius");
  if (mobius.schema_version === undefined) {
    throw new MalformedActionInvocationError(
      "mobius.schema_version is required",
    );
  }
  if (mobius.schema_version !== 1) {
    throw new UnsupportedActionInvocationSchemaError(mobius.schema_version);
  }
  const scope = requiredObject(mobius.scope, "mobius.scope");
  const action = requiredObject(mobius.action, "mobius.action");
  const actor = requiredObject(mobius.actor, "mobius.actor");
  const origin = requiredObject(mobius.origin, "mobius.origin");
  const parameters = requiredObject(root.parameters, "parameters");

  const principalType = requiredString(
    actor.principal_type,
    "mobius.actor.principal_type",
  );
  if (!isActionPrincipalType(principalType)) {
    throw new MalformedActionInvocationError(
      "mobius.actor.principal_type is invalid",
    );
  }
  const agentId = optionalString(actor.agent_id, "mobius.actor.agent_id");
  if (principalType === "agent" && agentId === undefined) {
    throw new MalformedActionInvocationError(
      "mobius.actor.agent_id is required for agent actors",
    );
  }
  if (principalType !== "agent" && agentId !== undefined) {
    throw new MalformedActionInvocationError(
      "mobius.actor.agent_id is only valid for agent actors",
    );
  }
  const originKind = requiredString(origin.kind, "mobius.origin.kind");
  if (!isActionInvocationOriginKind(originKind)) {
    throw new MalformedActionInvocationError("mobius.origin.kind is invalid");
  }

  return {
    mobius: {
      schemaVersion: 1,
      scope: {
        orgId: requiredString(scope.org_id, "mobius.scope.org_id"),
        projectId: requiredString(scope.project_id, "mobius.scope.project_id"),
      },
      action: {
        id: requiredString(action.id, "mobius.action.id"),
        name: requiredString(action.name, "mobius.action.name"),
      },
      actor: {
        principalId: requiredString(
          actor.principal_id,
          "mobius.actor.principal_id",
        ),
        principalType,
        ...(agentId === undefined ? {} : { agentId }),
      },
      origin: {
        kind: originKind,
        ...optionalOriginFields(origin),
      },
    },
    parameters,
  };
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
    throw new StaleDeliveryError("timestamp outside max age");
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

function requiredHeader(headers: Headers, name: string): string {
  const value = headers.get(name);
  if (!value) {
    throw new InvalidSignatureError(`missing ${name}`);
  }
  return value;
}

function requiredObject(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new MalformedActionInvocationError(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

function requiredString(value: unknown, path: string): string {
  if (typeof value !== "string" || !value.trim()) {
    throw new MalformedActionInvocationError(`${path} is required`);
  }
  return value;
}

function optionalString(value: unknown, path: string): string | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "string" || !value.trim()) {
    throw new MalformedActionInvocationError(
      `${path} must be a non-empty string`,
    );
  }
  return value;
}

function isActionPrincipalType(value: string): value is ActionPrincipalType {
  return ["human", "agent", "service", "system"].includes(value);
}

function isActionInvocationOriginKind(
  value: string,
): value is ActionInvocationOriginKind {
  return [
    "agent_tool_call",
    "loop_action_step",
    "direct_action_invoke",
    "server_internal",
  ].includes(value);
}

function optionalOriginFields(
  origin: Record<string, unknown>,
): Omit<ActionInvocationOriginV1, "kind"> {
  const fields: Omit<ActionInvocationOriginV1, "kind"> = {};
  const mappings = [
    ["run_id", "runId"],
    ["channel_exchange_id", "channelExchangeId"],
    ["loop_id", "loopId"],
    ["step_key", "stepKey"],
    ["agent_turn_id", "agentTurnId"],
    ["session_id", "sessionId"],
    ["tool_call_id", "toolCallId"],
  ] as const;
  for (const [wireName, fieldName] of mappings) {
    const value = optionalString(origin[wireName], `mobius.origin.${wireName}`);
    if (value !== undefined) fields[fieldName] = value;
  }
  return fields;
}
