# Session transcript API redesign — `SessionTranscript` and `TurnTranscript`

Scope: the session-transcript helper surface in all three SDKs —
`mobius/transcript.go`, `typescript/src/transcript.ts` +
`typescript/src/client.ts`, `python/deepnoodle/mobius/transcript.py` +
`client.py` — and the Session Transcripts section of `docs/sdk-helpers.md`.
Line references in the Problem section are to the tree at commit `0596759`
(pre-redesign).

## Implementation status

Implemented in all three SDKs as designed, with two additions that surfaced
during implementation: `TurnTranscript` also exposes `Deduped` and
`AfterSequence` (the two remaining `TurnAck` fields — dedupe visibility and
the v1 stream cursor for raw-API interop), and the view gained a
`Message(id)` accessor alongside `Turn(id)`. No deprecated
`SessionTranscriptReducer` alias was kept — clean break at v0.

No wire changes. The session-stream v2 protocol, the snapshot endpoint, and
every fixture in `internal/testdata/contract/` are untouched. This is a
repackaging of the client-side helpers only.

## Problem

The current design makes the caller assemble three things the SDK should
compose itself. The canonical Go example from `docs/sdk-helpers.md`:

```go
ack, events, err := client.InvokeAgentTranscript(ctx, opts)   // ack + raw frame channel
r := mobius.NewSessionTranscriptReducer()                     // caller builds the state holder
if ack.UserMessage != nil { r.Rows[ack.UserMessage.Id] = ack.UserMessage }  // caller seeds it by map-poking
for ev := range events {
    r.Apply(ev.Frame, ev.ID)                                  // caller threads SSE plumbing
    render(r.MessagesForTurn(ack.Turn.Id))                    // caller re-scopes to the turn
}
```

Specific defects, roughly by severity:

1. **The composition is boilerplate every caller writes identically.** The
   stream is useless without the reducer and vice versa, yet Go and Python
   hand out the halves. TypeScript already composes them — its
   `invokeAgentTranscript` drives the reducer internally and yields it
   (`typescript/src/client.ts:619`) — so `docs/sdk-helpers.md` has to spend a
   paragraph explaining that Go/Python yield raw frames while TS yields the
   reducer. Docs documenting an inconsistency is the design telling on
   itself.
2. **Manual seeding is a demonstrated trap.** The TS internal path seeds
   three things: the user message, the turn row, and the cursor
   (`client.ts:626-628`). The Go doc example seeds only the user message —
   the SDK's own reference example is incomplete (turn ordering degrades
   until the first `turn.upsert` arrives). If the reference example can't
   seed correctly, users won't.
3. **The internal representation is the public API.** `Rows` and `Turns` are
   exported mutable maps (`mobius/transcript.go:60-69`), and map-poking is
   the official seeding mechanism. That freezes the implementation and
   invites corruption (a row without its turn, a forgotten cursor).
4. **`Apply(frame, sseID)` leaks transport details.** The SSE `id:` line is
   connection plumbing; callers shouldn't know it exists, let alone be
   responsible for threading it correctly.
5. **"Reducer" names the mechanism, not the thing.** Redux/Elm jargon for
   "folds events into state". What the caller holds is a transcript.
6. **Go's error-as-last-event is a footgun.** `TranscriptStreamEvent.Err` is
   set only on the final event before the channel closes
   (`mobius/transcript.go:37-40`). Miss the check inside the loop and a 401
   looks like a clean end-of-turn.
7. **TS has the opposite gap.** Its `invokeAgentTranscript` never exposes the
   ack, so the caller can't learn the turn id — `messagesForTurn()` is
   uncallable and a turn-scoped stream renders the whole session.
8. **"ack" is wire-protocol jargon on the public surface.** Callers are
   handed a `TurnAck` and expected to know which pieces of it matter and
   where they go.

What is *right* and must be kept: the fold semantics (set-by-id merge,
streaming-row pruning, render ordering), the layering that separates protocol
state from the reconnect loop, and low-level frame access as an escape hatch
(custom transports, the polling path, contract testing). The problem is
packaging, not semantics.

## Design

Three decisions:

1. **One noun.** `SessionTranscriptReducer` becomes **`SessionTranscript`** —
   the live view of a session: messages, turns, cursor, readiness. Maps go
   private behind accessors. `Apply` takes the stream event (one argument);
   the cursor is handled inside.
2. **One method, one object for invocation.** `InvokeAgentTranscript` is
   deleted as a separate method. `InvokeAgent` always returns a
   **`TurnTranscript`** handle — the "Transcript" suffix lives on the *type*,
   not the method. The handle is cheap because the stream is **lazy**: the
   invoke is still a single HTTP POST, and the SSE connection opens only on
   first iteration. Fire-and-forget callers just don't iterate.
3. **No ack on the surface.** Every `TurnAck` field already has a better
   home:

   | `TurnAck` field | Where it lives now |
   |---|---|
   | `turn` | seeded into the view — `ID()` / `Status()` on the handle |
   | `session` | `SessionID()` on the handle |
   | `user_message` | seeded as a row — appears in `Messages()` |
   | `resume_cursor` | internal — used to open the stream |

   `TurnAck` remains a generated wire type in the `api` package but the
   helper surface never mentions it.

