import type {
  SessionTranscriptFrame,
  SessionTranscriptMessage,
  SessionTranscriptSnapshot,
  SessionTranscriptTurn,
  TurnAck,
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
 * opaque {@link SessionTranscript.cursor | resume cursor} in effect through
 * this frame. The server sends an SSE `id:` line only on state frames that
 * advance the delivered watermark; per the SSE spec the last-event-id
 * persists, so `id` here is that watermark and is `undefined` only before the
 * first `id:` line of the connection.
 */
export interface TranscriptStreamEvent {
  frame: SessionTranscriptFrame;
  id?: string;
}

/**
 * The live view of a session: message rows keyed by their immutable id, the
 * turns that produced them, the opaque resume cursor, and a `ready` flag. It
 * is built by folding session-transcript v2 stream frames (or snapshot pages)
 * into place — the session-scope analogue of Dive's `llm.ResponseAccumulator`.
 *
 * The whole merge is set-by-id: state frames carry absolute state, so last
 * write wins and nothing is an increment except {@link apply | message.delta}
 * text. Ignoring deltas entirely still converges.
 *
 * The streaming client methods drive one for you: {@link Client.invokeAgent}
 * returns a `TurnTranscript` and {@link Client.watchSessionTranscript} yields
 * this view after every change. Construct one directly only for the escape
 * hatches: polling {@link Client.getSessionTranscript} into
 * {@link applySnapshot}, or feeding {@link Client.streamSessionTranscript}
 * frames into {@link apply}.
 */
export class SessionTranscript {
  readonly #rows = new Map<string, SessionTranscriptMessage>();
  readonly #turns = new Map<string, SessionTranscriptTurn>();
  #cursor: string | null = null;
  #ready = false;

  /**
   * Opaque resume cursor in effect through everything folded in so far; never
   * parse it. Set it only to resume a fresh view from a persisted position —
   * applied frames and snapshots overwrite it.
   */
  get cursor(): string | null {
    return this.#cursor;
  }

  set cursor(value: string | null) {
    this.#cursor = value;
  }

  /** True once `stream.ready` has been seen on the current connection — safe to render. */
  get ready(): boolean {
    return this.#ready;
  }

  /** @internal Ready is per-connection; the watch loop re-arms it on reconnect. */
  _resetReady(): void {
    this.#ready = false;
  }

  /** The message row with the given id, if present. */
  message(id: string): SessionTranscriptMessage | undefined {
    return this.#rows.get(id);
  }

  /** The turn with the given id, if present. */
  turn(id: string): SessionTranscriptTurn | undefined {
    return this.#turns.get(id);
  }

  /** Turns ordered by `(created_at, id)`. */
  turns(): SessionTranscriptTurn[] {
    return [...this.#turns.values()].sort(
      (a, b) =>
        a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id),
    );
  }

  /**
   * Rows in render order: final rows by `sequence`, then streaming rows ordered
   * by `(turn.created_at, turn.id, turn_index)` — `turn_index` alone is unique
   * only within one turn, and turns can run concurrently.
   */
  messages(): SessionTranscriptMessage[] {
    const rows = [...this.#rows.values()];
    const final = rows
      .filter((r) => r.status === "final")
      .sort((a, b) => (a.sequence ?? 0) - (b.sequence ?? 0));
    const live = rows
      .filter((r) => r.status === "streaming")
      .sort((a, b) => {
        const ta = a.turn_id ? this.#turns.get(a.turn_id) : undefined;
        const tb = b.turn_id ? this.#turns.get(b.turn_id) : undefined;
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

  /**
   * Fold one stream frame into the view. Unknown `event_type`s are ignored so
   * the protocol can grow without breaking this client. This is the escape
   * hatch for frames obtained from {@link Client.streamSessionTranscript} or a
   * custom transport; the streaming client methods call it for you.
   */
  apply(ev: TranscriptStreamEvent): void {
    if (ev.id) this.#cursor = ev.id;
    const frame = ev.frame;
    // The union is discriminated by event_type; cast for field access since
    // several frames share a base shape (message/turn upserts extend rows).
    const f = frame as Record<string, unknown> & { event_type?: string };
    switch (f.event_type) {
      case "message.upsert":
        this.#rows.set(f.id as string, frame as unknown as SessionTranscriptMessage);
        break;
      case "message.block": {
        const row = this.#rows.get(f.message_id as string);
        const index = f.content_index as number;
        if (row && index >= 0) {
          // message.block opens (or completes) a block, so it may extend the
          // content list — unlike patch/delta. Pad with empty blocks rather
          // than index-assign so a gap never leaves a sparse array behind.
          while (row.content.length <= index) row.content.push({} as never);
          row.content[index] = f.block as never;
        }
        break;
      }
      case "message.block.patch": {
        const block = this.#rows.get(f.message_id as string)?.content[
          f.content_index as number
        ] as Record<string, unknown> | undefined;
        if (!block) break;
        if (f.status !== undefined) block.status = f.status;
        if (f.progress === null) delete block.progress; // null clears
        else if (f.progress !== undefined) block.progress = f.progress; // omitted preserves
        break;
      }
      case "message.delta": {
        const block = this.#rows.get(f.message_id as string)?.content[
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
        this.#turns.set(turn.id, turn);
        if (isTerminalTurnStatus(turn.status)) this.#pruneStreamingRows(turn.id);
        break;
      }
      case "stream.ready":
        // Authoritative — adopt unconditionally even if no frame advanced it.
        this.#cursor = f.resume_cursor as string;
        this.#ready = true;
        break;
      case "stream.end":
        // Control frame; the connection loop acts on it. No state change.
        break;
      default:
        break; // forward-compatible: ignore unknown frame types
    }
  }

  /**
   * Fold a transcript snapshot page (from {@link Client.getSessionTranscript}).
   * Each message folds in as a `message.upsert`, each turn as a `turn.upsert`.
   * On the final page (`has_more` false) the snapshot's streaming rows are the
   * complete live set, so any local streaming row absent from it is pruned.
   */
  applySnapshot(snap: SessionTranscriptSnapshot): void {
    for (const msg of snap.messages) this.#rows.set(msg.id, msg);
    for (const turn of snap.turns) {
      this.#turns.set(turn.id, turn);
      if (isTerminalTurnStatus(turn.status)) this.#pruneStreamingRows(turn.id);
    }
    if (!snap.has_more) {
      const live = new Set(
        snap.messages.filter((m) => m.status === "streaming").map((m) => m.id),
      );
      for (const [id, row] of this.#rows) {
        if (row.status === "streaming" && !live.has(id)) this.#rows.delete(id);
      }
    }
    this.#cursor = snap.resume_cursor;
  }

  /**
   * Fold a turn-start response into the view: the caller's message row, the
   * acked turn, and the resume cursor. {@link Client.invokeAgent} calls it for
   * you; it is public for callers wiring their own transport around a raw
   * invoke.
   */
  seed(ack: TurnAck): void {
    if (ack.user_message) this.#rows.set(ack.user_message.id, ack.user_message);
    const turn = ack.turn as unknown as SessionTranscriptTurn;
    if (turn?.id) this.#turns.set(turn.id, turn);
    if (ack.resume_cursor) this.#cursor = ack.resume_cursor;
  }

  #pruneStreamingRows(turnId: string): void {
    for (const [id, row] of this.#rows) {
      if (row.turn_id === turnId && row.status === "streaming")
        this.#rows.delete(id);
    }
  }
}
