import type {
  ActionInvocationListResponse,
  Agent,
  AgentListResponse,
  AgentMemory,
  AgentMemoryChange,
  AgentMemoryChangeListResponse,
  AgentMemoryEntry,
  AgentMemoryEntryListResponse,
  AgentTurn,
  AgentTurnListResponse,
  AgentTurnOperationPolicy,
  AgentTurnOutputSource,
  AgentRef,
  Artifact,
  ApplyBlueprintRequest,
  BlueprintApplyResult,
  BlueprintBindingListResponse,
  BlueprintDeleteResult,
  BillingUsageEvent,
  BillingUsageEventListResponse,
  ChannelContext,
  CreateAgentRequest,
  CreatePrincipalRequest,
  CreateRoleAssignmentRequest,
  CreateRoleRequest,
  ImportSkillRequest,
  Interaction,
  InteractionKind,
  InteractionListResponse,
  InvokeAgentRequest,
  InvokeInput,
  InvokeSessionSpec,
  MemoryKind,
  MemorySearchMode,
  OAuthReturnOrigins,
  PermissionCatalogResponse,
  Principal,
  PrincipalKind,
  PrincipalListResponse,
  PutOAuthReturnOriginsRequest,
  RuntimeContextItem,
  ReplaceSkillsRequest,
  SaveAgentMemoryEntryRequest,
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
  RespondToInteractionRequest,
  Role,
  RoleAssignment,
  RoleAssignmentListResponse,
  RoleListResponse,
  Skill,
  SkillAssignmentListResponse,
  SkillListResponse,
  SkillRequest,
  StartTurnRequest,
  StreamEndFrame,
  TagMap,
  TurnAck,
  TurnOutputSpec,
  UpdatePrincipalRequest,
  UpdateRoleRequest,
} from "./api/index.js";
import {
  DEFAULT_MAX_RETRIES,
  RateLimitError,
  wrapFetchWithRetry,
  type ReplaySafeRequestInit,
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

export interface CreateArtifactOptions {
  /** Display name or relative virtual path for the artifact. */
  name: string;
  /** Artifact bytes. A Blob's existing MIME type is preserved unless mimeType is supplied. */
  file: Blob | Uint8Array;
  /** Optional durable retry key for this one artifact. */
  idempotencyKey?: string;
  mimeType?: string;
  metadata?: Record<string, unknown>;
  signal?: AbortSignal;
}

export const DEFAULT_BASE_URL = "https://api.mobiusops.ai";

export class AuthRevokedError extends Error {
  constructor() {
    super("mobius: credential revoked");
    this.name = "AuthRevokedError";
  }
}

export class MobiusAPIError extends Error {
  /**
   * Adopt-mode create conflict code (409): the request names an identity
   * (agent name) that differs from the resource owning the
   * matched `external_ref`, or the match is soft-deleted — adopt never
   * resurrects or replaces a deleted resource.
   */
  static readonly EXTERNAL_IDENTITY_CONFLICT = "external_identity_conflict";

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
    init: Partial<
      Pick<MobiusAPIError, "code" | "details" | "requestId" | "retryAfter">
    > = {},
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
    message?: string,
  ) {
    super(
      message ??
        (workerInstanceId
          ? `mobius: worker_instance_id ${JSON.stringify(workerInstanceId)} is already registered by another live process`
          : "mobius: worker instance conflict"),
    );
    this.name = "WorkerInstanceConflictError";
  }
}

/** Filters for the org-scoped billing usage evidence reader. */
export interface ListBillingUsageEventsOptions {
  /** Inclusive ISO-8601 lower bound. Selects chronological `(recorded_at, id)` ordering. */
  recordedAfter?: string;
  periodStart?: string;
  counter?: string;
  sourceType?: string;
  sourceId?: string;
  jobId?: string;
  apiKeyId?: string;
  cursor?: string;
  limit?: number;
}

export interface InvokeAgentOptions {
  /** Agent identifier. Mutually exclusive with agentName. */
  agentId?: string;
  /** Org-unique agent name. Mutually exclusive with agentId. */
  agentName?: string;
  /** Ordered content blocks (text, images, …) for the caller's input message. Required. */
  content: Record<string, unknown>[];
  /**
   * Ordered application-owned state snapshots for this turn. Send full,
   * deterministic values; Mobius records only first use, changes, and
   * post-compaction re-entry.
   */
  context?: RuntimeContextItem[];
  /**
   * Dedup key scoped to the resolved session. A repeat call with the same
   * key resolves the same session and returns the existing invocation
   * without restarting it or starting a second one — derive it from the
   * provider event id for Slack/Telegram webhook retries. The SDK retries
   * admission automatically unless `session.mode` is `new`, which cannot be
   * safely replayed with a session-scoped key.
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
   * Policy for only this newly admitted turn. Its timeout takes precedence
   * over the stored agent timeout and is not saved on the session.
   */
  operation?: AgentTurnOperationPolicy;
  /**
   * Structured-output contract for this turn. When set, Mobius exposes a
   * reserved submit tool for the schema, validates the submission
   * server-side, and fails the turn if it never produces a schema-valid
   * object. Read the validated value from {@link TurnTranscript.output}.
   */
  output?: TurnOutputSpec;
  /**
   * Optional messaging provider/channel routing context (Slack, Telegram,
   * …) recorded on the started turn.
   */
  channelContext?: ChannelContext;
}