### Go surface

```go
// The view (renamed reducer). Maps unexported.
type SessionTranscript struct { /* unexported */ }

func NewSessionTranscript() *SessionTranscript
func (t *SessionTranscript) Messages() []*api.SessionTranscriptMessage
func (t *SessionTranscript) MessagesForTurn(turnID string) []*api.SessionTranscriptMessage
func (t *SessionTranscript) Turn(id string) (*api.SessionTranscriptTurn, bool)
func (t *SessionTranscript) Turns() []*api.SessionTranscriptTurn // by created_at
func (t *SessionTranscript) Cursor() string
func (t *SessionTranscript) Ready() bool
// Escape-hatch layer, still public — custom transports and polling:
func (t *SessionTranscript) Apply(ev TranscriptStreamEvent)
func (t *SessionTranscript) ApplySnapshot(snap *api.SessionTranscriptSnapshot)
func (t *SessionTranscript) Seed(ack *api.TurnAck) // typed seeding, no map-poking

// Turn-scope handle. InvokeAgent always returns one.
func (c *Client) InvokeAgent(ctx context.Context, opts InvokeAgentOptions) (*TurnTranscript, error)

type TurnTranscript struct { /* unexported */ }

func (t *TurnTranscript) ID() string        // turn id
func (t *TurnTranscript) SessionID() string
func (t *TurnTranscript) Status() string    // live — updates as turn.upsert frames fold in

func (t *TurnTranscript) Next() bool        // lazy: first call opens the stream
func (t *TurnTranscript) Err() error

func (t *TurnTranscript) Messages() []*api.SessionTranscriptMessage // this turn's rows
func (t *TurnTranscript) Transcript() *SessionTranscript            // full session view

// Session-scope handle: owns the reconnect loop, promotes the view methods.
func (c *Client) WatchSessionTranscript(ctx context.Context, sessionID string, opts *WatchSessionTranscriptOptions) *TranscriptWatcher

type TranscriptWatcher struct { /* unexported */ }

func (w *TranscriptWatcher) Next() bool // blocks through reconnects; false on idle/ctx/error
func (w *TranscriptWatcher) Err() error
func (w *TranscriptWatcher) Transcript() *SessionTranscript
// view methods promoted: w.Messages(), w.Cursor(), w.Ready(), ...
```

