import type {
  AgentTurn,
  AgentTurnListResponse,
  AgentRef,
  CancelLoopRunRequest,
  ChannelContext,
  CreateLoopRequest,
  InlineAgentConfig,
  InvokeAgentRequest,
  InvokeInput,
  InvokeSessionSpec,
  Loop,
  LoopListResponse,
  LoopRun,
  LoopRunEvent,
  LoopRunListResponse,
  LoopRunSource,
  LoopRunStatus,
  LoopStatus,
  Session,
  SessionListResponse,
  SessionMessageListResponse,
  SessionNudge,
  SessionNudgeAck,
  SessionNudgeListResponse,
  SessionNudgeStatus,
  SessionStreamFrame,
  SessionTranscriptFrame,
  SessionTranscriptMessage,
  SessionTranscriptSnapshot,
  SignalLoopRunRequest,
  StartLoopRunRequest,
  StreamEndFrame,
  TagMap,
  TurnAck,
  UpdateLoopRequest,
} from "./api/index.js";
import {
  DEFAULT_MAX_RETRIES,
  RateLimitError,
  wrapFetchWithRetry,
} from "./retry.js";
import {
  SessionTranscript,
  isTerminalTurnStatus,
  type TranscriptStreamEvent,
} from "./transcript.js";

export { RateLimitError } from "./retry.js";

export interface ClientOptions {
  apiKey: string;
  baseURL?: string;
  /** Project handle used for all project-scoped operations. */
  project?: string;
  /** Compatibility alias for older callers. */
  namespace?: string;
  /** Fetch timeout in milliseconds. Defaults to 60_000. */
  timeoutMs?: number;
  /** Number of retries for 429/503 responses. */
  retry?: number;
  /** Optional structured transport logger. Headers, bodies, and message content are never included. */
  logger?: (event: ClientLogEvent) => void;
}

export interface ClientLogEvent {
  type: "request" | "retry" | "stream";
  event: string;
  method?: string;
  path?: string;
  status?: number;
  durationMs?: number;
  attempt?: number;
  waitSeconds?: number;
  frameType?: string;
  error?: string;
}

export const DEFAULT_BASE_URL = "https://api.mobiusops.ai";
export const DEFAULT_PROJECT = "default";
export const DEFAULT_NAMESPACE = DEFAULT_PROJECT;

export class AuthRevokedError extends Error {
  constructor() {
    super("mobius: credential revoked");
    this.name = "AuthRevokedError";
  }
}

export class MobiusAPIError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: Record<string, unknown>;
  readonly requestId?: string;
  readonly retryAfter?: number;

  constructor(init: {
    status: number;
    code: string;
    message: string;
    details?: Record<string, unknown>;
    requestId?: string;
    retryAfter?: number;
  }) {
    super(init.message);
    this.name = "MobiusAPIError";
    this.status = init.status;
    this.code = init.code;
    this.details = init.details;
    this.requestId = init.requestId;
    this.retryAfter = init.retryAfter;
  }
}

/**
 * Thrown when a streaming request (e.g. {@link Client.streamSessionTranscript})
 * returns a non-OK HTTP status. `status` carries the response code so callers
 * of {@link Client.watchSessionTranscript} can branch on it — the watch loop
 * reconnects only on transient statuses (429/503) and surfaces every other
 * status by throwing this error.
 */
export class StreamHTTPError extends MobiusAPIError {
  constructor(
    status: number,
    message: string,
    init: Partial<Pick<MobiusAPIError, "code" | "details" | "requestId" | "retryAfter">> = {},
  ) {
    super({
      status,
      message,
      code: init.code ?? "http_error",
      details: init.details,
      requestId: init.requestId,
      retryAfter: init.retryAfter,
    });
    this.name = "StreamHTTPError";
  }
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

export class LeaseLostError extends Error {
  constructor(public readonly jobId: string) {
    super(`lease lost for job ${jobId}`);
    this.name = "LeaseLostError";
  }
}

export class PayloadTooLargeError extends Error {
  constructor(public readonly jobId: string) {
    super(`custom event payload too large for job ${jobId}`);
    this.name = "PayloadTooLargeError";
  }
}

export class RateLimitedError extends RateLimitError {
  readonly jobId: string;

