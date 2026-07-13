import type {
  SessionResolvedAction,
  SessionToolResultBlock,
  SessionToolUseBlock,
  SessionTranscriptFrame,
  SessionTranscriptMessage,
  SessionTranscriptSnapshot,
  SessionTranscriptTurn,
  TurnAck,
} from "./api/index.js";

export interface NormalizedToolUse {
  wireName: string;
  wireInput: Record<string, unknown>;
  resolvedAction?: SessionResolvedAction;
  toolkitName?: string;
  command?: string;
  raw: SessionToolUseBlock;
}

/**
 * Normalize a tool-use block without discarding the model-facing wire shape.
 * Persisted `resolved_action` is authoritative; the legacy `{command,args}`
 * shape is recognized only as a compatibility fallback.
 */
export function normalizeToolUse(block: SessionToolUseBlock): NormalizedToolUse {
  const raw = block as SessionToolUseBlock & Record<string, unknown>;
  const wireName = typeof raw.name === "string" ? raw.name : "";
  const wireInput = isRecord(raw.input) ? raw.input : {};
  const persisted = isResolvedAction(raw.resolved_action)
    ? raw.resolved_action
    : undefined;
  const command =
    typeof wireInput.command === "string" && wireInput.command.trim() !== ""
      ? wireInput.command
      : undefined;
  const args = isRecord(wireInput.args) ? wireInput.args : undefined;
  const resolvedAction =
    persisted ??
    (command && args
      ? { name: `${wireName}.${command}`, input: args }
      : undefined);
  let toolkitName: string | undefined;
  let normalizedCommand = command;
  if (command) {
    toolkitName = wireName || undefined;
  } else if (resolvedAction) {
    const dot = resolvedAction.name.indexOf(".");
    if (dot > 0) {
      toolkitName = resolvedAction.name.slice(0, dot);
      normalizedCommand = resolvedAction.name.slice(dot + 1);
    }
  }
  return removeUndefined({
    wireName,
    wireInput,
    resolvedAction,
    toolkitName,
    command: normalizedCommand,
    raw: block,
  });
}

/** Concatenate the text blocks in one transcript message. */
export function textOf(message: SessionTranscriptMessage): string {
  return normalizeContent(message.content)
    .map((block) =>
      isRecord(block) && typeof block.text === "string" ? block.text : "",
    )
    .filter(Boolean)
    .join("");
}

/** Return readable text from a tool-result block's string-or-block content. */
export function toolResultText(block: SessionToolResultBlock): string {
  const content = (block as SessionToolResultBlock & { content?: unknown }).content;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((item) =>
      isRecord(item) && item.type === "text" && typeof item.text === "string"
        ? item.text
        : "",
    )
    .join("");
}

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
   * Policy-light rendering projection: one row per logical response segment,
   * final over streaming, empty-preview suppression, and tool-call-id dedup.
   * The lossless {@link messages} view remains unchanged.
   */
  renderableMessages(): SessionTranscriptMessage[] {
    const selected = new Map<string, SessionTranscriptMessage>();
    for (const row of this.messages()) {
      const normalized = normalizeMessage(row);
      const key = logicalMessageKey(normalized);
      const current = selected.get(key);
      if (!current || preferMessage(normalized, current)) selected.set(key, normalized);
    }

    const rows = [...selected.values()];
    const newestEmpty = new Map<string, SessionTranscriptMessage>();
    for (const row of rows) {
      if (
        row.status !== "streaming" ||
        row.role !== "assistant" ||
        row.content.length !== 0 ||
        !row.turn_id ||
        !this.#turnIsActive(row.turn_id)
      ) {
        continue;
      }
      const current = newestEmpty.get(row.turn_id);
      if (!current || (row.turn_index ?? 0) >= (current.turn_index ?? 0)) {
        newestEmpty.set(row.turn_id, row);
      }
    }
    const visible = rows.filter(
      (row) =>
        row.status !== "streaming" ||
        row.role !== "assistant" ||
        row.content.length !== 0 ||
        !row.turn_id ||
        !this.#turnIsActive(row.turn_id) ||
        newestEmpty.get(row.turn_id)?.id === row.id,
    );
    return dedupeToolBlocks(visible);
  }

  /** Renderable rows belonging to one turn. */
  renderableMessagesForTurn(turnId: string): SessionTranscriptMessage[] {
    return this.renderableMessages().filter((row) => row.turn_id === turnId);
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
        this.#rows.set(
          f.id as string,
          normalizeMessage(frame as unknown as SessionTranscriptMessage),
        );
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
        if (f.resolved_action !== undefined)
          block.resolved_action = f.resolved_action;
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
    for (const msg of snap.messages) this.#rows.set(msg.id, normalizeMessage(msg));
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
    if (ack.user_message)
      this.#rows.set(ack.user_message.id, normalizeMessage(ack.user_message));
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

  #turnIsActive(turnId: string): boolean {
    const turn = this.#turns.get(turnId);
    return turn !== undefined && !isTerminalTurnStatus(turn.status);
  }
}