`WatchSessionTranscriptOptions` keeps `Cursor` and `ReconnectDelay` and gains
`Transcript *SessionTranscript` to continue an existing view (the analogue of
today's TS `opts.reducer`).

Usage:

```go
turn, err := client.InvokeAgent(ctx, mobius.InvokeAgentOptions{
	AgentName: "support",
	Content:   []map[string]any{{"type": "text", "text": "check the deploy"}},
})
if err != nil { return err }
for turn.Next() {
	render(turn.Messages())
}
if err := turn.Err(); err != nil { return err }
// turn.Status() == "completed"
```

```go
// Fire-and-forget: same method, never iterate, no stream is opened.
turn, err := client.InvokeAgent(ctx, opts)
log.Printf("started turn %s in session %s", turn.ID(), turn.SessionID())
```

The scanner idiom (`Next()`/`Err()`, as in `bufio.Scanner` / `sql.Rows`) is
pull-based: the SSE read and the fold both happen in the caller's goroutine.
That removes the internal goroutine + channel from the watch loop, makes the
"not safe for concurrent use" hazard structural rather than documented (no
producer goroutine writes state the consumer reads), and lets
`TranscriptStreamEvent.Err` be deleted outright — errors surface through
`Err()`. Cancellation comes from the ctx captured at construction; `Next()`
returns false when it fires.

### Python surface

The handle is iterable and yields itself after each applied frame; errors
raise (the existing httpx behaviour).

```python
turn = client.invoke_agent(mobius.InvokeAgentOptions(
    agent_name="support",
    content=[{"type": "text", "text": "check the deploy"}],
))
turn.id, turn.session_id          # available immediately
for t in turn:                    # lazy: iteration opens the stream
    render(t.messages())          # this turn's rows
turn.status                       # "completed"
```

```python
watch = client.watch_session_transcript(session_id)
for t in watch:
    render(t.messages())          # full session view
save_cursor(watch.transcript.cursor)
```

`SessionTranscript` (renamed class) keeps `apply(event)` /
`apply_snapshot(snap)` / `seed(ack)` public; `rows` / `turns` go private
behind `messages()` / `turn(id)` / `turns()` accessors. `TurnTranscript`
exposes `id`, `session_id`, `status` as properties, `messages()` scoped to
the turn, and `transcript` for the full view.

### TypeScript surface

`invokeAgent` returns a promise of the handle (the POST resolves it); the
handle is `AsyncIterable` and yields after each applied frame.

```ts
const turn = await client.invokeAgent({
  agentName: "support",
  content: [{ type: "text", text: "check the deploy" }],
});
turn.id; turn.sessionId;             // sync after the await
for await (const t of turn) {        // lazy: iteration opens the stream
  render(t.messages());              // this turn's rows
}
turn.status; // "completed"
```

`watchSessionTranscript` keeps its generator shape but yields
`SessionTranscript` (renamed), and `opts.reducer` becomes `opts.transcript`.
The `rows` / `turns` Maps go private behind `messages()` / `turn(id)` /
`turns()`; `apply(event)` / `applySnapshot(snap)` / `seed(ack)` stay public.

### Semantics

- **Yield granularity** is per-frame, matching today's TS behaviour. Callers
  who render expensively can throttle; a coalescing option can come later if
  it proves needed.
- **Seeding is internal.** `InvokeAgent` seeds the view with the ack's user
  message, turn row, and cursor before returning the handle — the three
  things today's Go doc example gets one-third right.
- **Deduped / already-terminal turns.** Today TS special-cases an invoke that
  acks an already-completed turn (it returns before streaming, leaving no
  assistant rows). New behaviour: if the acked turn is terminal, first
  iteration hydrates once from `GetSessionTranscript` instead of opening a
  stream, so `Messages()` is complete either way and the caller can't tell
  the difference.
- **Termination.** `TurnTranscript` iteration ends at this turn's terminal
  `turn.upsert` (or idle / ctx / error). `TranscriptWatcher` iteration ends
  on `stream.end idle` (or ctx / error) and reconnects through `rotate` and
  drops, exactly as today's watch loop does. Retry classification is
  unchanged: 429/503 reconnect, everything else is terminal.
- **The full session stream is still consumed internally** during a turn
  stream, so the resume cursor stays valid when other turns interleave —
  same as today; only the packaging changes.

### API tiers

| Tier | Go | Python | TypeScript |
|---|---|---|---|
| Invoke + render a turn | `InvokeAgent` → `*TurnTranscript` | `invoke_agent` → `TurnTranscript` | `invokeAgent` → `Promise<TurnTranscript>` |
| Follow a session | `WatchSessionTranscript` → `*TranscriptWatcher` | `watch_session_transcript` → iterable | `watchSessionTranscript` → async generator of `SessionTranscript` |
| Poll | `GetSessionTranscript` + `ApplySnapshot` | same | same |
| Raw frames (escape hatch) | `StreamSessionTranscript` → channel | `stream_session_transcript` → iterator | `streamSessionTranscript` → async generator |

`StreamSessionTranscript` is unchanged: it never set
`TranscriptStreamEvent.Err` (only watch/invoke did), so deleting that field
keeps its behaviour at status quo.

## Naming decision

The method stays `InvokeAgent` / `invoke_agent` / `invokeAgent`; the
"Transcript" suffix lives on the returned `TurnTranscript` type. Since it is
now the only invoke variant, suffixing the method would make every
fire-and-forget caller name a stream they never open. If call-site clarity is
preferred anyway, renaming the method is a one-word change.

## Migration

Breaking changes (fine at v0.0.x):

- `InvokeAgent` return type: `*api.TurnAck` → `*TurnTranscript`. Field access
  migrates mechanically (`ack.Turn.Id` → `turn.ID()`, `ack.Session.Id` →
  `turn.SessionID()`, `ack.UserMessage` → already in `turn.Messages()`).
- `InvokeAgentTranscript` (all three SDKs): deleted; callers move to
  `InvokeAgent`.
- `WatchSessionTranscript` (Go/Python) return type: raw frame channel /
  iterator → handle. TS signature keeps its shape.
- `SessionTranscriptReducer` → `SessionTranscript`; `Rows` / `Turns` go
  private; `Apply` drops the `sseID` parameter. A deprecated alias (Go type
  alias, TS re-export, Python subclass) can ride along for one release if a
  softer landing is wanted.
- `TranscriptStreamEvent.Err` (Go): deleted.

Not breaking:

- Wire protocol, `openapi.yaml`, generated types, and all contract fixtures.
- `GetSessionTranscript` / `StreamSessionTranscript` signatures (modulo the
  `Err` field note above).

Test and doc impact: `mobius/transcript_test.go`,
`typescript/test/transcript.test.ts`, and `python/tests/test_transcript.py`
update mechanically to the new names/accessors; fold semantics tests are
unchanged. The Session Transcripts section of `docs/sdk-helpers.md` collapses
to one snippet shape told three times — the paragraph explaining how Go and
Python differ from TS is deleted. That deletion is the litmus test for the
whole redesign.

## Open questions

- Does anything need arbitrary row injection beyond `Seed(ack)`? If a real
  case appears, add `Put(msg)` rather than re-exposing the maps.
- Should `TurnTranscript` grow a `Wait()` (drain to terminal without
  rendering, return final status)? Deferred until someone asks; `for
  turn.Next() {}` already expresses it.
- Per-frame yields may be chatty for TUI renderers; revisit a `coalesce`
  option only with evidence.
