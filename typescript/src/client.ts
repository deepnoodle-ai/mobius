import type {
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
  SessionStreamFrame,
  SessionTranscriptFrame,
  SessionTranscriptSnapshot,
  SessionTranscriptTurn,
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
  SessionTranscriptReducer,
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
  /** Opaque resume cursor for the first connection. Ignored if `reducer` carries one. */
  cursor?: string;
  /**
   * Reducer to fold frames into. Omit to start fresh. Pass a pre-seeded reducer
   * (e.g. from a bootstrap snapshot or a StartTurn ack) to continue its state.
   */
  reducer?: SessionTranscriptReducer;
  /** Delay before reconnecting after a dropped connection (not a clean `rotate`). Defaults to 1000. */
  reconnectDelayMs?: number;
  signal?: AbortSignal;
}

export class Client {
  private readonly baseURL: string;
  readonly project: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof globalThis.fetch;

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
    this.fetchFn = wrapFetchWithRetry(
      (input, init) => globalThis.fetch(input, init),
      { maxRetries: opts.retry ?? DEFAULT_MAX_RETRIES },
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

  /**
   * Resolves (or creates) a session, appends opts.content as the caller's
   * input message, and starts an agent turn — collapsing the
   * create-or-resolve-session + start-turn sequence into one retryable
   * call. This is the entry point for a product backend (an embedded app,
   * a Slack handler, a Telegram bot) calling per inbound message.
   *
   * Returns once the turn is accepted; the response's after_sequence is a
   * durable stream cursor for `GET …/sessions/{id}/stream`. Use
   * {@link Client.invokeAgentStream} instead to observe the turn's
   * activity inline on the same connection.
   */
  async invokeAgent(opts: InvokeAgentOptions): Promise<TurnAck> {
    const body = invokeAgentRequest(opts);
    const resp = await this.request("/v1/projects/:project/agents/invoke", {
      method: "POST",
      body,
    });
    return (await resp.json()) as TurnAck;
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
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API POST ${path}: HTTP ${resp.status}: ${text}`);
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
   * Fold each page into a {@link SessionTranscriptReducer} with
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
   * feed the frames to a {@link SessionTranscriptReducer}, or use
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
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API GET ${path}: HTTP ${resp.status}: ${text}`);
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
   * Drive a {@link SessionTranscriptReducer} across the full session-transcript
   * stream, yielding it after every applied frame. Owns the connection loop:
   * reconnects with the current cursor on `stream.end rotate` (and after a
   * dropped connection), and returns on `stream.end idle`. Reconnect is the
   * same code path as the first connect — the reducer keeps its rows and only
   * resets `ready`. On `idle` the caller can poll {@link
   * Client.getSessionTranscript} and reopen when `resume_cursor` moves.
   */
  async *watchSessionTranscript(
    sessionId: string,
    opts: WatchSessionTranscriptOptions = {},
  ): AsyncGenerator<SessionTranscriptReducer> {
    const reducer = opts.reducer ?? new SessionTranscriptReducer();
    if (opts.cursor && !reducer.cursor) reducer.cursor = opts.cursor;
    const reconnectDelayMs = opts.reconnectDelayMs ?? 1000;
    for (;;) {
      reducer.ready = false;
      let rotate = false;
      try {
        for await (const { frame, id } of this.streamSessionTranscript(sessionId, {
          cursor: reducer.cursor ?? undefined,
          signal: opts.signal,
        })) {
          reducer.apply(frame, id);
          const eventType = (frame as { event_type?: string }).event_type;
          if (eventType === "stream.end") {
            if ((frame as StreamEndFrame).reason === "idle") return;
            rotate = true; // reconnect immediately with the current cursor
            break;
          }
          yield reducer;
        }
      } catch (err) {
        if (opts.signal?.aborted) throw err;
        // otherwise fall through and reconnect
      }
      if (opts.signal?.aborted) return;
      if (!rotate) await delay(reconnectDelayMs, opts.signal);
    }
  }

  /**
   * Behaves like {@link Client.invokeAgent} but streams the started turn's
   * transcript inline (session-stream v2). Starts the turn, seeds a reducer
   * with the ack's caller row and turn, then opens the transcript stream from
   * the ack's cursor and yields the reducer after each change, returning when
   * this turn reaches a terminal `turn.upsert`. The full session stream is
   * consumed internally so the resume cursor stays valid even when other turns
   * interleave; filter the reducer with `messagesForTurn(turnId)` to render
   * only this turn.
   */
  async *invokeAgentTranscript(
    opts: InvokeAgentOptions,
    streamOpts: { signal?: AbortSignal } = {},
  ): AsyncGenerator<SessionTranscriptReducer> {
    const ack = await this.invokeAgent(opts);
    const turnId = ack.turn.id;
    const reducer = new SessionTranscriptReducer();
    if (ack.user_message) reducer.rows.set(ack.user_message.id, ack.user_message);
    reducer.turns.set(turnId, ack.turn as unknown as SessionTranscriptTurn);
    reducer.cursor = ack.resume_cursor ?? null;
    yield reducer;
    if (isTerminalTurnStatus(ack.turn.status)) return; // deduped/already-terminal
    for await (const r of this.watchSessionTranscript(ack.session.id, {
      reducer,
      signal: streamOpts.signal,
    })) {
      yield r;
      const turn = r.turns.get(turnId);
      if (turn && isTerminalTurnStatus(turn.status)) return;
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
    const timeout = AbortSignal.timeout(this.timeoutMs);
    const signal = opts.signal ? anySignal(opts.signal, timeout) : timeout;
    const init: RequestInit = {
      method: opts.method,
      headers: this.headers,
      signal,
    };
    if (opts.body != null) init.body = JSON.stringify(opts.body);
    const resp = await this.fetchFn(this.url(path), init);
    if (!resp.ok && resp.status !== 204) {
      if (resp.status === 401) throw new AuthRevokedError();
      const text = await resp.text().catch(() => "");
      throw new Error(`mobius API ${opts.method} ${path}: HTTP ${resp.status}: ${text}`);
    }
    return resp;
  }

  private url(path: string): string {
    return this.baseURL + path.replace(":project", encodeURIComponent(this.project));
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
