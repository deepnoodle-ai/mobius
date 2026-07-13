# Immutable Terminal Transcript Resume Cursor

**Status:** Accepted
**Author:** Codex
**Date:** 2026-07-13
**Workflow:** #3 — spec first, then build while the Nabaname retest remains the
external acceptance case.

## Context

Mobius 0.0.46 reconciles a durable transcript snapshot before exposing a
terminal turn update, but it starts that read from the moving live cursor. A
live cursor can advance past durable message sequences represented only by
streaming rows in memory. Applying the terminal turn prunes those rows, and an
incremental read from the advanced cursor cannot restore their durable
replacements. Nabaname reproduced this with two completed tool calls absent
from the terminal projection but present in an immediate snapshot.

## Proposal

Each `TurnTranscript` will retain the acknowledgement's immutable
`resume_cursor` separately from the transcript's moving reconnect cursor:

- `TurnAck.resume_cursor` is a stable lower boundary that the API guarantees
  precedes every durable row owned by the returned turn. Fresh and deduplicated
  invocations both receive a safe boundary.
- Continue using `SessionTranscript.cursor` for stream reconnects and
  diagnostics.
- On a terminal `turn.upsert`, drain all incremental snapshot pages from the
  immutable cursor, apply them, then expose the terminal update.
- Reject a malformed acknowledgement whose `resume_cursor` is blank rather
  than silently claiming completeness from a bounded bootstrap tail.
- Apply the same behavior in Go, Python, and TypeScript.

Regression tests will model server lower-bound filtering: a request from the
terminal cursor will not return earlier durable rows, while the immutable
invocation cursor will. The terminal projection must equal the projection built
from the durable snapshot, including `resolved_action`.

## Alternatives considered

- **Reconcile from the latest cursor:** this is the 0.0.46 behavior and cannot
  recover rows at or below the acknowledged message watermark.
- **Always bootstrap the session tail:** safe for ordinary turns but bounded by
  the bootstrap tail limit and therefore incomplete for an unusually large
  turn.
- **Stop pruning streaming rows:** avoids visible loss but leaves non-durable
  preview identities in a view documented as settled durable state.

## Tradeoffs

Terminal settlement may redeliver rows already observed during the turn. The
reducer is set-by-id and already requires idempotent upserts, so this adds a
bounded read and merge in exchange for a reliable completion boundary. A
deduplicated retry may also replay prior frames from the same stable cursor;
the reducer merges them the same way.