  constructor(jobId: string, retryAfter?: number) {
    super({
      retryAfter: retryAfter ?? 0,
      message:
        retryAfter != null
          ? `custom event rate limited for job ${jobId} (retry after ${retryAfter}s)`
          : `custom event rate limited for job ${jobId}`,
    });
    this.name = "RateLimitedError";
    this.jobId = jobId;
  }
}

export class WorkerInstanceConflictError extends Error {
  constructor(
    public readonly workerInstanceId: string | undefined,
    public readonly projectHandle: string,
    message?: string,
  ) {
    super(
      message ??
        (workerInstanceId
          ? `mobius: worker_instance_id ${JSON.stringify(workerInstanceId)} is already registered in project ${JSON.stringify(projectHandle)} by another live process`
          : "mobius: worker instance conflict"),
    );
    this.name = "WorkerInstanceConflictError";
  }
}

const HANDLE_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;

function extractHandleFromApiKey(apiKey: string): string | null {
  if (!apiKey.startsWith("mbx_") && !apiKey.startsWith("mbc_")) return null;
  const dot = apiKey.lastIndexOf(".");
  if (dot < 0 || dot === apiKey.length - 1) return null;
  const handle = apiKey.slice(dot + 1);
  if (!HANDLE_RE.test(handle)) {
    throw new ConfigError(
      `invalid project handle suffix in API key: ${JSON.stringify(handle)}`,
    );
  }
  return handle;
}

export interface LoopOptions {
  name: string;
  description?: string;
  agent_id?: string;
  default_config?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  tags?: TagMap;
  /**
   * Authoring definition for the loop. Recognised keys are schema_version,
   * steps, event, config, triggers, defaults, limits, output, repositories,
   * cleanup, …. When it carries steps the loop is runnable immediately.
   */
  spec?: Record<string, unknown>;
}

export interface UpdateLoopOptions {
  name?: string;
  description?: string;
  agent_id?: string;
  default_config?: Record<string, unknown>;
  settings?: Record<string, unknown>;
  status?: LoopStatus;
  tags?: TagMap;
  /** Replacement authoring definition. See {@link LoopOptions.spec}. */
  spec?: Record<string, unknown>;
}

export interface ListLoopsOptions {
  status?: LoopStatus;
  cursor?: string;
  limit?: number;
}

export interface StartRunOptions {
  /** Exact event object that starts the run, reachable in templates at `event.*`. */
  event?: Record<string, unknown>;
  /** Optional static or caller-provided configuration, reachable at `config.*`. */
  config?: Record<string, unknown>;
  /** Optional caller-supplied event metadata; Mobius adds its own provenance. */
  meta?: Record<string, unknown>;
  source?: LoopRunSource;
  external_id?: string;
}

export interface ListRunsOptions {
  status?: LoopRunStatus;
  loop_id?: string;
  cursor?: string;
  limit?: number;
}

export interface WatchRunOptions {
  since?: number;
  signal?: AbortSignal;
}

export interface WaitRunOptions extends WatchRunOptions {
  reconnectDelayMs?: number;
}

export type RunEvent = LoopRunEvent;

export interface InvokeAgentOptions {
  /** Agent identifier. Mutually exclusive with agentName. */
  agentId?: string;
  /** Project-unique agent name. Mutually exclusive with agentId. */
  agentName?: string;
  /** Ordered content blocks (text, images, …) for the caller's input message. Required. */
  content: Record<string, unknown>[];
  /**
   * Dedup key scoped to the resolved session. A repeat call with the same
   * key resolves the same session and resumes the existing turn rather
   * than starting a second one — derive it from the provider event id for
   * Slack/Telegram webhook retries.
   */
  idempotencyKey?: string;
  /** Free-form caller metadata attached to the input message. */
  inputMetadata?: Record<string, unknown>;
  /**
   * How to resolve or create the session this invocation runs in. Omit to
   * use a single default session per agent in continue_or_create mode. Set
   * `session.thinking_effort` to override the agent's reasoning-effort
   * default for this session.
   */
  session?: InvokeSessionSpec;
  /**
   * Inline agent definition (instructions, model, effort, timeout, toolkits,
   * skills) sent with the invocation instead of using the agent stored in
   * Mobius. Set fields replace the agent's values; omitted fields keep them.
   * Mobius remembers the config on the session and reuses it on later turns
   * until a new one is sent. Omit to run the agent on its stored definition.
   */
  config?: InlineAgentConfig;
  /**
   * Optional messaging provider/channel routing context (Slack, Telegram,
   * …) recorded on the started turn.
   */
  channelContext?: ChannelContext;
}

/**
 * A single decoded frame from a session SSE stream. eventType is the
 * authoritative SSE `event:` name (e.g. "user.message", "turn.completed",
 * "tool.call") — {@link SessionStreamFrame} is a reference-only union that
 * cannot be shape-matched from data alone, so dispatch on eventType and cast
 * data to the corresponding payload type.
 */
export interface SessionStreamEvent {
  eventType: string;
  data: SessionStreamFrame;
}

export interface GetSessionTranscriptOptions {
  /** Opaque resume cursor from a prior snapshot or stream; omit for a bootstrap tail. */
  cursor?: string;
  /** Opaque fixed-cut continuation (`next_page_token`) when draining an incremental cycle. */
  pageToken?: string;
  /** Max messages per page. */
  limit?: number;
  signal?: AbortSignal;
}

export interface StreamSessionTranscriptOptions {
  /** Opaque resume cursor; omit to hydrate from the live tail. */
  cursor?: string;
  signal?: AbortSignal;
}

export interface WatchSessionTranscriptOptions {
  /** Opaque resume cursor for the first connection. Ignored if `transcript` carries one. */
  cursor?: string;
  /**
   * Existing view to continue folding into (e.g. one bootstrapped from
   * getSessionTranscript pages). Omit to start fresh.
   */
  transcript?: SessionTranscript;
  /** Delay before reconnecting after a dropped connection (not a clean `rotate`). Defaults to 1000. */
  reconnectDelayMs?: number;
  signal?: AbortSignal;
}

export type TranscriptConnectionState = "open" | "reconnecting" | "ended";

export interface TranscriptUpdate {
  frame: SessionTranscriptFrame;
  cursor: string | null;
  transcript: SessionTranscript;
  connection: TranscriptConnectionState;
  reconnectCount: number;
}

export interface TranscriptDiagnostics {
  status: string;
  cursor: string | null;
  ready: boolean;
  reconnectCount: number;
  lastFrameType?: string;
  lastFrameAt?: string;
  connection: TranscriptConnectionState | "idle";
}

/** @deprecated Use {@link TranscriptDiagnostics}. */
export type TurnDiagnostics = TranscriptDiagnostics;

export interface ListSessionsOptions {
  agentId?: string;
  sessionKey?: string;
  status?: string;
  scope?: string;
  provider?: string;
  integrationId?: string;
  since?: string;
  cursor?: string;
  limit?: number;
}

export interface ListSessionMessagesOptions {
  afterSequence?: number;
  beforeSequence?: number;
  order?: "asc" | "desc";
  limit?: number;
}

export interface ListSessionTurnsOptions {
  ids?: string[];
  order?: "asc" | "desc";
  cursor?: string;
  limit?: number;
}

export interface NudgeSessionOptions {
  content: string;
  idempotencyKey?: string;
  metadata?: Record<string, unknown>;
  wake?: boolean;
}

export interface ListSessionNudgesOptions {
  status?: SessionNudgeStatus[];
  order?: "asc" | "desc";
  cursor?: string;
  limit?: number;
}

export class Client {
  private readonly baseURL: string;
  readonly project: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof globalThis.fetch;
  private readonly logger?: (event: ClientLogEvent) => void;

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    const explicitProject = opts.project ?? opts.namespace;
    const handleInKey = extractHandleFromApiKey(opts.apiKey);
    if (handleInKey != null) {
      if (
        explicitProject != null &&
        explicitProject !== DEFAULT_PROJECT &&
        explicitProject !== handleInKey
      ) {
        throw new ConfigError(
          `project=${JSON.stringify(explicitProject)} conflicts with the handle embedded in the API key (${JSON.stringify(handleInKey)})`,
        );
      }
      this.project = handleInKey;
    } else {
      this.project = explicitProject ?? DEFAULT_PROJECT;
    }
    this.headers = {
      Authorization: `Bearer ${opts.apiKey}`,
      "Content-Type": "application/json",
    };
    this.timeoutMs = opts.timeoutMs ?? 60_000;
    this.logger = opts.logger;
    this.fetchFn = wrapFetchWithRetry(
      (input, init) => globalThis.fetch(input, init),
      {
        maxRetries: opts.retry ?? DEFAULT_MAX_RETRIES,
        onRetry: (event) =>
          this.log({
            type: "retry",
            event: event.reason,
            method: event.method,
            status: event.status,
            attempt: event.attempt,
            waitSeconds: event.waitSeconds,
          }),
      },
    );
  }

