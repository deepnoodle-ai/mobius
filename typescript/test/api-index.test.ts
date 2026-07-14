import { strict as assert } from "node:assert";
import { test } from "node:test";

import type {
  InteractionUpsertFrame,
  InteractionValue,
  MessageUpsertFrame,
  TurnUpsertFrame,
} from "../src/api/index.js";

test("api index exports composed transcript schemas", () => {
  const frames: Array<
    | InteractionUpsertFrame
    | InteractionValue
    | MessageUpsertFrame
    | TurnUpsertFrame
  > = [];
  assert.equal(frames.length, 0);
});
