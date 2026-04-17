# Contract fixtures

Canonical JSON wire-format fixtures for the Mobius runtime API. Each of the Go,
TypeScript, and Python SDKs loads these and round-trips them through its own
request/response types to verify wire-format parity across languages.

A fixture is one JSON document containing a single wire payload. `manifest.json`
lists every fixture with the schema it corresponds to and whether it is a
request or response body, so each SDK's contract test can iterate over the set
without hard-coding filenames.

Round-trip rule: after `parse(fixture) -> serialize`, the output must equal the
fixture byte-for-byte once both sides are normalized (same key order, no extra
or missing fields, same scalar encoding). If an SDK cannot represent a field
losslessly, that is a contract bug — fix the SDK, not the fixture.

These fixtures are the source of truth for the hand-written runtime task
endpoints (claim, heartbeat, complete). Do not edit them to match an SDK's
current behavior; edit them to match `openapi.yaml` and fix the SDK.
