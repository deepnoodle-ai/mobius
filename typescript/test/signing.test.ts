import { strict as assert } from "node:assert";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { test } from "node:test";

import {
  MOBIUS_DELIVERY_ID_HEADER,
  MOBIUS_SECRET_REF_HEADER,
  MOBIUS_SECRET_VERSION_HEADER,
  MOBIUS_SIGNATURE_HEADER,
  MOBIUS_SIGNATURE_VERSION_HEADER,
  MOBIUS_TIMESTAMP_HEADER,
  MalformedActionInvocationError,
  StaleDeliveryError,
  UnsupportedActionInvocationSchemaError,
  parseActionInvocationV1,
  signDelivery,
  verifyActionInvocationV1,
  type VerifiedDelivery,
} from "../src/signing.js";

const key = Buffer.from("01234567890123456789012345678901");
const deliveryId = "delivery_fixture_1";
const timestamp = 1710000000;
const signature =
  "sha256=9db53d763fc7bf16d9df33322860b5f1f6fbf77c4c21cdce26c13570d97a61e7";

function headers(value = signature): Headers {
  return new Headers({
    [MOBIUS_SIGNATURE_VERSION_HEADER]: "v1",
    [MOBIUS_SIGNATURE_HEADER]: value,
    [MOBIUS_TIMESTAMP_HEADER]: String(timestamp),
    [MOBIUS_DELIVERY_ID_HEADER]: deliveryId,
    [MOBIUS_SECRET_REF_HEADER]: "mobius/action/act_fixture",
    [MOBIUS_SECRET_VERSION_HEADER]: "3",
  });
}

async function fixture(): Promise<Uint8Array> {
  return readFile(
    resolve(process.cwd(), "../internal/testdata/action-invocation-v1.json"),
  );
}

test("signed action invocation: verifies and parses the shared golden fixture", async () => {
  const body = await fixture();
  assert.equal(
    signDelivery(key, body, { deliveryId, timestamp }).signature,
    signature,
  );

  const verified = await verifyActionInvocationV1(body, headers(), {
    key,
    now: () => timestamp + 5,
  });

  assert.equal(verified.invocation.mobius.scope.orgId, "org_fixture");
  assert.equal(verified.invocation.mobius.scope.projectId, "prj_fixture");
  assert.equal(verified.invocation.mobius.action.id, "act_fixture");
  assert.equal(verified.invocation.mobius.actor.agentId, "agt_fixture");
  assert.equal(verified.invocation.mobius.origin.kind, "agent_tool_call");
  assert.equal(verified.invocation.parameters.document_id, "doc_fixture");
});

test("signed action invocation: reports stale delivery explicitly", async () => {
  await assert.rejects(
    verifyActionInvocationV1(await fixture(), headers(), {
      key,
      now: () => timestamp + 301,
    }),
    StaleDeliveryError,
  );
});

test("signed action invocation: rejects unsupported schemas", () => {
  const delivery: VerifiedDelivery = {
    signatureVersion: "v1",
    signature,
    timestamp,
    deliveryId,
    secretRef: "mobius/action/act_fixture",
    secretVersion: 3,
    body: Buffer.from('{"mobius":{"schema_version":2},"parameters":{}}'),
  };
  assert.throws(
    () => parseActionInvocationV1(delivery),
    UnsupportedActionInvocationSchemaError,
  );
});

test("signed action invocation: enforces the agent identity invariant", () => {
  const delivery: VerifiedDelivery = {
    signatureVersion: "v1",
    signature,
    timestamp,
    deliveryId,
    secretRef: "mobius/action/act_fixture",
    secretVersion: 3,
    body: Buffer.from(
      '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"agent"},"origin":{"kind":"agent_tool_call"}},"parameters":{}}',
    ),
  };
  assert.throws(
    () => parseActionInvocationV1(delivery),
    MalformedActionInvocationError,
  );
});