export interface StartTurnOptions {
  /** Ordered content blocks (text, images, …) for the caller's input message. */
  content: Record<string, unknown>[];
  /** Ordered application-owned state snapshots for this turn. */
  context?: RuntimeContextItem[];
  /**
   * Dedup key scoped to the existing session. A repeat returns the existing
   * invocation, writes no new input, and never restarts a terminal turn.
   */
  idempotencyKey?: string;
  /**
   * Policy for only this newly admitted turn. Its timeout takes precedence
   * over the stored agent timeout and is not saved on the session.
   */
  operation?: AgentTurnOperationPolicy;
  /**
   * Structured-output contract for this turn. See
   * {@link InvokeAgentOptions.output}; read the validated value from
   * {@link TurnTranscript.output}.
   */
  output?: TurnOutputSpec;
  /** Free-form caller metadata attached to the input message. */
  metadata?: Record<string, unknown>;
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
  /** Reopen after an idle close so turns started later are observed. Defaults to false. */
  follow?: boolean;
  signal?: AbortSignal;
}

export interface ListAgentsOptions {
  /** Exact org-unique agent name. */
  name?: string;
  principalId?: string;
  status?: string;
  limit?: number;
}

export interface CreateAgentOptions {
  /**
   * Makes the create safely retryable: when a live agent already carries
   * `externalRef`, it is returned unchanged (HTTP 200) instead of failing
   * with 409, and no fields are written. Requires `externalRef`. Adopt-mode
   * creates are retried on transient failures like idempotent requests,
   * because `external_ref` makes the create replay-safe server-side.
   * Conflicts that cannot be adopted fail with a {@link MobiusAPIError}
   * whose code is {@link MobiusAPIError.EXTERNAL_IDENTITY_CONFLICT}.
   */
  adoptExisting?: boolean;
  /**
   * Client-owned durable identity key, unique within the org and
   * assign-once. Required with `adoptExisting`; optional otherwise.
   */
  externalRef?: string;
}

export interface ListAgentMemoryEntriesOptions {
  /** Searches entry keys, kinds, summaries, and content. Omit to list. */
  query?: string;
  /**
   * Ranks a non-blank query: keyword (the server default), semantic, or
   * hybrid. Semantic and hybrid surface a 503
   * memory_semantic_search_unavailable {@link MobiusAPIError} when the index
   * is unavailable; the SDK never downgrades to keyword silently because that
   * would change result semantics — retry or fall back explicitly.
   */
  searchMode?: MemorySearchMode;
  /** Filters to a single memory kind. */
  kind?: MemoryKind;
  cursor?: string;
  limit?: number;
}

export interface ListAgentMemoryChangesOptions {
  /**
   * Opaque cursor returned as next_cursor by the previous page. Omit on the
   * first request to read retained changes oldest-first.
   */
  after?: string;
  limit?: number;
}

/**
 * One bounded synchronization step of an agent's memory change feed, returned
 * by {@link Client.syncAgentMemory}. When reset is true the supplied cursor
 * predated retained history (HTTP 410) and entries carries a full current
 * snapshot to replace local state; otherwise changes carries every feed item
 * after the cursor. nextCursor is the new feed position to persist.
 */
export type MemorySyncResult =
  | { reset: false; changes: AgentMemoryChange[]; nextCursor: string }
  | { reset: true; entries: AgentMemoryEntry[]; nextCursor: string };

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
  agentName?: string;
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
  /** Set to context to return caller-supplied runtime context rows. */
  include?: "context";
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

export interface ListBlueprintBindingsOptions {
  namespace?: string;
  blueprintKey?: string;
}

export interface DeleteBlueprintOptions {
  namespace?: string;
  deleteRetained?: boolean;
}

export interface SetBlueprintProtectionOptions {
  namespace?: string;
}

export interface ListInteractionsOptions {
  status?: "pending" | "completed" | "expired" | "cancelled";
  kind?: InteractionKind;
  sessionId?: string;
  targetUserId?: string;
  inbox?: boolean;
  cursor?: string;
  limit?: number;
}

export interface ListPrincipalsOptions {
  kind?: PrincipalKind;
  includeDisabled?: boolean;
  limit?: number;
}

export interface ListRoleAssignmentsOptions {
  principalId?: string;
  roleId?: string;
}

export interface ListRolesOptions {
  cursor?: string;
  limit?: number;
}

export interface ListActionInvocationsOptions {
  /** Filter to invocations from a specific job. */
  jobId?: string;
  /** Filter to invocations executed in a specific environment. */
  environmentId?: string;
  /** Filter to invocations of a specific action. */
  actionName?: string;
  /** Filter to an immutable custom or organization Action ID. */
  actionId?: string;
  /**
   * Filter by the scope that owned the selected definition: "platform",
   * "custom", or "organization".
   */
  definitionScope?: "platform" | "custom" | "organization";
  /** Filter to deliveries signed with a specific signing-secret version. */
  secretVersion?: number;
  /** Filter to a signed delivery identity. */
  deliveryId?: string;
  /** Filter to the request or dispatch correlation identity. */
  correlationId?: string;
  /** Filter by terminal status (e.g. "success", "failed"). */
  status?: string;
  cursor?: string;
  limit?: number;
}

export interface ListSkillsOptions {
  /**
   * Include read-only system skill templates. The server includes them by
   * default; pass false to omit them.
   */
  includeSystem?: boolean;
}