  workerSocketURL(): string {
    const url = new URL(this.baseURL);
    if (url.protocol === "http:") url.protocol = "ws:";
    if (url.protocol === "https:") url.protocol = "wss:";
    url.pathname = `${url.pathname.replace(/\/$/, "")}/v1/projects/${encodeURIComponent(this.project)}/workers/socket`;
    url.search = "";
    return url.toString();
  }

  async listLoops(opts: ListLoopsOptions = {}): Promise<LoopListResponse> {
    const resp = await this.request(withQuery("/v1/projects/:project/loops", opts), {
      method: "GET",
    });
    return (await resp.json()) as LoopListResponse;
  }

  async getLoop(id: string): Promise<Loop> {
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Loop;
  }

  async createLoop(opts: LoopOptions): Promise<Loop> {
    const body = {
      schema_version: "1" as const,
      ...(opts.spec ?? {}),
      ...removeUndefined({
        name: opts.name,
        description: opts.description,
        agent_id: opts.agent_id,
        default_config: opts.default_config,
        settings: opts.settings,
        tags: opts.tags,
      }),
    } as CreateLoopRequest;
    const resp = await this.request("/v1/projects/:project/loops", {
      method: "POST",
      body,
    });
    return (await resp.json()) as Loop;
  }

  async updateLoop(id: string, opts: UpdateLoopOptions): Promise<Loop> {
    const { spec, ...meta } = opts;
    const body = {
      ...(spec ?? {}),
      ...removeUndefined(meta),
    } as UpdateLoopRequest;
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "PATCH", body },
    );
    return (await resp.json()) as Loop;
  }

  async deleteLoop(id: string): Promise<void> {
    await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
  }

  async startRun(
    loopId: string,
    opts: StartRunOptions = {},
  ): Promise<LoopRun> {
    const body: StartLoopRunRequest = removeUndefined({
      event: opts.event,
      config: opts.config,
      meta: opts.meta,
      source: opts.source,
      idempotency_key: opts.external_id,
    });
    const resp = await this.request(
      `/v1/projects/:project/loops/${encodeURIComponent(loopId)}/runs`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async listRuns(opts: ListRunsOptions = {}): Promise<LoopRunListResponse> {
    const resp = await this.request(
      withQuery("/v1/projects/:project/runs", opts),
      { method: "GET" },
    );
    return (await resp.json()) as LoopRunListResponse;
  }

  async getRun(runId: string): Promise<LoopRun> {
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as LoopRun;
  }

  async cancelRun(runId: string, reason?: string): Promise<LoopRun> {
    const body: CancelLoopRunRequest = removeUndefined({ reason });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/cancel`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async signalRun(
    runId: string,
    stepKey: string,
    result?: Record<string, unknown>,
  ): Promise<LoopRun> {
    const body: SignalLoopRunRequest = removeUndefined({
      step_key: stepKey,
      result,
    });
    const resp = await this.request(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/signals`,
      { method: "POST", body },
    );
    return (await resp.json()) as LoopRun;
  }

  async listSessions(opts: ListSessionsOptions = {}): Promise<SessionListResponse> {
    const path = withQuery("/v1/projects/:project/sessions", {
      agent_id: opts.agentId,
      session_key: opts.sessionKey,
      status: opts.status,
      scope: opts.scope,
      provider: opts.provider,
      integration_id: opts.integrationId,
      since: opts.since,
      cursor: opts.cursor,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SessionListResponse;
  }

  async getSession(sessionId: string): Promise<Session> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Session;
  }

  async cancelSession(sessionId: string, opts: { force?: boolean } = {}): Promise<Session> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/cancel`,
      { force: opts.force },
    );
    const resp = await this.request(path, { method: "POST" });
    return (await resp.json()) as Session;
  }

  async compactSession(sessionId: string): Promise<Session> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/compact`,
      { method: "POST" },
    );
    return (await resp.json()) as Session;
  }

  async listSessionMessages(
    sessionId: string,
    opts: ListSessionMessagesOptions = {},
  ): Promise<SessionMessageListResponse> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/messages`,
      {
        after_sequence: opts.afterSequence,
        before_sequence: opts.beforeSequence,
        order: opts.order,
        limit: opts.limit,
      },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SessionMessageListResponse;
  }

  async nudgeSession(
    sessionId: string,
    opts: NudgeSessionOptions,
  ): Promise<SessionNudgeAck> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/nudges`,
      {
        method: "POST",
        body: removeUndefined({
          content: opts.content,
          idempotency_key: opts.idempotencyKey,
          metadata: opts.metadata,
          wake: opts.wake,
        }),
      },
    );
    return (await resp.json()) as SessionNudgeAck;
  }

  async listNudges(
    sessionId: string,
    opts: ListSessionNudgesOptions = {},
  ): Promise<SessionNudgeListResponse> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/nudges`,
      { status: opts.status, order: opts.order, cursor: opts.cursor, limit: opts.limit },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SessionNudgeListResponse;
  }

  async getNudge(sessionId: string, nudgeId: string): Promise<SessionNudge> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/nudges/${encodeURIComponent(nudgeId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as SessionNudge;
  }

  async cancelNudge(sessionId: string, nudgeId: string): Promise<SessionNudge> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/nudges/${encodeURIComponent(nudgeId)}/cancel`,
      { method: "POST" },
    );
    return (await resp.json()) as SessionNudge;
  }

  async listTurns(
    sessionId: string,
    opts: ListSessionTurnsOptions = {},
  ): Promise<AgentTurnListResponse> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/turns`,
      { ids: opts.ids, order: opts.order, cursor: opts.cursor, limit: opts.limit },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as AgentTurnListResponse;
  }

  async getTurn(sessionId: string, turnId: string): Promise<AgentTurn> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/turns/${encodeURIComponent(turnId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as AgentTurn;
  }

  async cancelTurn(sessionId: string, turnId: string): Promise<AgentTurn> {
    const resp = await this.request(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/turns/${encodeURIComponent(turnId)}/cancel`,
      { method: "POST" },
    );
    return (await resp.json()) as AgentTurn;
  }

  /**
   * Resolves (or creates) a session, appends opts.content as the caller's
   * input message, and starts an agent turn — collapsing the
   * create-or-resolve-session + start-turn sequence into one retryable
   * call. This is the entry point for a product backend (an embedded app,
   * a Slack handler, a Telegram bot) calling per inbound message.
   *
   * Resolves once the turn is accepted. The returned {@link TurnTranscript}
   * carries the turn's identity (`id`, `sessionId`, `status`) immediately and
   * its live transcript on demand: the stream is lazy, so `for await` the
   * handle to render the turn as it runs, or never iterate for
   * fire-and-forget. Use {@link Client.invokeAgentStream} instead to observe
   * the turn's activity inline on the same connection with v1 session-stream
   * framing.
   */
  async invokeAgent(
    opts: InvokeAgentOptions,
    streamOpts: { signal?: AbortSignal } = {},
  ): Promise<TurnTranscript> {
    const body = invokeAgentRequest(opts);
    const resp = await this.request("/v1/projects/:project/agents/invoke", {
      method: "POST",
      body,
      signal: streamOpts.signal,
    });
    const ack = (await resp.json()) as TurnAck;
    const transcript = new SessionTranscript();
    transcript.seed(ack);
    return new TurnTranscript(this, ack, transcript, streamOpts.signal);
  }

  /**
   * Behaves like {@link Client.invokeAgent} but streams the turn's
   * activity inline on the same connection instead of waiting for a
   * TurnAck, identical to framing from `GET …/sessions/{id}/stream`.
   */
  async *invokeAgentStream(
    opts: InvokeAgentOptions,
    streamOpts: { signal?: AbortSignal } = {},
  ): AsyncGenerator<SessionStreamEvent> {
    const path = "/v1/projects/:project/agents/invoke";
    const body = invokeAgentRequest(opts);
    const resp = await this.fetchFn(this.url(path), {
      method: "POST",
      headers: { ...this.headers, Accept: "text/event-stream" },
      body: JSON.stringify(body),
      signal: streamOpts.signal,
    });
    if (!resp.ok) {
      if (resp.status === 401) throw new AuthRevokedError();
      throw await responseError(resp, "POST", path, true);
    }
    if (!resp.body) {
      throw new Error("mobius API POST agents/invoke: response body is not readable");
    }
    for await (const evt of parseSSE(resp.body)) {
      if (!evt.event || !evt.data) continue;
      yield { eventType: evt.event, data: JSON.parse(evt.data) as SessionStreamFrame };
    }
  }

  /**
   * Fetch a session transcript snapshot (session-stream v2). Without a cursor
   * this is a bootstrap tail (latest final page + all live rows and turns);
   * with a cursor it drains everything after it toward a fixed upper cut —
   * continue with the returned `next_page_token` until `has_more` is false.
   * Fold each page into a {@link SessionTranscript} with
   * `applySnapshot`; polling is the same protocol the stream accelerates.
   */
  async getSessionTranscript(
    sessionId: string,
    opts: GetSessionTranscriptOptions = {},
  ): Promise<SessionTranscriptSnapshot> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/transcript`,
      { cursor: opts.cursor, page_token: opts.pageToken, limit: opts.limit },
    );
    const resp = await this.request(path, { method: "GET", signal: opts.signal });
    return (await resp.json()) as SessionTranscriptSnapshot;
  }

  /**
   * Open one session-transcript SSE connection and yield each decoded frame
   * with its SSE `id:` (the resume cursor). This is the low-level primitive;
   * feed the events to a {@link SessionTranscript}'s `apply`, or use
   * {@link Client.watchSessionTranscript} for the managed connection loop
   * (reconnect on `rotate`, stop on `idle`).
   */
  async *streamSessionTranscript(
    sessionId: string,
    opts: StreamSessionTranscriptOptions = {},
  ): AsyncGenerator<TranscriptStreamEvent> {
    const path = withQuery(
      `/v1/projects/:project/sessions/${encodeURIComponent(sessionId)}/transcript/stream`,
      { cursor: opts.cursor },
    );
    const resp = await this.fetchFn(this.url(path), {
      method: "GET",
      headers: { ...this.headers, Accept: "text/event-stream" },
      signal: opts.signal,
    });
    if (!resp.ok) {
      if (resp.status === 401) throw new AuthRevokedError();
      throw await responseError(resp, "GET", path, true);
    }
    if (!resp.body) {
      throw new Error(
        "mobius API GET session transcript stream: response body is not readable",
      );
    }
    for await (const evt of parseSSE(resp.body)) {
      if (!evt.data) continue;
      yield {
        frame: JSON.parse(evt.data) as SessionTranscriptFrame,
        id: evt.id,
      };
    }
  }

  /**
   * Drive a {@link SessionTranscript} across the full session-transcript
   * stream, yielding it after every applied frame. Owns the connection loop:
   * reconnects with the current cursor on `stream.end rotate` (and after a
   * dropped connection), and returns on `stream.end idle`. Reconnect is the
   * same code path as the first connect — the view keeps its rows and only
   * resets `ready`. On `idle` the caller can poll {@link
   * Client.getSessionTranscript} and reopen when `resume_cursor` moves.
   */
  async *watchSessionTranscript(
    sessionId: string,
    opts: WatchSessionTranscriptOptions = {},
  ): AsyncGenerator<SessionTranscript> {
    for await (const update of this.watchSessionTranscriptUpdates(sessionId, opts)) {
      const eventType = (update.frame as { event_type?: string }).event_type;
      if (eventType !== "stream.end") yield update.transcript;
    }
  }

  /** Yield each observed transcript frame together with the accumulated view. */
  async *watchSessionTranscriptUpdates(
    sessionId: string,
    opts: WatchSessionTranscriptOptions = {},
  ): AsyncGenerator<TranscriptUpdate> {
    const transcript = opts.transcript ?? new SessionTranscript();
    if (opts.cursor && !transcript.cursor) transcript.cursor = opts.cursor;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    let reconnectCount = 0;
    let firstConnection = true;
    for (;;) {
      transcript._resetReady();
      let rotate = false;
      if (!firstConnection) reconnectCount += 1;
      firstConnection = false;
      this.log({ type: "stream", event: reconnectCount ? "reconnect" : "open", path: sessionId });
      try {
        for await (const ev of this.streamSessionTranscript(sessionId, {
          cursor: transcript.cursor ?? undefined,
          signal: opts.signal,
        })) {
          transcript.apply(ev);
          const eventType = (ev.frame as { event_type?: string }).event_type;
          let connection: TranscriptConnectionState = "open";
          if (eventType === "stream.end") {
            if ((ev.frame as StreamEndFrame).reason === "idle") {
              connection = "ended";
            } else {
              connection = "reconnecting";
              rotate = true;
            }
          }
          this.log({
            type: "stream",
            event: eventType === "stream.ready" ? "ready" : eventType === "stream.end" ? (connection === "ended" ? "idle" : "rotate") : "frame",
            path: sessionId,
            frameType: eventType,
          });
          yield {
            frame: ev.frame,
            cursor: transcript.cursor,
            transcript,
            connection,
            reconnectCount,
          };
          if (connection === "ended") return;
          if (connection === "reconnecting") break;
        }
      } catch (err) {
        if (opts.signal?.aborted) throw err;
        // Reconnect only on transient failures; a permanent status
        // (401/403/404, or a 5xx other than 503) is surfaced to the caller
        // instead of looping forever.
        if (!isRetryableStreamError(err)) throw err;
        this.log({
          type: "stream",
          event: "transport_error",
          path: sessionId,
          error: errorMessage(err),
        });
      }
      if (opts.signal?.aborted) return;
      if (!rotate) await delay(reconnectDelayMs, opts.signal);
    }
  }

  async *watchRun(
    runId: string,
    opts: WatchRunOptions = {},
  ): AsyncGenerator<LoopRunEvent> {
    const path = withQuery(
      `/v1/projects/:project/runs/${encodeURIComponent(runId)}/events.stream`,
      opts.since && opts.since > 0 ? { after_sequence: opts.since } : {},
    );
    const resp = await this.fetchFn(this.url(path), {
      method: "GET",
      headers: this.headers,
      signal: opts.signal,
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API GET ${path}: HTTP ${resp.status}: ${text}`);
    }
    if (!resp.body) {
      throw new Error("mobius API GET run events: response body is not readable");
    }
    for await (const evt of parseSSE(resp.body)) {
      if (!evt.data) continue;
      yield JSON.parse(evt.data) as LoopRunEvent;
    }
  }

  async waitRun(runId: string, opts: WaitRunOptions = {}): Promise<LoopRun> {
    let since = opts.since ?? 0;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    for (;;) {
      const run = await this.getRun(runId);
      if (isTerminalRunStatus(run.status)) return run;
      try {
        for await (const ev of this.watchRun(runId, { ...opts, since })) {
          if (ev.sequence > since) since = ev.sequence;
          const status = ev.payload?.status;
          if (typeof status === "string" && isTerminalRunStatus(status)) {
            return await this.getRun(runId);
          }
        }
      } catch (err) {
        if (opts.signal?.aborted) throw err;
      }
      await delay(reconnectDelayMs, opts.signal);
    }
  }

  private async request(
    path: string,
    opts: { method: string; body?: unknown; signal?: AbortSignal | undefined },
  ): Promise<Response> {
    const started = Date.now();
    const timeout = AbortSignal.timeout(this.timeoutMs);
    const signal = opts.signal ? anySignal(opts.signal, timeout) : timeout;
    const init: RequestInit = {
      method: opts.method,
      headers: this.headers,
      signal,
    };
    if (opts.body != null) init.body = JSON.stringify(opts.body);
    let resp: Response;
    try {
      resp = await this.fetchFn(this.url(path), init);
    } catch (err) {
      this.log({
        type: "request",
        event: "transport_error",
        method: opts.method,
        path,
        durationMs: Date.now() - started,
        error: errorMessage(err),
      });
      throw err;
    }
    this.log({
      type: "request",
      event: "complete",
      method: opts.method,
      path,
      status: resp.status,
      durationMs: Date.now() - started,
    });
    if (!resp.ok && resp.status !== 204) {
      if (resp.status === 401) throw new AuthRevokedError();
      throw await responseError(resp, opts.method, path);
    }
    return resp;
  }

  private log(event: ClientLogEvent): void {
    try {
      this.logger?.(event);
    } catch {
      // Diagnostics must never change request behavior.
    }
  }

  private url(path: string): string {
    return this.baseURL + path.replace(":project", encodeURIComponent(this.project));
  }
}

