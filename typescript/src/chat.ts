import type {
  Interaction,
  RespondToInteractionRequest,
  SessionTranscriptMessage,
  SessionTranscriptTurn,
} from "./api/index.js";
import type {
  Client,
  InvokeAgentOptions,
  TranscriptUpdate,
  WatchSessionTranscriptOptions,
} from "./client.js";
import type { SessionTranscript } from "./transcript.js";

export type SessionChatPhase = "queued" | "running" | "waiting";

export interface SessionChatCallbacks {
  onMessage?: (
    message: SessionTranscriptMessage,
    transcript: SessionTranscript,
  ) => void;
  onPhase?: (
    phase: SessionChatPhase | null,
    turn: SessionTranscriptTurn | null,
    transcript: SessionTranscript,
  ) => void;
  onInteraction?: (
    interaction: Interaction,
    transcript: SessionTranscript,
  ) => void;
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
 * High-level embedded-chat protocol: invoke an agent, follow the canonical v2
 * transcript through reconnects/rotations, observe human-input interactions,
 * and respond without implementing frame dispatch or polling.
 */
export class SessionChat {
  readonly #client: Client;
  readonly #callbacks: SessionChatCallbacks;
  readonly #messageSignatures = new Map<string, string>();
  readonly #interactionSignatures = new Map<string, string>();
  #phase: SessionChatPhase | null = null;

  constructor(client: Client, callbacks: SessionChatCallbacks = {}) {
    this.#client = client;
    this.#callbacks = callbacks;
  }

  /** Invoke and follow one turn until it settles. */
  async *send(
    input: InvokeAgentOptions,
    opts: { signal?: AbortSignal } = {},
  ): AsyncGenerator<SessionChatUpdate> {
    const turn = await this.#client.invokeAgent(input, opts);
    for await (const update of turn.updates()) {
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
      const previous = this.#interactionSignatures.get(interaction.id);
      this.#interactionSignatures.set(interaction.id, signature);
      this.#callbacks.onInteraction?.(interaction, transcript);
      if (previous !== undefined && interaction.status !== "pending") {
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
