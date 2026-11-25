import type {
  Interaction,
  RespondToInteractionRequest,
  SessionTranscriptMessage,
  SessionTranscriptTurn,
} from "./api/index.js";
import type {
  Client,
  SendSessionInputOptions,
  TranscriptUpdate,
  WatchSessionTranscriptOptions,
} from "./client.js";
import type { SessionTranscript } from "./transcript.js";

export type SessionChatPhase = "queued" | "running" | "waiting";

export interface SessionChatCallbacks {
  /** Fires whenever a message first appears or its folded content changes. */
  onMessage?: (
    message: SessionTranscriptMessage,
    transcript: SessionTranscript,
  ) => void;
  /** Fires when the current active chat phase changes. */
  onPhase?: (
    phase: SessionChatPhase | null,
    turn: SessionTranscriptTurn | null,
    transcript: SessionTranscript,
  ) => void;
  /** Fires for every new or changed interaction, including terminal upserts. */
  onInteraction?: (
    interaction: Interaction,
    transcript: SessionTranscript,
  ) => void;
  /**
   * Fires only when a previously observed pending interaction transitions to
   * a terminal status. A terminal interaction first seen after reconnect is
   * reported through onInteraction only.
   */
  onInteractionResolved?: (
    interaction: Interaction,
    transcript: SessionTranscript,
  ) => void;
}

export interface SessionChatUpdate extends TranscriptUpdate {
  phase: SessionChatPhase | null;
  pendingInteractions: Interaction[];
}

/**
 * High-level embedded-chat protocol: send input to a session, follow the canonical v2
 * transcript through reconnects/rotations, observe human-input interactions,
 * and respond without implementing frame dispatch or polling.
 */
export class SessionChat {
  readonly #client: Client;
  readonly #callbacks: SessionChatCallbacks;
  readonly #messageSignatures = new Map<string, string>();
  readonly #interactionSignatures = new Map<string, string>();
  readonly #interactionStatuses = new Map<string, Interaction["status"]>();
  #phase: SessionChatPhase | null = null;

  constructor(client: Client, callbacks: SessionChatCallbacks = {}) {
    this.#client = client;
    this.#callbacks = callbacks;
  }

  /**
   * Send input to a session and follow the turn it started until it
   * settles. When the input did not start a turn (it became direction for a
   * running turn), this yields nothing; use {@link watch} to follow the
   * session instead.
   */
  async *send(
    sessionId: string,
    options: SendSessionInputOptions,
    opts: { signal?: AbortSignal } = {},
  ): AsyncGenerator<SessionChatUpdate> {
    const result = await this.#client.sendSessionInput(sessionId, options, opts);
    if (!result.turn) return;
    for await (const update of result.turn.updates()) {
      yield this.#emit(update);
    }
  }

  /** Follow an existing session; reconnect, rotate, idle, and cursor handling are managed by the client. */
  async *watch(
    sessionId: string,
    opts: WatchSessionTranscriptOptions = {},
  ): AsyncGenerator<SessionChatUpdate> {
    for await (const update of this.#client.watchSessionTranscriptUpdates(
      sessionId,
      opts,
    )) {
      yield this.#emit(update);
    }
  }

  /** Respond to a pushed interaction. The stream remains authoritative. */
  async respond(
    interactionId: string,
    input: RespondToInteractionRequest,
  ): Promise<Interaction> {
    return this.#client.respondToInteraction(interactionId, input);
  }

  #emit(update: TranscriptUpdate): SessionChatUpdate {
    const transcript = update.transcript;
    for (const message of transcript.renderableMessages()) {
      const signature = stableSignature(message);
      if (this.#messageSignatures.get(message.id) === signature) continue;
      this.#messageSignatures.set(message.id, signature);
      this.#callbacks.onMessage?.(message, transcript);
    }

    for (const interaction of transcript.interactions()) {
      const signature = stableSignature(interaction);
      if (this.#interactionSignatures.get(interaction.id) === signature)
        continue;
      const previousStatus = this.#interactionStatuses.get(interaction.id);
      this.#interactionSignatures.set(interaction.id, signature);
      this.#interactionStatuses.set(interaction.id, interaction.status);
      this.#callbacks.onInteraction?.(interaction, transcript);
      if (previousStatus === "pending" && interaction.status !== "pending") {
        this.#callbacks.onInteractionResolved?.(interaction, transcript);
      }
    }

    const turn = latestActiveTurn(transcript.turns());
    const phase = isChatPhase(turn?.status) ? turn.status : null;
    if (phase !== this.#phase) {
      this.#phase = phase;
      this.#callbacks.onPhase?.(phase, turn ?? null, transcript);
    }
    return {
      ...update,
      phase,
      pendingInteractions: transcript.pendingInteractions(),
    };
  }
}

function latestActiveTurn(
  turns: SessionTranscriptTurn[],
): SessionTranscriptTurn | undefined {
  return [...turns].reverse().find(({ status }) => isChatPhase(status));
}

function isChatPhase(value: string | undefined): value is SessionChatPhase {
  return value === "queued" || value === "running" || value === "waiting";
}

function stableSignature(value: unknown): string {
  return JSON.stringify(value);
}