/**
 * A started agent turn and its live transcript, returned by
 * {@link Client.invokeAgent}. The identity fields (`id`, `sessionId`, …) are
 * available immediately; the transcript stream is lazy — iteration opens it,
 * so a caller that never iterates pays for nothing beyond the invoke itself.
 *
 * `for await` the handle to render the turn as it streams; it yields after
 * every state change and completes once this turn reaches a terminal
 * `turn.upsert` (already applied — `status` reflects it), reconnecting through
 * stream rotations and dropped connections along the way. The full session
 * stream is consumed internally so the resume cursor stays valid when other
 * turns interleave; {@link messages} is scoped to this turn, and
 * {@link transcript} exposes the whole session view.
 *
 * ```ts
 * const turn = await client.invokeAgent({ agentName: "support", content });
 * for await (const t of turn) render(t.messages());
 * turn.status; // "completed"
 * ```
 */
export class TurnTranscript implements AsyncIterable<TurnTranscript> {
  /** Turn id. */
  readonly id: string;
  /** Id of the session this turn runs in. */
  readonly sessionId: string;
  /**
   * Durable v1 stream cursor from the turn-start response; pass it as
   * `after_sequence` to `GET …/sessions/{id}/stream` to follow this turn on
   * the v1 session stream instead.
   */
  readonly afterSequence: number;
  /** True when a repeated idempotency key resumed an existing turn. */
  readonly deduped: boolean;
  /** Full session view the stream folds into. */
  readonly transcript: SessionTranscript;

