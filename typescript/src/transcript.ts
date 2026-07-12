import type {
  SessionTranscriptFrame,
  SessionTranscriptMessage,
  SessionTranscriptSnapshot,
  SessionTranscriptTurn,
} from "./api/index.js";

/**
 * Terminal {@link SessionTranscriptTurn} statuses. A turn in one of these
 * states will not transition again. The set is closed (turn status is an open
 * string in the contract, but only these three are terminal).
 */
export function isTerminalTurnStatus(status: string): boolean {
  return (
    status === "completed" || status === "failed" || status === "cancelled"
  );
}

/**
 * A single decoded frame from a session transcript stream, paired with the
 * opaque {@link SessionTranscriptReducer.cursor | resume cursor} in effect
 * through this frame. The server sends an SSE `id:` line only on state frames
 * that advance the delivered watermark; per the SSE spec the last-event-id
 * persists, so `id` here is that watermark and is `undefined` only before the
 * first `id:` line of the connection.
 */
export interface TranscriptStreamEvent {
  frame: SessionTranscriptFrame;
  id?: string;
}

/**
 * The session-transcript v2 reducer: the session-scope analogue of Dive's
 * `llm.ResponseAccumulator`. It folds transcript stream frames (or snapshot
 * pages) into the authoritative view — a Map of message rows keyed by their
 * immutable id, a Map of turns, the opaque resume cursor, and a `ready` flag.
 *
 * The whole merge is `Map.set(row.id, row)`: state frames carry absolute
 * state, so last write wins and nothing is an increment except {@link
 * apply | message.delta} text. Ignoring deltas entirely still converges. See
 * `docs` in the mobius-cloud session-stream-v2 proposal for the protocol.
 *
 * This owns protocol logic only; the connection loop (open/retry, reconnect on
 * `stream.end rotate`, stop on `idle`) lives in {@link
 * Client.watchSessionTranscript}.
 */
export class SessionTranscriptReducer {
  /** Message rows keyed by their immutable id. */
  readonly rows = new Map<string, SessionTranscriptMessage>();
  /** Turns keyed by id. */
  readonly turns = new Map<string, SessionTranscriptTurn>();
  /** Opaque resume cursor; never parse it. From SSE `id:` lines / snapshots. */
  cursor: string | null = null;
  /** True once `stream.ready` has been seen — safe to render. */
  ready = false;

  /**
   * Apply one stream frame. `sseId` is the frame's SSE `id:` line when present;
   * it advances the cursor. Unknown `event_type`s are ignored so the protocol
   * can grow without breaking this client.
   */
  apply(frame: SessionTranscriptFrame, sseId?: string): void {
    if (sseId) this.cursor = sseId;
    // The union is discriminated by event_type; cast for field access since
    // several frames share a base shape (message/turn upserts extend rows).
    const f = frame as Record<string, unknown> & { event_type?: string };
    switch (f.event_type) {
      case "message.upsert":
        this.rows.set(f.id as string, frame as unknown as SessionTranscriptMessage);
        break;
      case "message.block": {
        const row = this.rows.get(f.message_id as string);
        if (row) row.content[f.content_index as number] = f.block as never;
        break;
      }
      case "message.block.patch": {
        const block = this.rows.get(f.message_id as string)?.content[
          f.content_index as number
        ] as Record<string, unknown> | undefined;
        if (!block) break;
        if (f.status !== undefined) block.status = f.status;
        if (f.progress === null) delete block.progress; // null clears
        else if (f.progress !== undefined) block.progress = f.progress; // omitted preserves
        break;
      }
      case "message.delta": {
        const block = this.rows.get(f.message_id as string)?.content[
          f.content_index as number
        ] as Record<string, unknown> | undefined;
        if (!block) break;
        if (f.text)
          block.text = ((block.text as string) ?? "") + (f.text as string);
        if (f.thinking)
          block.thinking =
            ((block.thinking as string) ?? "") + (f.thinking as string);
        break;
      }
      case "turn.upsert": {
        const turn = frame as unknown as SessionTranscriptTurn;
        this.turns.set(turn.id, turn);
        if (isTerminalTurnStatus(turn.status)) this.pruneStreamingRows(turn.id);
        break;
      }
      case "stream.ready":
        // Authoritative — adopt unconditionally even if no frame advanced it.
        this.cursor = f.resume_cursor as string;
        this.ready = true;
        break;
      case "stream.end":
        // Control frame; the connection loop acts on it. No state change.
        break;
      default:
        break; // forward-compatible: ignore unknown frame types
    }
  }

  /**
   * Apply a transcript snapshot page (from {@link Client.getSessionTranscript}).
   * Each message folds in as a `message.upsert`, each turn as a `turn.upsert`.
   * On the final page (`has_more` false) the snapshot's streaming rows are the
   * complete live set, so any local streaming row absent from it is pruned.
   */
  applySnapshot(snap: SessionTranscriptSnapshot): void {
    for (const msg of snap.messages) this.rows.set(msg.id, msg);
    for (const turn of snap.turns) {
      this.turns.set(turn.id, turn);
      if (isTerminalTurnStatus(turn.status)) this.pruneStreamingRows(turn.id);
    }
    if (!snap.has_more) {
      const live = new Set(
        snap.messages.filter((m) => m.status === "streaming").map((m) => m.id),
      );
      for (const [id, row] of this.rows) {
        if (row.status === "streaming" && !live.has(id)) this.rows.delete(id);
      }
    }
    this.cursor = snap.resume_cursor;
  }

  /**
   * Rows in render order: final rows by `sequence`, then streaming rows ordered
   * by `(turn.created_at, turn.id, turn_index)` — `turn_index` alone is unique
   * only within one turn, and turns can run concurrently.
   */
  messages(): SessionTranscriptMessage[] {
    const rows = [...this.rows.values()];
    const final = rows
      .filter((r) => r.status === "final")
      .sort((a, b) => (a.sequence ?? 0) - (b.sequence ?? 0));
    const live = rows
      .filter((r) => r.status === "streaming")
      .sort((a, b) => {
        const ta = a.turn_id ? this.turns.get(a.turn_id) : undefined;
        const tb = b.turn_id ? this.turns.get(b.turn_id) : undefined;
        return (
          (ta?.created_at ?? "").localeCompare(tb?.created_at ?? "") ||
          (a.turn_id ?? "").localeCompare(b.turn_id ?? "") ||
          (a.turn_index ?? 0) - (b.turn_index ?? 0)
        );
      });
    return [...final, ...live];
  }

  /** Rows belonging to one turn, in render order. */
  messagesForTurn(turnId: string): SessionTranscriptMessage[] {
    return this.messages().filter((r) => r.turn_id === turnId);
  }

  private pruneStreamingRows(turnId: string): void {
    for (const [id, row] of this.rows) {
      if (row.turn_id === turnId && row.status === "streaming")
        this.rows.delete(id);
    }
  }
}