export interface ImportSkillOptions {
  /** Override the skill name derived from the document. */
  name?: string;
}

export class Client {
  private readonly baseURL: string;
  private readonly headers: Record<string, string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof globalThis.fetch;
  private readonly logger?: (event: ClientLogEvent) => void;
  private readonly agentNameCache = new Map<string, Agent>();

  constructor(opts: ClientOptions) {
    this.baseURL = (opts.baseURL ?? DEFAULT_BASE_URL).replace(/\/$/, "");
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
    url.pathname = `${url.pathname.replace(/\/$/, "")}/v1/workers/socket`;
    url.search = "";
    return url.toString();
  }

  /** Publish a private artifact using the client's org authorization. */
  async createArtifact(opts: CreateArtifactOptions): Promise<Artifact> {
    const idempotencyKey = normalizeIdempotencyKey(opts.idempotencyKey);
    if (idempotencyKey != null && idempotencyKey.length > 255) {
      throw new ConfigError(
        "artifact idempotencyKey must be at most 255 characters",
      );
    }
    const name = opts.name.trim();
    if (!name) throw new ConfigError("artifact name is required");

    const mimeType = opts.mimeType?.trim();
    let file: Blob;
    if (opts.file instanceof Blob) {
      file =
        mimeType && mimeType !== opts.file.type
          ? new Blob([opts.file], { type: mimeType })
          : opts.file;
    } else {
      const bytes = Uint8Array.from(opts.file);
      file = new Blob([bytes.buffer], {
        type: mimeType || "application/octet-stream",
      });
    }

    const form = new FormData();
    form.append("name", name);
    form.append("mime", mimeType || file.type || "application/octet-stream");
    form.append("size_bytes", String(file.size));
    if (opts.metadata != null) {
      form.append("metadata", JSON.stringify(opts.metadata));
    }
    const filename = name.split("/").pop() || "artifact";
    form.append("file", file, filename);

    const resp = await this.request("/v1/artifacts", {
      method: "POST",
      formData: form,
      idempotencyKey,
      signal: opts.signal,
    });
    return (await resp.json()) as Artifact;
  }