  readonly #client: Client;
  readonly #signal?: AbortSignal;
  // Set when the acked turn was already terminal (a deduped resume of a
  // completed turn): there is nothing to stream, so iteration fetches the
  // snapshot (all pages) instead, making messages() complete either way.
  #hydrate: boolean;
  #diagnostics: TranscriptDiagnostics = {
    status: "",
    cursor: null,
    ready: false,
    reconnectCount: 0,
    connection: "idle",
  };

  /** @internal Constructed by {@link Client.invokeAgent}. */
  constructor(
    client: Client,
    ack: TurnAck,
    transcript: SessionTranscript,
    signal?: AbortSignal,
  ) {
    this.#client = client;
    this.#signal = signal;
    this.id = ack.turn.id;
    this.sessionId = ack.session.id;
    this.afterSequence = ack.after_sequence;
    this.deduped = ack.deduped ?? false;
    this.transcript = transcript;
    this.#hydrate = isTerminalTurnStatus(ack.turn.status);
    this.#diagnostics.status = ack.turn.status;
    this.#diagnostics.cursor = transcript.cursor;
  }

  /**
   * The turn's lifecycle status ("queued", "running", "completed", …). It is
   * live: each applied `turn.upsert` updates it.
   */
  get status(): string {
    return this.transcript.turn(this.id)?.status ?? "";
  }

  get errorType(): string | undefined {
    return this.transcript.turn(this.id)?.error_type ?? undefined;
  }

  get errorMessage(): string | undefined {
    return this.transcript.turn(this.id)?.error_message ?? undefined;
  }

  get error(): Error | undefined {
    if (this.status !== "failed") return undefined;
    const error = new Error(this.errorMessage ?? "Mobius turn failed");
    error.name = this.errorType ?? "MobiusTurnError";
    return error;
  }

  /** This turn's rows, in render order. */
  messages(): SessionTranscriptMessage[] {
    return this.transcript.messagesForTurn(this.id);
  }

  /** This turn's policy-light rendering projection. */
  renderableMessages(): SessionTranscriptMessage[] {
    return this.transcript.renderableMessagesForTurn(this.id);
  }

  /** Last observed transport and turn facts; no backend state is inferred. */
  diagnostics(): TranscriptDiagnostics {
    return { ...this.#diagnostics, status: this.status, cursor: this.transcript.cursor, ready: this.transcript.ready };
  }

  /**
   * Yield observed protocol frames while folding the full session. The
   * terminal update is exposed only after its incremental durable snapshot is
   * reconciled; snapshot failures reject iteration.
   */
  async *updates(): AsyncGenerator<TranscriptUpdate> {
    if (this.#hydrate) {
      await this.#hydrateSnapshot();
      return;
    }
    const current = this.transcript.turn(this.id);
    if (current && isTerminalTurnStatus(current.status)) return;
    for await (const update of this.#client.watchSessionTranscriptUpdates(
      this.sessionId,
      { transcript: this.transcript, signal: this.#signal },
    )) {
      const frameType = (update.frame as { event_type?: string }).event_type;
      const turn = update.transcript.turn(this.id);
      const terminal = turn != null && isTerminalTurnStatus(turn.status);
      if (terminal) {
        await this.#reconcileSnapshot(this.transcript.cursor ?? undefined);
      }
      const exposedUpdate: TranscriptUpdate = terminal
        ? { ...update, cursor: this.transcript.cursor, connection: "ended" }
        : update;
      this.#diagnostics = {
        status: this.status,
        cursor: exposedUpdate.cursor,
        ready: this.transcript.ready,
        reconnectCount: exposedUpdate.reconnectCount,
        lastFrameType: frameType,
        lastFrameAt: new Date().toISOString(),
        connection: exposedUpdate.connection,
      };
      yield exposedUpdate;
      if (terminal) return;
    }
  }

  async *[Symbol.asyncIterator](): AsyncIterator<TurnTranscript> {
    if (this.#hydrate) {
      await this.#hydrateSnapshot();
      yield this;
      return;
    }
    // Already terminal (a completed prior iteration): nothing left to stream.
    const current = this.transcript.turn(this.id);
    if (current && isTerminalTurnStatus(current.status)) return;
    for await (const update of this.updates()) {
      yield this;
      void update;
    }
  }

  async #hydrateSnapshot(): Promise<void> {
    this.#hydrate = false;
    await this.#reconcileSnapshot();
    this.#diagnostics = {
      status: this.status,
      cursor: this.transcript.cursor,
      ready: this.transcript.ready,
      reconnectCount: 0,
      connection: "ended",
    };
  }

  async #reconcileSnapshot(cursor?: string): Promise<void> {
    let pageToken: string | undefined;
    do {
      const snap = await this.#client.getSessionTranscript(this.sessionId, {
        cursor: pageToken ? undefined : cursor,
        pageToken,
        signal: this.#signal,
      });
      this.transcript.applySnapshot(snap);
      pageToken = snap.has_more ? snap.next_page_token : undefined;
    } while (pageToken);
  }
}

