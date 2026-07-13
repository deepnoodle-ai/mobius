import type { RuntimeContextItem } from "./api/index.js";

/** Opts a signed HTTP action response into the Mobius action envelope. */
export const MOBIUS_ACTION_CONTENT_TYPE =
  "application/vnd.mobius.action+json";

/** Canonical response body for a customer-owned signed HTTP action. */
export interface ActionResponseEnvelope<Output = unknown> {
  output?: Output;
  context?: RuntimeContextItem[];
}
