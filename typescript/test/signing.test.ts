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
  verifySignedDeliveryBytes,
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

async function fixture(
  name = "action-invocation-v1.json",
): Promise<Uint8Array> {
  return readFile(resolve(process.cwd(), `../internal/testdata/${name}`));
}

function delivery(body: Uint8Array): VerifiedDelivery {
  return {
    signatureVersion: "v1",
    signature,
    timestamp,
    deliveryId,
    secretRef: "mobius/action/act_fixture",
    secretVersion: 3,
    body,
  };
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

test("signed delivery: snapshots the authenticated body", async () => {
  const body = Buffer.from('{"ok":true}');
  const bodySignature = signDelivery(key, body, {
    deliveryId,
    timestamp,
  }).signature;
  const verified = await verifySignedDeliveryBytes(
    body,
    headers(bodySignature),
    { key, now: () => timestamp + 5 },
  );
  const want = Buffer.from(body);
  body[0] = "x".charCodeAt(0);
  assert.deepEqual(Buffer.from(verified.body), want);
});

test("signed action invocation: accepts null optional strings", async () => {
  const invocation = parseActionInvocationV1(
    delivery(await fixture("action-invocation-v1-null-optionals.json")),
  );
  assert.equal(invocation.mobius.actor.agentId, undefined);
  assert.equal(invocation.mobius.origin.runId, undefined);
});

test("signed action invocation: rejects unsupported schemas", () => {
  assert.throws(
    () =>
      parseActionInvocationV1(
        delivery(
          Buffer.from('{"mobius":{"schema_version":2},"parameters":{}}'),
        ),
      ),
    UnsupportedActionInvocationSchemaError,
  );
});

for (const [name, body] of [
  [
    "string schema version",
    '{"mobius":{"schema_version":"1"},"parameters":{}}',
  ],
  [
    "boolean schema version",
    '{"mobius":{"schema_version":true},"parameters":{}}',
  ],
  [
    "agent missing agent_id",
    '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"agent"},"origin":{"kind":"agent_tool_call"}},"parameters":{}}',
  ],
  [
    "human with agent_id",
    '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human","agent_id":"agt_1"},"origin":{"kind":"direct_action_invoke"}},"parameters":{}}',
  ],
  [
    "missing parameters",
    '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}}}',
  ],
  [
    "null parameters",
    '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}},"parameters":null}',
  ],
  [
    "array parameters",
    '{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}},"parameters":[]}',
  ],
] as const) {
  test(`signed action invocation: rejects ${name}`, () => {
    assert.throws(
      () => parseActionInvocationV1(delivery(Buffer.from(body))),
      MalformedActionInvocationError,
    );
  });
}