export function isTerminalRunStatus(status: LoopRunStatus | string): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

function anySignal(...signals: AbortSignal[]): AbortSignal {
  const controller = new AbortController();
  for (const signal of signals) {
    if (signal.aborted) {
      controller.abort(signal.reason);
      break;
    }
    signal.addEventListener("abort", () => controller.abort(signal.reason), {
      once: true,
    });
  }
  return controller.signal;
}

function withQuery(path: string, params: object): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value == null || value === "") continue;
    if (Array.isArray(value)) {
      for (const item of value) query.append(key, String(item));
    } else {
      query.set(key, String(value));
    }
  }
  const qs = query.toString();
  return qs ? `${path}?${qs}` : path;
}

function invokeAgentRequest(opts: InvokeAgentOptions): InvokeAgentRequest {
  if (!opts.agentId && !opts.agentName) {
    throw new Error("mobius: invoke agent: agentId or agentName is required");
  }
  if (!opts.content || opts.content.length === 0) {
    throw new Error("mobius: invoke agent: content is required");
  }
  return removeUndefined({
    agent_ref: removeUndefined({
      id: opts.agentId,
      name: opts.agentName,
    }) as AgentRef,
    input: removeUndefined({
      content: opts.content,
      idempotency_key: opts.idempotencyKey,
      metadata: opts.inputMetadata,
    }) as InvokeInput,
    session: opts.session,
    config: opts.config,
    channel_context: opts.channelContext,
  }) as InvokeAgentRequest;
}

