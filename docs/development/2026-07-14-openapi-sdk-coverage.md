# Recent OpenAPI SDK Coverage

Status: Accepted

## Context

The OpenAPI contract gained blueprint lifecycle, project RBAC, interaction
filtering, transcript/runtime-context, structured wait, and session nudge
features during the three days preceding 2026-07-14. Generated Go transport
code and generated Python/TypeScript models reflect those changes, but the
curated clients and regression tests do not expose or exercise every new
surface consistently.

One TypeScript generator bug also omits schemas whose generated declaration
uses a composed type instead of an object literal. That leaves valid schemas
such as the transcript upsert frames unavailable through `api/index.ts`.

## Goals

- Expose the recent project RBAC, blueprint lifecycle, and interaction-list
  operations through the curated Go, Python, and TypeScript clients.
- Preserve the OpenAPI wire names while presenting each language's established
  option naming conventions.
- Export every generated TypeScript schema, including composed schemas.
- Exercise runtime-context message blocks, structured transcript wait data,
  and the complete session nudge read/cancel lifecycle at the HTTP boundary.
- Remove an unreferenced reusable idempotency-key parameter that cannot produce
  an SDK surface.

## Non-goals

- Reimplement generated request or response models in handwritten code.
- Add behavior not represented in the current OpenAPI contract.
- Change server semantics or introduce compatibility aliases for endpoints
  that have not previously existed in the curated clients.

## Proposal

### Curated client surface

Add thin methods for:

- applying, listing, protecting, and deleting blueprint bindings;
- listing permissions and managing principals, roles, and role assignments;
- listing interactions with all contract filters, including `session_id`.

Request bodies and return values use generated models directly. Handwritten
option types are limited to query parameters and translate idiomatic field
names to the OpenAPI wire names. All methods use the clients' existing request,
authentication, retry, and structured-error paths.

### TypeScript schema exports

Change `gen-api-index.js` to identify every direct key in
`components.schemas` before updating brace depth. The right-hand side is
deliberately ignored, so object, enum, union, intersection, array, and alias
schemas are treated uniformly. A compile-time regression test imports the
previously omitted schemas.

### Contract tests

Add route tests in each SDK for the new curated operations and the session
nudge list/get/cancel methods. Extend transcript fixtures with structured wait
metadata, and add a real session-message response containing a reminder block
so generated decoding is validated instead of only testing model construction.

## Alternatives considered

- **Require callers to use the Go raw client and hand-write Python/TypeScript
  requests.** This keeps the diff smaller but leaves the recommended clients
  with materially different capabilities.
- **Generate full Python and TypeScript operation clients.** This would solve
  transport parity broadly, but it is a larger packaging and API-design change
  than the recent contract coverage requires.
- **Export only the currently missing TypeScript names.** This would hide the
  generator defect and recur for the next composed schema.

## Compatibility and rollout

The changes are additive except for deleting an unused OpenAPI component that
has no references or generated API. Existing methods and model shapes remain
unchanged. Regeneration must be clean, focused SDK tests must pass, and the full
Go/Python/TypeScript test suites must pass before the branch is pushed.