  /** Read one page of exact, org-scoped billing usage evidence. */
  async listBillingUsageEvents(
    opts: ListBillingUsageEventsOptions = {},
  ): Promise<BillingUsageEventListResponse> {
    const path = withQuery("/v1/billing/usage-events", {
      recorded_after: opts.recordedAfter,
      period_start: opts.periodStart,
      counter: opts.counter,
      source_type: opts.sourceType,
      source_id: opts.sourceId,
      job_id: opts.jobId,
      api_key_id: opts.apiKeyId,
      cursor: opts.cursor,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as BillingUsageEventListResponse;
  }

  /**
   * Drain every usage page for these filters in server order, yielding one
   * event at a time. Pass `recordedAfter` to select the durable oldest-first
   * incremental contract; a single call yields each matching event exactly
   * once. Deduplication is the caller's job only *across* calls: when you
   * advance `recordedAfter` and replay a bounded overlap, skip events whose
   * `id` you have already seen — this iterator does not remember events from
   * earlier calls.
   */
  async *iterateBillingUsageEvents(
    opts: Omit<ListBillingUsageEventsOptions, "cursor">,
  ): AsyncGenerator<BillingUsageEvent> {
    let cursor: string | undefined;
    do {
      const page = await this.listBillingUsageEvents({ ...opts, cursor });
      for (const item of page.items) yield item;
      if (!page.has_more) return;
      if (!page.next_cursor) {
        throw new ConfigError(
          "billing usage response has_more without next_cursor",
        );
      }
      cursor = page.next_cursor;
    } while (cursor);
  }

  async applyBlueprint(
    input: ApplyBlueprintRequest,
  ): Promise<BlueprintApplyResult> {
    const resp = await this.request("/v1/blueprints/apply", {
      method: "POST",
      body: input,
    });
    return (await resp.json()) as BlueprintApplyResult;
  }

  async listBlueprintBindings(
    opts: ListBlueprintBindingsOptions = {},
  ): Promise<BlueprintBindingListResponse> {
    const path = withQuery("/v1/blueprints/bindings", {
      namespace: opts.namespace,
      blueprint_key: opts.blueprintKey,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as BlueprintBindingListResponse;
  }

  async setBlueprintProtection(
    blueprintKey: string,
    protectedResources: boolean,
    opts: SetBlueprintProtectionOptions = {},
  ): Promise<BlueprintBindingListResponse> {
    const path = withQuery(
      `/v1/blueprints/${encodeURIComponent(blueprintKey)}/protection`,
      { namespace: opts.namespace },
    );
    const resp = await this.request(path, {
      method: "PUT",
      body: { protected: protectedResources },
    });
    return (await resp.json()) as BlueprintBindingListResponse;
  }

  async deleteBlueprint(
    blueprintKey: string,
    opts: DeleteBlueprintOptions = {},
  ): Promise<BlueprintDeleteResult> {
    const path = withQuery(
      `/v1/blueprints/${encodeURIComponent(blueprintKey)}`,
      {
        namespace: opts.namespace,
        delete_retained: opts.deleteRetained,
      },
    );
    const resp = await this.request(path, { method: "DELETE" });
    return (await resp.json()) as BlueprintDeleteResult;
  }

  async listInteractions(
    opts: ListInteractionsOptions = {},
  ): Promise<InteractionListResponse> {
    const path = withQuery("/v1/interactions", {
      status: opts.status,
      kind: opts.kind,
      session_id: opts.sessionId,
      target_user_id: opts.targetUserId,
      inbox: opts.inbox,
      cursor: opts.cursor,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as InteractionListResponse;
  }

  async listOrgPermissions(): Promise<PermissionCatalogResponse> {
    const resp = await this.request("/v1/permissions", {
      method: "GET",
    });
    return (await resp.json()) as PermissionCatalogResponse;
  }

  async listPrincipals(
    opts: ListPrincipalsOptions = {},
  ): Promise<PrincipalListResponse> {
    const path = withQuery("/v1/principals", {
      kind: opts.kind,
      include_disabled: opts.includeDisabled,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as PrincipalListResponse;
  }

  async createPrincipal(input: CreatePrincipalRequest): Promise<Principal> {
    const resp = await this.request("/v1/principals", {
      method: "POST",
      body: input,
    });
    return (await resp.json()) as Principal;
  }

  async getPrincipal(id: string): Promise<Principal> {
    const resp = await this.request(
      `/v1/principals/${encodeURIComponent(id)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Principal;
  }

  async updatePrincipal(
    id: string,
    input: UpdatePrincipalRequest,
  ): Promise<Principal> {
    const resp = await this.request(
      `/v1/principals/${encodeURIComponent(id)}`,
      { method: "PATCH", body: input },
    );
    return (await resp.json()) as Principal;
  }

  async deletePrincipal(id: string): Promise<void> {
    await this.request(
      `/v1/principals/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
  }

  async listRoles(opts: ListRolesOptions = {}): Promise<RoleListResponse> {
    const path = withQuery("/v1/roles", opts);
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as RoleListResponse;
  }

  async createRole(input: CreateRoleRequest): Promise<Role> {
    const resp = await this.request("/v1/roles", {
      method: "POST",
      body: input,
    });
    return (await resp.json()) as Role;
  }

  async getRole(id: string): Promise<Role> {
    const resp = await this.request(
      `/v1/roles/${encodeURIComponent(id)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Role;
  }

  async updateRole(id: string, input: UpdateRoleRequest): Promise<Role> {
    const resp = await this.request(
      `/v1/roles/${encodeURIComponent(id)}`,
      { method: "PATCH", body: input },
    );
    return (await resp.json()) as Role;
  }

  async deleteRole(id: string): Promise<void> {
    await this.request(
      `/v1/roles/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
  }

  async listRoleAssignments(
    opts: ListRoleAssignmentsOptions = {},
  ): Promise<RoleAssignmentListResponse> {
    const path = withQuery("/v1/role-assignments", {
      principal_id: opts.principalId,
      role_id: opts.roleId,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as RoleAssignmentListResponse;
  }

  async createRoleAssignment(
    input: CreateRoleAssignmentRequest,
  ): Promise<RoleAssignment> {
    const resp = await this.request(
      "/v1/role-assignments",
      { method: "POST", body: input },
    );
    return (await resp.json()) as RoleAssignment;
  }

  async deleteRoleAssignment(id: string): Promise<void> {
    await this.request(
      `/v1/role-assignments/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
  }

  /**
   * Lists org-owned, organization-shared, and system skills. System
   * skill templates are included by default; pass includeSystem: false to
   * omit them. Organization skills are read-only through these mutation
   * routes.
   */
  async listSkills(opts: ListSkillsOptions = {}): Promise<SkillListResponse> {
    const path = withQuery("/v1/skills", {
      include_system: opts.includeSystem,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SkillListResponse;
  }

  /** Creates an org-owned skill. */
  async createSkill(input: SkillRequest): Promise<Skill> {
    const resp = await this.request("/v1/skills", {
      method: "POST",
      body: input,
    });
    return (await resp.json()) as Skill;
  }

  /**
   * Imports a Claude Code or Dive-style skill document into an org-owned
   * skill. The content string is sent verbatim, including any YAML
   * frontmatter; pass opts.name to override the document's own.
   */
  async importSkill(
    content: string,
    opts: ImportSkillOptions = {},
  ): Promise<Skill> {
    const body: ImportSkillRequest = { content };
    if (opts.name != null) body.name = opts.name;
    const resp = await this.request("/v1/skills/import", {
      method: "POST",
      body,
    });
    return (await resp.json()) as Skill;
  }

  /**
   * Returns a single org-owned, organization-shared, or system skill by
   * ID.
   */
  async getSkill(skillId: string): Promise<Skill> {
    const resp = await this.request(
      `/v1/skills/${encodeURIComponent(skillId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Skill;
  }

  /**
   * Replaces an org-owned skill. The server requires the full body —
   * including name and instructions — on every update; there is no partial
   * patch, so read-modify-write is the caller's responsibility.
   */
  async updateSkill(skillId: string, input: SkillRequest): Promise<Skill> {
    const resp = await this.request(
      `/v1/skills/${encodeURIComponent(skillId)}`,
      { method: "PUT", body: input },
    );
    return (await resp.json()) as Skill;
  }

  /**
   * Deletes an org-owned skill. The skill is automatically detached from
   * any agents that reference it.
   */
  async deleteSkill(skillId: string): Promise<void> {
    await this.request(
      `/v1/skills/${encodeURIComponent(skillId)}`,
      { method: "DELETE" },
    );
  }

  /** Returns the skills assigned to an agent in assignment order. */
  async listAgentSkillAssignments(
    agentId: string,
  ): Promise<SkillAssignmentListResponse> {
    const resp = await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}/skill-assignments`,
      { method: "GET" },
    );
    return (await resp.json()) as SkillAssignmentListResponse;
  }

  /**
   * Replaces the agent's skill assignment set as a whole with skillIds, in
   * the given order. Pass an empty array to remove every assignment.
   */
  async replaceAgentSkillAssignments(
    agentId: string,
    skillIds: string[],
  ): Promise<SkillAssignmentListResponse> {
    const body: ReplaceSkillsRequest = { skill_ids: skillIds };
    const resp = await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}/skill-assignments`,
      { method: "PUT", body },
    );
    return (await resp.json()) as SkillAssignmentListResponse;
  }

  /**
   * Lists recent action invocation audit records from loops, agents, direct
   * invocations, and job-backed execution. Each entry carries definition
   * provenance (action_id, definition_scope) and, for signed HTTP
   * deliveries, the delivery identity (delivery_id, correlation_id,
   * secret_version).
   */
  async listActionInvocations(
    opts: ListActionInvocationsOptions = {},
  ): Promise<ActionInvocationListResponse> {
    const path = withQuery("/v1/action-invocations", {
      job_id: opts.jobId,
      environment_id: opts.environmentId,
      action_name: opts.actionName,
      action_id: opts.actionId,
      definition_scope: opts.definitionScope,
      secret_version: opts.secretVersion,
      delivery_id: opts.deliveryId,
      correlation_id: opts.correlationId,
      status: opts.status,
      cursor: opts.cursor,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as ActionInvocationListResponse;
  }

  async listSessions(
    opts: ListSessionsOptions = {},
  ): Promise<SessionListResponse> {
    const path = withQuery("/v1/sessions", {
      agent_id: opts.agentId,
      agent_name: opts.agentName,
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

  async getSessionByKey(
    sessionKey: string,
    agent: { agentId?: string; agentName?: string },
  ): Promise<Session | null> {
    if (!sessionKey) throw new ConfigError("sessionKey is required");
    if (!!agent.agentId === !!agent.agentName) {
      throw new ConfigError("exactly one of agentId or agentName is required");
    }
    const page = await this.listSessions({
      agentId: agent.agentId,
      agentName: agent.agentName,
      sessionKey,
      limit: 1,
    });
    return page.items[0] ?? null;
  }

  async listAgents(opts: ListAgentsOptions = {}): Promise<AgentListResponse> {
    const path = withQuery("/v1/agents", {
      name: opts.name,
      principal_id: opts.principalId,
      status: opts.status,
      limit: opts.limit,
    });
    const resp = await this.request(path, { method: "GET" });
    const page = (await resp.json()) as AgentListResponse;
    for (const agent of page.items) this.agentNameCache.set(agent.name, agent);
    return page;
  }

  async getAgent(agentId: string): Promise<Agent> {
    const resp = await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}`,
      { method: "GET" },
    );
    const agent = (await resp.json()) as Agent;
    this.agentNameCache.set(agent.name, agent);
    return agent;
  }

  /**
   * Creates an agent, or — with `adoptExisting` — adopts the live agent
   * that already carries the same `externalRef`. On adopt the existing
   * agent is returned unchanged (HTTP 200) and mutable fields in `input`
   * are ignored, since no write happens.
   */
  async createAgent(
    input: CreateAgentRequest,
    opts: CreateAgentOptions = {},
  ): Promise<Agent> {
    const body: CreateAgentRequest = { ...input };
    if (opts.externalRef != null) body.external_ref = opts.externalRef;
    if (opts.adoptExisting) {
      if (!body.external_ref) {
        throw new ConfigError(
          "createAgent: adoptExisting requires externalRef to be set",
        );
      }
      body.if_exists = "adopt";
    }
    const resp = await this.request("/v1/agents", {
      method: "POST",
      body,
      replaySafe: opts.adoptExisting === true,
    });
    const agent = (await resp.json()) as Agent;
    this.agentNameCache.set(agent.name, agent);
    return agent;
  }

  /**
   * Returns the organization's allowlist of exact HTTPS origins an embedded
   * partner may name as an OAuth connect `return_url`. An empty list means
   * embedded return is disabled. Requires Admin or Owner membership.
   */
  async getOAuthReturnOrigins(): Promise<OAuthReturnOrigins> {
    const resp = await this.request("/v1/organization/oauth-return-origins", {
      method: "GET",
    });
    return (await resp.json()) as OAuthReturnOrigins;
  }

  /**
   * Full-replaces the organization's OAuth return-origin allowlist. The
   * server normalizes each entry (lowercase host, default ports stripped)
   * and rejects invalid origins; no validation happens client-side. Pass an
   * empty array to disable embedded return. Requires Admin or Owner
   * membership.
   */
  async replaceOAuthReturnOrigins(
    origins: string[],
  ): Promise<OAuthReturnOrigins> {
    const body: PutOAuthReturnOriginsRequest = { origins };
    const resp = await this.request("/v1/organization/oauth-return-origins", {
      method: "PUT",
      body,
    });
    return (await resp.json()) as OAuthReturnOrigins;
  }

  async resolveAgent(name: string): Promise<Agent> {
    const cached = this.agentNameCache.get(name);
    if (cached) return cached;
    const page = await this.listAgents({ name, limit: 1 });
    const agent = page.items[0];
    if (!agent) {
      throw new MobiusAPIError({
        status: 404,
        code: "not_found",
        message: `agent ${JSON.stringify(name)} not found`,
      });
    }
    return agent;
  }

  /** Returns a summary of an agent's private memory. */
  async getAgentMemory(agentId: string): Promise<AgentMemory> {
    const resp = await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}/memory`,
      { method: "GET" },
    );
    return (await resp.json()) as AgentMemory;
  }

  /**
   * Lists or searches an agent's memory entries. The response preserves
   * search_coverage so callers can see when semantic or hybrid results ranked
   * only a partially indexed subset.
   */
  async listAgentMemoryEntries(
    agentId: string,
    opts: ListAgentMemoryEntriesOptions = {},
  ): Promise<AgentMemoryEntryListResponse> {
    const path = withQuery(
      `/v1/agents/${encodeURIComponent(agentId)}/memory/entries`,
      {
        query: opts.query,
        search_mode: opts.searchMode,
        kind: opts.kind,
        cursor: opts.cursor,
        limit: opts.limit,
      },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as AgentMemoryEntryListResponse;
  }

  /** Creates or updates the memory entry stored under key. */
  async saveAgentMemoryEntry(
    agentId: string,
    key: string,
    input: SaveAgentMemoryEntryRequest,
  ): Promise<AgentMemoryEntry> {
    const resp = await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}/memory/entries/${encodeURIComponent(key)}`,
      { method: "PUT", body: input },
    );
    return (await resp.json()) as AgentMemoryEntry;
  }

  /** Deletes the memory entry stored under key. */
  async deleteAgentMemoryEntry(agentId: string, key: string): Promise<void> {
    await this.request(
      `/v1/agents/${encodeURIComponent(agentId)}/memory/entries/${encodeURIComponent(key)}`,
      { method: "DELETE" },
    );
  }

  /**
   * Returns one page of the content-free, append-only memory change feed. A
   * 410 {@link MobiusAPIError} means the cursor predates retained history;
   * recover with {@link syncAgentMemory} or by relisting entries.
   */
  async listAgentMemoryChanges(
    agentId: string,
    opts: ListAgentMemoryChangesOptions = {},
  ): Promise<AgentMemoryChangeListResponse> {
    const path = withQuery(
      `/v1/agents/${encodeURIComponent(agentId)}/memory/changes`,
      { after: opts.after, limit: opts.limit },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as AgentMemoryChangeListResponse;
  }

  /**
   * Advances a memory change-feed consumer by one bounded synchronization
   * step: drains every change page after cursor and returns the new feed
   * position to persist. When the cursor has expired (HTTP 410) it recovers
   * explicitly — establishing a fresh feed position and returning a full
   * entry snapshot with reset set — instead of failing or silently replaying.
   * Omit cursor on first use. Polling cadence and retry policy stay with the
   * caller; this makes no timing decisions.
   */
  async syncAgentMemory(
    agentId: string,
    cursor?: string,
  ): Promise<MemorySyncResult> {
    try {
      const { changes, nextCursor } = await this.drainAgentMemoryChanges(
        agentId,
        cursor,
      );
      return { reset: false, changes, nextCursor };
    } catch (err) {
      if (!(err instanceof MobiusAPIError) || err.status !== 410) throw err;
    }
    // The cursor predates retained history. Take the fresh feed position
    // BEFORE the snapshot: a mutation racing the snapshot then replays after
    // the new cursor (versions make replays detectable) instead of being lost.
    const { nextCursor } = await this.drainAgentMemoryChanges(
      agentId,
      undefined,
    );
    const entries = await this.drainAgentMemoryEntries(agentId);
    return { reset: true, entries, nextCursor };
  }

  private async drainAgentMemoryChanges(
    agentId: string,
    after: string | undefined,
  ): Promise<{ changes: AgentMemoryChange[]; nextCursor: string }> {
    const changes: AgentMemoryChange[] = [];
    let cursor = after;
    for (;;) {
      const page = await this.listAgentMemoryChanges(agentId, {
        after: cursor,
      });
      changes.push(...page.items);
      cursor = page.next_cursor;
      if (!page.has_more) return { changes, nextCursor: cursor };
    }
  }

  private async drainAgentMemoryEntries(
    agentId: string,
  ): Promise<AgentMemoryEntry[]> {
    const entries: AgentMemoryEntry[] = [];
    let cursor: string | undefined;
    for (;;) {
      const page = await this.listAgentMemoryEntries(agentId, { cursor });
      entries.push(...page.items);
      if (!page.has_more || !page.next_cursor) return entries;
      cursor = page.next_cursor;
    }
  }

  async getSession(sessionId: string): Promise<Session> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as Session;
  }

  async cancelSession(
    sessionId: string,
    opts: { force?: boolean } = {},
  ): Promise<Session> {
    const path = withQuery(
      `/v1/sessions/${encodeURIComponent(sessionId)}/cancel`,
      { force: opts.force },
    );
    const resp = await this.request(path, { method: "POST" });
    return (await resp.json()) as Session;
  }

  async compactSession(sessionId: string): Promise<Session> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/compact`,
      { method: "POST" },
    );
    return (await resp.json()) as Session;
  }

  async listSessionMessages(
    sessionId: string,
    opts: ListSessionMessagesOptions = {},
  ): Promise<SessionMessageListResponse> {
    const path = withQuery(
      `/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
      {
        after_sequence: opts.afterSequence,
        before_sequence: opts.beforeSequence,
        order: opts.order,
        limit: opts.limit,
        include: opts.include,
      },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SessionMessageListResponse;
  }

  async nudgeSession(
    sessionId: string,
    opts: NudgeSessionOptions,
  ): Promise<SessionNudgeAck> {
    const body = removeUndefined({
      content: opts.content,
      idempotency_key: normalizeIdempotencyKey(opts.idempotencyKey),
      metadata: opts.metadata,
      wake: opts.wake,
    });
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/nudges`,
      {
        method: "POST",
        body,
        idempotencyKey: body.idempotency_key,
      },
    );
    return (await resp.json()) as SessionNudgeAck;
  }

  async listNudges(
    sessionId: string,
    opts: ListSessionNudgesOptions = {},
  ): Promise<SessionNudgeListResponse> {
    const path = withQuery(
      `/v1/sessions/${encodeURIComponent(sessionId)}/nudges`,
      {
        status: opts.status,
        order: opts.order,
        cursor: opts.cursor,
        limit: opts.limit,
      },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as SessionNudgeListResponse;
  }

  async getNudge(sessionId: string, nudgeId: string): Promise<SessionNudge> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/nudges/${encodeURIComponent(nudgeId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as SessionNudge;
  }

  async cancelNudge(sessionId: string, nudgeId: string): Promise<SessionNudge> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/nudges/${encodeURIComponent(nudgeId)}/cancel`,
      { method: "POST" },
    );
    return (await resp.json()) as SessionNudge;
  }

  /** Submit a response to an interaction observed on a session transcript. */
  async respondToInteraction(
    interactionId: string,
    input: RespondToInteractionRequest,
  ): Promise<Interaction> {
    const resp = await this.request(
      `/v1/interactions/${encodeURIComponent(interactionId)}/respond`,
      { method: "POST", body: input },
    );
    return (await resp.json()) as Interaction;
  }

  async listTurns(
    sessionId: string,
    opts: ListSessionTurnsOptions = {},
  ): Promise<AgentTurnListResponse> {
    const path = withQuery(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns`,
      {
        ids: opts.ids,
        order: opts.order,
        cursor: opts.cursor,
        limit: opts.limit,
      },
    );
    const resp = await this.request(path, { method: "GET" });
    return (await resp.json()) as AgentTurnListResponse;
  }

  async getTurn(sessionId: string, turnId: string): Promise<AgentTurn> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/${encodeURIComponent(turnId)}`,
      { method: "GET" },
    );
    return (await resp.json()) as AgentTurn;
  }

  async cancelTurn(sessionId: string, turnId: string): Promise<AgentTurn> {
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/${encodeURIComponent(turnId)}/cancel`,
      { method: "POST" },
    );
    return (await resp.json()) as AgentTurn;
  }

  /** Start a turn in an existing session and return its lazy transcript handle. */
  async startTurn(
    sessionId: string,
    opts: StartTurnOptions,
    streamOpts: { signal?: AbortSignal } = {},
  ): Promise<TurnTranscript> {
    if (!opts.content || opts.content.length === 0) {
      throw new Error("mobius: start turn: content is required");
    }
    const body: StartTurnRequest = removeUndefined({
      role: "user",
      content: opts.content,
      context: opts.context,
      idempotency_key: normalizeIdempotencyKey(opts.idempotencyKey),
      operation: opts.operation,
      output: opts.output,
      metadata: opts.metadata,
    });
    const resp = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns`,
      {
        method: "POST",
        body,
        idempotencyKey: body.idempotency_key,
        signal: streamOpts.signal,
      },
    );
    const ack = (await resp.json()) as TurnAck;
    const transcript = new SessionTranscript();
    transcript.seed(ack);
    return new TurnTranscript(this, ack, transcript, streamOpts.signal);
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
    const resp = await this.request("/v1/agents/invoke", {
      method: "POST",
      body,
      idempotencyKey: invokeAgentReplayKey(body),
      signal: streamOpts.signal,
    });
    const ack = (await resp.json()) as TurnAck;
    if (!ack.resume_cursor?.trim()) {
      throw new Error("mobius: invoke agent response missing resume_cursor");
    }
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
    const path = "/v1/agents/invoke";
    const body = invokeAgentRequest(opts);
    const resp = await this.fetchFn(this.url(path), {
      method: "POST",
      headers: this.requestHeaders(
        invokeAgentReplayKey(body),
        "text/event-stream",
      ),
      body: JSON.stringify(body),
      signal: streamOpts.signal,
    });
    if (!resp.ok) {
      if (resp.status === 401) throw new AuthRevokedError();
      throw await responseError(resp, "POST", path, true);
    }
    if (!resp.body) {
      throw new Error(
        "mobius API POST agents/invoke: response body is not readable",
      );
    }
    for await (const evt of parseSSE(resp.body)) {
      if (!evt.event || !evt.data) continue;
      yield {
        eventType: evt.event,
        data: JSON.parse(evt.data) as SessionStreamFrame,
      };
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
      `/v1/sessions/${encodeURIComponent(sessionId)}/transcript`,
      { cursor: opts.cursor, page_token: opts.pageToken, limit: opts.limit },
    );
    const resp = await this.request(path, {
      method: "GET",
      signal: opts.signal,
    });
    const snapshot = (await resp.json()) as SessionTranscriptSnapshot;
    // Preserve rolling-deploy compatibility when an older server omits the
    // newly-added pending-interactions projection.
    snapshot.interactions ??= [];
    return snapshot;
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
      `/v1/sessions/${encodeURIComponent(sessionId)}/transcript/stream`,
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
    for await (const update of this.watchSessionTranscriptUpdates(
      sessionId,
      opts,
    )) {
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
      this.log({
        type: "stream",
        event: reconnectCount ? "reconnect" : "open",
        path: sessionId,
      });
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
              connection = opts.follow ? "reconnecting" : "ended";
              rotate = false;
            } else {
              connection = "reconnecting";
              rotate = true;
            }
          }
          this.log({
            type: "stream",
            event:
              eventType === "stream.ready"
                ? "ready"
                : eventType === "stream.end"
                  ? connection === "ended"
                    ? "idle"
                    : "rotate"
                  : "frame",
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

  private async request(
    path: string,
    opts: {
      method: string;
      body?: unknown;
      formData?: FormData;
      idempotencyKey?: string | undefined;
      /**
       * Marks a POST/PATCH as safe to replay without an Idempotency-Key
       * header (adopt-mode creates, whose idempotency comes from
       * external_ref). Internal — set only by curated methods.
       */
      replaySafe?: boolean | undefined;
      signal?: AbortSignal | undefined;
    },
  ): Promise<Response> {
    const started = Date.now();
    const timeout = AbortSignal.timeout(this.timeoutMs);
    const signal = opts.signal ? anySignal(opts.signal, timeout) : timeout;
    const headers = this.requestHeaders(opts.idempotencyKey);
    if (opts.formData != null) headers.delete("Content-Type");
    const init: ReplaySafeRequestInit = {
      method: opts.method,
      headers,
      signal,
    };
    if (opts.replaySafe) init.replaySafe = true;
    if (opts.formData != null) init.body = opts.formData;
    else if (opts.body != null) init.body = JSON.stringify(opts.body);
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

  private requestHeaders(
    idempotencyKey?: string,
    accept?: string,
  ): Headers {
    const headers = new Headers(this.headers);
    const normalizedKey = normalizeIdempotencyKey(idempotencyKey);
    if (normalizedKey) headers.set("Idempotency-Key", normalizedKey);
    if (accept) headers.set("Accept", accept);
    return headers;
  }

  private log(event: ClientLogEvent): void {
    try {
      this.logger?.(event);
    } catch {
      // Diagnostics must never change request behavior.
    }
  }

  private url(path: string): string {
    return this.baseURL + path;
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
  /** True when a repeated idempotency key returned an existing turn without restarting it. */
  readonly deduped: boolean;
  /** Full session view the stream folds into. */
  readonly transcript: SessionTranscript;

  readonly #client: Client;
  readonly #signal?: AbortSignal;
  // Immutable invocation boundary used for initial replay and terminal
  // settlement. The transcript cursor keeps moving for stream reconnects.
  readonly #invocationCursor: string;
  // Set when deduplication returned an already-terminal turn: there is
  // nothing to stream, so iteration fetches the
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
    this.#invocationCursor = ack.resume_cursor;
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

  /**
   * The turn's validated structured output, present only on a completed turn
   * that declared an output contract (see {@link InvokeAgentOptions.output}).
   * `undefined` until the terminal `turn.upsert` frame is applied. Read this
   * instead of parsing the transcript messages.
   */
  get output(): Record<string, unknown> | undefined {
    return this.transcript.turn(this.id)?.output ?? undefined;
  }

  /**
   * Where {@link output} came from: `"tool"` when the agent submitted it
   * through the reserved `mobius_submit_output` tool, or `"text"` when Mobius
   * accepted a schema-valid final message as a fallback. `undefined` when the
   * turn produced no structured output.
   */
  get outputSource(): AgentTurnOutputSource | undefined {
    return this.transcript.turn(this.id)?.output_source ?? undefined;
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
    return {
      ...this.#diagnostics,
      status: this.status,
      cursor: this.transcript.cursor,
      ready: this.transcript.ready,
    };
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
        await this.#reconcileSnapshot(this.#invocationCursor);
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
    await this.#reconcileSnapshot(this.#invocationCursor);
    this.#diagnostics = {
      status: this.status,
      cursor: this.transcript.cursor,
      ready: this.transcript.ready,
      reconnectCount: 0,
      connection: "ended",
    };
  }

  async #reconcileSnapshot(cursor: string): Promise<void> {
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
      context: opts.context,
      idempotency_key: normalizeIdempotencyKey(opts.idempotencyKey),
      metadata: opts.inputMetadata,
    }) as InvokeInput,
    session: opts.session,
    operation: opts.operation,
    output: opts.output,
    channel_context: opts.channelContext,
  }) as InvokeAgentRequest;
}

function invokeAgentReplayKey(body: InvokeAgentRequest): string | undefined {
  if (body.session?.mode === "new") return undefined;
  return normalizeIdempotencyKey(body.input.idempotency_key);
}

function normalizeIdempotencyKey(value?: string): string | undefined {
  const normalized = value?.trim();
  return normalized ? normalized : undefined;
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
  const retryAfter = parseRetryAfterSeconds(
    response.headers.get("Retry-After"),
  );
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

async function* parseSSE(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SSEEvent> {
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