function removeUndefined<T extends object>(obj: T): T {
  return Object.fromEntries(
    Object.entries(obj).filter(([, v]) => v !== undefined),
  ) as T;
}

async function responseError(
  response: Response,
  method: string,
  path: string,
  stream = false,
): Promise<Error> {
  const text = await response.text().catch(() => "");
  let envelope: unknown;
  try {
    envelope = text ? JSON.parse(text) : undefined;
  } catch {
    envelope = undefined;
  }
  const error =
    isRecord(envelope) && isRecord(envelope.error) ? envelope.error : undefined;
  const code = error && typeof error.code === "string" ? error.code : undefined;
  const message =
    error && typeof error.message === "string"
      ? error.message
      : `mobius API ${method} ${path}: HTTP ${response.status}: ${text}`;
  const details = error && isRecord(error.details) ? error.details : undefined;
  const requestId =
    response.headers.get("X-Request-Id") ??
    response.headers.get("X-Request-ID") ??
    undefined;
  const retryAfter = parseRetryAfterSeconds(response.headers.get("Retry-After"));
  if (stream) {
    return new StreamHTTPError(response.status, message, {
      code: code ?? "http_error",
      details,
      requestId,
      retryAfter,
    });
  }
  if (code) {
    return new MobiusAPIError({
      status: response.status,
      code,
      message,
      details,
      requestId,
      retryAfter,
    });
  }
  return new Error(message);
}