function normalizeMessage(
  message: SessionTranscriptMessage,
): SessionTranscriptMessage {
  return { ...message, content: normalizeContent(message.content) };
}

function normalizeContent(content: unknown): SessionTranscriptMessage["content"] {
  return (Array.isArray(content) ? content : []) as SessionTranscriptMessage["content"];
}

function logicalMessageKey(message: SessionTranscriptMessage): string {
  const metadata = isRecord(message.metadata) ? message.metadata : {};
  const legacy =
    typeof metadata.response_message_index === "number"
      ? metadata.response_message_index
      : undefined;
  const index = message.turn_index ?? legacy;
  if (!message.turn_id || index == null) return `id:${message.id}`;
  return `logical:${message.turn_id}:${message.role}:${index}`;
}

function preferMessage(
  candidate: SessionTranscriptMessage,
  current: SessionTranscriptMessage,
): boolean {
  if (candidate.status === "final" && current.status !== "final") return true;
  if (candidate.status !== "final" && current.status === "final") return false;
  return messageCompleteness(candidate) >= messageCompleteness(current);
}

function messageCompleteness(message: SessionTranscriptMessage): number {
  try {
    return JSON.stringify(message.content).length + (message.status === "final" ? 1_000_000 : 0);
  } catch {
    return message.content.length;
  }
}

function dedupeToolBlocks(
  messages: SessionTranscriptMessage[],
): SessionTranscriptMessage[] {
  const best = new Map<string, { row: number; block: number; score: number }>();
  messages.forEach((message, row) => {
    message.content.forEach((block, index) => {
      const key = toolBlockKey(block);
      if (!key) return;
      const score = blockCompleteness(block);
      const current = best.get(key);
      if (!current || score >= current.score) best.set(key, { row, block: index, score });
    });
  });
  return messages.map((message, row) => ({
    ...message,
    content: message.content.filter((block, index) => {
      const key = toolBlockKey(block);
      const chosen = key ? best.get(key) : undefined;
      return !chosen || (chosen.row === row && chosen.block === index);
    }) as SessionTranscriptMessage["content"],
  }));
}

function toolBlockKey(block: unknown): string | undefined {
  if (!isRecord(block)) return undefined;
  if (block.type === "tool_use" && typeof block.id === "string")
    return `use:${block.id}`;
  if (block.type === "tool_result" && typeof block.tool_use_id === "string")
    return `result:${block.tool_use_id}`;
  return undefined;
}

function blockCompleteness(block: unknown): number {
  try {
    return JSON.stringify(block).length;
  } catch {
    return 0;
  }
}

function isResolvedAction(value: unknown): value is SessionResolvedAction {
  return (
    isRecord(value) &&
    typeof value.name === "string" &&
    isRecord(value.input)
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function removeUndefined<T extends object>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(([, item]) => item !== undefined),
  ) as T;
}
