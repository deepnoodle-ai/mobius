import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  MOBIUS_ACTION_CONTENT_TYPE,
  type ActionResponseEnvelope,
} from "../src/index.js";

test("action response envelope uses the canonical content type and shape", () => {
  const response: ActionResponseEnvelope<{ ok: boolean }> = {
    output: { ok: true },
    context: [{ name: "board", content: "fresh" }],
  };

  assert.equal(
    MOBIUS_ACTION_CONTENT_TYPE,
    "application/vnd.mobius.action+json",
  );
  assert.deepEqual(response, {
    output: { ok: true },
    context: [{ name: "board", content: "fresh" }],
  });
});