function parseRetryAfterSeconds(value: string | null): number | undefined {
  if (!value) return undefined;
  const seconds = Number(value);
  if (Number.isFinite(seconds)) return Math.max(0, seconds);
  const at = Date.parse(value);
  return Number.isNaN(at) ? undefined : Math.max(0, (at - Date.now()) / 1000);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

// isRetryableStreamError reports whether a failed stream connection should be
// reconnected. Mirrors docs/retries.md: reconnect only on 429/503 and
// transport/parse failures; 401 (AuthRevokedError) and every other HTTP status
// propagate to the caller.
function isRetryableStreamError(err: unknown): boolean {
  if (err instanceof AuthRevokedError) return false;
  if (err instanceof RateLimitError) return true; // 429 the transport gave up on
  if (err instanceof StreamHTTPError) {
    return err.status === 429 || err.status === 503;
  }
  return true; // fetch rejection / body dropped mid-stream
}

async function delay(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) return;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    if (signal) {
      signal.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          reject(signal.reason ?? new Error("aborted"));
        },
        { once: true },
      );
    }
  });
}

interface SSEEvent {
  event?: string;
  id?: string;
  data: string;
}

async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  // Per the SSE spec the last-event-id persists across events until an `id:`
  // line changes it — the transcript stream relies on this so a frame that
  // repeats already-delivered state can omit `id:` without regressing the
  // resume cursor.
  let lastId: string | undefined;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      for (;;) {
        const match = /\r?\n\r?\n/.exec(buffer);
        if (!match) break;
        const raw = buffer.slice(0, match.index);
        buffer = buffer.slice(match.index + match[0].length);
        const lines = raw.split(/\r?\n/);
        const data = lines
          .filter((line) => line.startsWith("data:"))
          .map((line) => line.slice(5).trimStart())
          .join("\n");
        const eventLine = lines.find((line) => line.startsWith("event:"));
        const event = eventLine ? eventLine.slice(6).trimStart() : undefined;
        const idLine = lines.find((line) => line.startsWith("id:"));
        if (idLine) lastId = idLine.slice(3).trimStart();
        yield { event, id: lastId, data };
      }
    }
  } finally {
    reader.releaseLock();
  }
}
