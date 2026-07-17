from __future__ import annotations

import base64
import json
import logging
import os
import re
import time
from dataclasses import dataclass, field
from collections.abc import Iterator
from typing import Any, BinaryIO
from urllib.parse import quote, urlencode, urlparse, urlunparse

import httpx

from ._api.models import (
    AgentMemory,
    AgentMemoryChange,
    AgentMemoryChangeListResponse,
    AgentMemoryEntry,
    AgentMemoryEntryListResponse,
    AgentTurn,
    AgentTurnListResponse,
    AgentTurnOperationPolicy,
    AgentRef,
    ApplyBlueprintRequest,
    Artifact,
    BlueprintApplyResult,
    BlueprintBindingListResponse,
    BlueprintDeleteResult,
    ActivateOrganizationActionSecretRequest,
    CancelLoopRunRequest,
    ChannelContext,
    CreateOrganizationActionRequest,
    CreatePrincipalRequest,
    CreateRoleAssignmentRequest,
    CreateRoleRequest,
    CreateLoopRequest,
    InlineAgentConfig,
    InteractionKind,
    InteractionListResponse,
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
    MemoryKind,
    MemorySearchMode,
    NudgeSessionRequest,
    OrganizationAction,
    OrganizationActionListResponse,
    PermissionCatalogResponse,
    Principal,
    PrincipalKind,
    PrincipalListResponse,
    RuntimeContext,
    RuntimeContextItem,
    SaveAgentMemoryEntryRequest,
    Role1 as ProjectRole,
    RoleAssignment,
    RoleAssignmentListResponse,
    RoleListResponse,
    Session,
    SessionListResponse,
    SessionMessageListResponse,
    SessionNudge,
    SessionNudgeAck,
    SessionNudgeListResponse,
    SessionTranscriptSnapshot,
    SetBlueprintProtectionRequest,
    SignalLoopRunRequest,
    StartLoopRunRequest,
    StartTurnRequest,
    TagMap,
    TurnAck,
    TurnOutputSpec,
    UpdateLoopRequest,
    UpdateOrganizationActionRequest,
    UpdatePrincipalRequest,
    UpdateRoleRequest,
)
from .errors import AuthRevokedError, MobiusAPIError, RateLimitError
from .retry import DEFAULT_MAX_RETRIES, RetryingTransport
from .transcript import (
    GetSessionTranscriptOptions,
    SessionTranscript,
    StreamSessionTranscriptOptions,
    TranscriptStreamEvent,
    WatchSessionTranscriptOptions,
    is_terminal_turn_status,
)

DEFAULT_BASE_URL = "https://api.mobiusops.ai"
_HANDLE_RE = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


@dataclass
class ClientOptions:
    api_key: str
    base_url: str = DEFAULT_BASE_URL
    project: str = "default"
    namespace: str | None = None
    timeout: float = 60.0
    retry: int = DEFAULT_MAX_RETRIES
    logger: logging.Logger | None = None

    def __post_init__(self) -> None:
        if self.namespace and self.project == "default":
            self.project = self.namespace
        self.namespace = self.project


@dataclass
class LoopOptions:
    name: str
    description: str | None = None
    agent_id: str | None = None
    default_config: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    tags: TagMap | dict[str, str] | None = None
    # Authoring definition for the loop. Recognised keys are schema_version,
    # steps, event, config, triggers, defaults, limits, output, repositories,
    # cleanup, .... When it carries steps the loop is runnable immediately.
    # Keys are merged into the create request; explicit fields above take
    # precedence.
    spec: dict[str, Any] | None = None


@dataclass
class UpdateLoopOptions:
    name: str | None = None
    description: str | None = None
    agent_id: str | None = None
    default_config: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    status: LoopStatus | None = None
    tags: TagMap | dict[str, str] | None = None
    # Replacement authoring definition. See LoopOptions.spec.
    spec: dict[str, Any] | None = None


@dataclass
class ListLoopsOptions:
    status: LoopStatus | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class StartRunOptions:
    # Exact event object that starts the run, reachable in templates at
    # ``event.*``. ``config`` holds optional static or caller-provided
    # configuration (``config.*``); ``meta`` holds optional caller-supplied
    # event metadata (Mobius adds its own provenance).
    event: dict[str, Any] | None = None
    config: dict[str, Any] | None = None
    meta: dict[str, Any] | None = None
    source: LoopRunSource | None = None
    idempotency_key: str | None = None
    # Deprecated: use idempotency_key.
    external_id: str | None = None


@dataclass
class ListRunsOptions:
    status: LoopRunStatus | None = None
    loop_id: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class WaitRunOptions:
    since: int = 0
    reconnect_delay: float = 1.0
    timeout: float | None = None


@dataclass
class RunEvent:
    id: str
    run_id: str
    event_type: str
    sequence: int
    payload: dict[str, Any] | None = None


@dataclass
class InvokeAgentOptions:
    # Ordered content blocks (text, images, ...) for the caller's input
    # message. Required.
    content: list[dict[str, Any]]
    # Agent identifier. Mutually exclusive with agent_name.
    agent_id: str | None = None
    # Project-unique agent name. Mutually exclusive with agent_id.
    agent_name: str | None = None
    # Ordered application-owned state for this turn.
    context: list[RuntimeContextItem] | None = None
    # Dedup key scoped to the resolved session. A repeat call with the same
    # key resolves the same session and returns the existing invocation
    # without restarting it or starting a second one — derive it from the
    # provider event id for Slack/Telegram webhook retries. The SDK retries
    # admission automatically unless session.mode is "new", which cannot be
    # safely replayed with a session-scoped key.
    idempotency_key: str | None = None
    # Free-form caller metadata attached to the input message.
    input_metadata: dict[str, Any] | None = None
    # How to resolve or create the session this invocation runs in. Omit to
    # use a single default session per agent in continue_or_create mode. Set
    # session.thinking_effort to override the agent's reasoning-effort default
    # for this session.
    session: InvokeSessionSpec | None = None
    # Inline agent definition (instructions, model, effort, timeout, toolkits,
    # skills) sent with the invocation instead of using the agent stored in
    # Mobius. Set fields replace the agent's values; omitted fields keep them.
    # Mobius remembers the config on the session and reuses it on later turns
    # until a new one is sent. Omit to run the agent on its stored definition.
    config: InlineAgentConfig | None = None
    # Policy for only this newly admitted turn. Its timeout takes precedence
    # over the saved config timeout and is not saved on the session.
    operation: AgentTurnOperationPolicy | None = None
    # Structured-output contract for this turn. When set, Mobius exposes a
    # reserved submit tool for the schema, validates the submission
    # server-side, and fails the turn if it never produces a schema-valid
    # object. Read the validated value from TurnTranscript.output.
    output: TurnOutputSpec | None = None
    # Optional messaging provider/channel routing context (Slack, Telegram,
    # ...) recorded on the started turn.
    channel_context: ChannelContext | None = None


@dataclass
class StartTurnOptions:
    # Ordered content blocks (text, images, ...) for the caller's input
    # message. Required.
    content: list[dict[str, Any]]
    # Ordered application-owned state for this turn.
    context: list[RuntimeContextItem] | None = None
    # Dedup key scoped to the existing session. A repeat returns the existing
    # invocation, writes no new input, and never restarts a terminal turn.
    idempotency_key: str | None = None
    # Policy for only this newly admitted turn. Its timeout takes precedence
    # over the saved config timeout and is not saved on the session.
    operation: AgentTurnOperationPolicy | None = None
    # Structured-output contract for this turn. See InvokeAgentOptions.output;
    # read the validated value from TurnTranscript.output.
    output: TurnOutputSpec | None = None
    # Free-form caller metadata attached to the input message.
    metadata: dict[str, Any] | None = None


@dataclass
class ListAgentMemoryEntriesOptions:
    # Optional search over entry keys, kinds, summaries, and content; omit to
    # list.
    query: str | None = None
    # Search ranking mode for a non-blank query: keyword (the server
    # default), semantic, or hybrid. Semantic and hybrid raise a 503
    # memory_semantic_search_unavailable MobiusAPIError when the index is
    # unavailable; the SDK never downgrades to keyword silently because that
    # would change result semantics — retry or fall back explicitly.
    search_mode: MemorySearchMode | str | None = None
    # Optional filter to a single memory kind.
    kind: MemoryKind | str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class MemorySyncResult:
    """One bounded synchronization step of an agent memory change feed."""

    # True when the supplied cursor predated retained history (HTTP 410).
    # entries then carries a full current snapshot to replace local state,
    # and changes is empty.
    reset: bool
    # New feed position to persist for the next call.
    next_cursor: str
    # All feed items after the supplied cursor, drained across pages.
    changes: list[AgentMemoryChange] = field(default_factory=list)
    # Full current entry snapshot, present only when reset is True.
    entries: list[AgentMemoryEntry] = field(default_factory=list)


@dataclass
class ListOrganizationActionsOptions:
    cursor: str | None = None
    limit: int | None = None


@dataclass
class OrganizationActionSecretMaterial:
    """One-time signing secret revealed by create/rotate of an org action.

    The server never returns this key again; store ``key_bytes`` before
    discarding the value, and never log it.
    """

    # The created or updated action. Its signing_secret field is cleared;
    # the revealed key lives only in key_bytes.
    action: OrganizationAction
    # Stable reference sent in X-Mobius-Secret-Ref on signed deliveries.
    secret_ref: str
    # Key version the revealed secret belongs to: the active version after
    # create, the pending version after rotate.
    version: int
    # Base64-decoded signing key.
    key_bytes: bytes


@dataclass
class ListSessionsOptions:
    agent_id: str | None = None
    agent_name: str | None = None
    session_key: str | None = None
    status: str | None = None
    scope: str | None = None
    provider: str | None = None
    integration_id: str | None = None
    since: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class ListSessionMessagesOptions:
    after_sequence: int | None = None
    before_sequence: int | None = None
    order: str | None = None
    limit: int | None = None
    # Set to "context" to return caller-supplied runtime context rows.
    include: str | None = None


@dataclass
class NudgeSessionOptions:
    content: str
    idempotency_key: str | None = None
    metadata: dict[str, Any] | None = None
    wake: bool = False


@dataclass
class ListSessionNudgesOptions:
    status: list[str] | None = None
    order: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class ListBlueprintBindingsOptions:
    namespace: str | None = None
    blueprint_key: str | None = None


@dataclass
class DeleteBlueprintOptions:
    namespace: str | None = None
    delete_retained: bool = False


@dataclass
class SetBlueprintProtectionOptions:
    namespace: str | None = None


@dataclass
class ListInteractionsOptions:
    status: str | None = None
    kind: InteractionKind | None = None
    run_id: str | None = None
    session_id: str | None = None
    target_user_id: str | None = None
    inbox: bool | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class ListPrincipalsOptions:
    kind: PrincipalKind | None = None
    include_disabled: bool | None = None
    limit: int | None = None


@dataclass
class ListRoleAssignmentsOptions:
    principal_id: str | None = None
    role_id: str | None = None


@dataclass
class ListRolesOptions:
    cursor: str | None = None
    limit: int | None = None


@dataclass
class ListSessionTurnsOptions:
    ids: list[str] | None = None
    order: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class SessionStreamEvent:
    # event_type is the authoritative SSE `event:` name (e.g.
    # "turn.completed"); validate data with the payload model matching it —
    # SessionStreamFrame is a reference-only union, ambiguous from data alone.
    event_type: str
    data: dict[str, Any]


@dataclass(frozen=True)
class TranscriptUpdate:
    frame: dict[str, Any]
    cursor: str | None
    transcript: SessionTranscript
    connection: str
    reconnect_count: int


@dataclass(frozen=True)
class TranscriptDiagnostics:
    status: str
    cursor: str | None
    ready: bool
    reconnect_count: int
    last_frame_type: str | None
    last_frame_at: float | None
    connection: str


class LeaseLostError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"lease lost for job {job_id}")
        self.job_id = job_id


class PayloadTooLargeError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"custom event payload too large for job {job_id}")
        self.job_id = job_id


class RateLimitedError(RateLimitError):
    def __init__(self, job_id: str, retry_after: int | None = None) -> None:
        msg = f"custom event rate limited for job {job_id}"
        if retry_after:
            msg += f" (retry after {retry_after}s)"
        super().__init__(retry_after=float(retry_after or 0), message=msg)
        self.job_id = job_id
        self.retry_after = retry_after


class Client:
    """Mobius public API client for loops, runs, and workers."""

    def __init__(
        self,
        opts: ClientOptions | str,
        *,
        base_url: str | None = None,
        project: str | None = None,
        namespace: str | None = None,
        timeout: float = 60.0,
        retry: int = DEFAULT_MAX_RETRIES,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        if isinstance(opts, str):
            opts = ClientOptions(
                api_key=opts,
                base_url=base_url or DEFAULT_BASE_URL,
                project=project or namespace or "default",
                timeout=timeout,
                retry=retry,
            )
        elif base_url is not None:
            opts.base_url = base_url
        handle = _extract_handle_from_api_key(opts.api_key)
        explicit = opts.project or opts.namespace
        if handle:
            if explicit and explicit != "default" and explicit != handle:
                raise ValueError(
                    f"project={explicit!r} conflicts with the handle embedded in the API key ({handle!r})"
                )
            self.project = handle
        else:
            self.project = explicit or "default"
        self.base_url = opts.base_url.rstrip("/")
        self.api_key = opts.api_key
        self._logger = opts.logger or logging.getLogger("deepnoodle.mobius")
        base_transport = transport or httpx.HTTPTransport()
        # Content-Type is set per request by httpx (application/json for
        # json= bodies, multipart boundaries for files=). A client-level
        # default would override the multipart boundary and break uploads.
        self._client = httpx.Client(
            base_url=self.base_url,
            timeout=opts.timeout,
            transport=RetryingTransport(base_transport, max_retries=opts.retry),
            headers={"Authorization": f"Bearer {opts.api_key}"},
        )

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def worker_socket_url(self) -> str:
        parsed = urlparse(self.base_url)
        scheme = "wss" if parsed.scheme == "https" else "ws"
        base_path = parsed.path.rstrip("/")
        path = f"{base_path}/v1/projects/{quote(self.project, safe='')}/workers/socket"
        return urlunparse((scheme, parsed.netloc, path, "", "", ""))

    def create_artifact(
        self,
        file: bytes | BinaryIO | str | os.PathLike[str],
        *,
        name: str | None = None,
        mime: str | None = None,
        metadata: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> Artifact:
        """Publish a private artifact using the client's project authorization.

        ``file`` may be raw bytes, a binary file object, or a filesystem
        path. Path sources stream from disk and default ``name`` to the
        file's base name; bytes and file-object sources require ``name``.
        Ownership and visibility are derived from the authenticated
        principal; run/step lineage can only be attached by the server from
        a worker lease.
        """
        if idempotency_key is not None and len(idempotency_key) > 255:
            raise ValueError("artifact idempotency_key must be at most 255 characters")
        opened: BinaryIO | None = None
        try:
            source: bytes | BinaryIO
            size: int | None = None
            if isinstance(file, (str, os.PathLike)):
                opened = open(file, "rb")
                source = opened
                size = os.fstat(opened.fileno()).st_size
                if name is None:
                    name = os.path.basename(os.fspath(file))
            elif isinstance(file, (bytes, bytearray, memoryview)):
                source = bytes(file)
                size = len(source)
            else:
                source = file
            clean_name = (name or "").strip()
            if not clean_name:
                raise ValueError("artifact name is required")
            fields: dict[str, Any] = {"name": clean_name}
            if mime:
                fields["mime"] = mime
            if size is not None:
                fields["size_bytes"] = str(size)
            if metadata is not None:
                fields["metadata"] = json.dumps(metadata)
            filename = clean_name.rsplit("/", 1)[-1] or "artifact"
            resp = self._request(
                "POST",
                "/v1/projects/{project}/artifacts",
                files={"file": (filename, source, mime or "application/octet-stream")},
                data=fields,
                idempotency_key=idempotency_key,
            )
            return Artifact.model_validate(resp.json())
        finally:
            if opened is not None:
                opened.close()

    def list_loops(self, opts: ListLoopsOptions | None = None) -> LoopListResponse:
        resp = self._request("GET", "/v1/projects/{project}/loops", params=_params(opts))
        return LoopListResponse.model_validate(resp.json())

    def get_loop(self, loop_id: str) -> Loop:
        resp = self._request("GET", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}")
        return Loop.model_validate(resp.json())

    def create_loop(self, opts: LoopOptions) -> Loop:
        body = CreateLoopRequest(**_merge_loop_fields(opts))
        resp = self._request("POST", "/v1/projects/{project}/loops", json=body)
        return Loop.model_validate(resp.json())

    def update_loop(self, loop_id: str, opts: UpdateLoopOptions) -> Loop:
        body = UpdateLoopRequest(**_merge_loop_fields(opts))
        resp = self._request("PATCH", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}", json=body)
        return Loop.model_validate(resp.json())

    def delete_loop(self, loop_id: str) -> None:
        self._request("DELETE", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}")

    def apply_blueprint(self, request: ApplyBlueprintRequest) -> BlueprintApplyResult:
        resp = self._request(
            "POST", "/v1/projects/{project}/blueprints/apply", json=request
        )
        return BlueprintApplyResult.model_validate(resp.json())

    def list_blueprint_bindings(
        self, opts: ListBlueprintBindingsOptions | None = None
    ) -> BlueprintBindingListResponse:
        resp = self._request(
            "GET",
            "/v1/projects/{project}/blueprints/bindings",
            params=_params(opts),
        )
        return BlueprintBindingListResponse.model_validate(resp.json())

    def set_blueprint_protection(
        self,
        blueprint_key: str,
        protected: bool,
        opts: SetBlueprintProtectionOptions | None = None,
    ) -> BlueprintBindingListResponse:
        resp = self._request(
            "PUT",
            f"/v1/projects/{{project}}/blueprints/{quote(blueprint_key, safe='')}/protection",
            params=_params(opts),
            json=SetBlueprintProtectionRequest(protected=protected),
        )
        return BlueprintBindingListResponse.model_validate(resp.json())

    def delete_blueprint(
        self, blueprint_key: str, opts: DeleteBlueprintOptions | None = None
    ) -> BlueprintDeleteResult:
        resp = self._request(
            "DELETE",
            f"/v1/projects/{{project}}/blueprints/{quote(blueprint_key, safe='')}",
            params=_params(opts),
        )
        return BlueprintDeleteResult.model_validate(resp.json())

    def list_interactions(
        self, opts: ListInteractionsOptions | None = None
    ) -> InteractionListResponse:
        resp = self._request(
            "GET", "/v1/projects/{project}/interactions", params=_params(opts)
        )
        return InteractionListResponse.model_validate(resp.json())

    def list_project_permissions(self) -> PermissionCatalogResponse:
        resp = self._request("GET", "/v1/projects/{project}/permissions")
        return PermissionCatalogResponse.model_validate(resp.json())

    def list_principals(
        self, opts: ListPrincipalsOptions | None = None
    ) -> PrincipalListResponse:
        resp = self._request(
            "GET", "/v1/projects/{project}/principals", params=_params(opts)
        )
        return PrincipalListResponse.model_validate(resp.json())

    def create_principal(self, request: CreatePrincipalRequest) -> Principal:
        resp = self._request(
            "POST", "/v1/projects/{project}/principals", json=request
        )
        return Principal.model_validate(resp.json())

    def get_principal(self, principal_id: str) -> Principal:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/principals/{quote(principal_id, safe='')}",
        )
        return Principal.model_validate(resp.json())

    def update_principal(
        self, principal_id: str, request: UpdatePrincipalRequest
    ) -> Principal:
        resp = self._request(
            "PATCH",
            f"/v1/projects/{{project}}/principals/{quote(principal_id, safe='')}",
            json=request,
        )
        return Principal.model_validate(resp.json())

    def delete_principal(self, principal_id: str) -> None:
        self._request(
            "DELETE",
            f"/v1/projects/{{project}}/principals/{quote(principal_id, safe='')}",
        )

    def list_roles(self, opts: ListRolesOptions | None = None) -> RoleListResponse:
        resp = self._request(
            "GET", "/v1/projects/{project}/roles", params=_params(opts)
        )
        return RoleListResponse.model_validate(resp.json())

    def create_role(self, request: CreateRoleRequest) -> ProjectRole:
        resp = self._request("POST", "/v1/projects/{project}/roles", json=request)
        return ProjectRole.model_validate(resp.json())

    def get_role(self, role_id: str) -> ProjectRole:
        resp = self._request(
            "GET", f"/v1/projects/{{project}}/roles/{quote(role_id, safe='')}"
        )
        return ProjectRole.model_validate(resp.json())

    def update_role(self, role_id: str, request: UpdateRoleRequest) -> ProjectRole:
        resp = self._request(
            "PATCH",
            f"/v1/projects/{{project}}/roles/{quote(role_id, safe='')}",
            json=request,
        )
        return ProjectRole.model_validate(resp.json())

    def delete_role(self, role_id: str) -> None:
        self._request(
            "DELETE", f"/v1/projects/{{project}}/roles/{quote(role_id, safe='')}"
        )

    def list_role_assignments(
        self, opts: ListRoleAssignmentsOptions | None = None
    ) -> RoleAssignmentListResponse:
        resp = self._request(
            "GET", "/v1/projects/{project}/role-assignments", params=_params(opts)
        )
        return RoleAssignmentListResponse.model_validate(resp.json())

    def create_role_assignment(
        self, request: CreateRoleAssignmentRequest
    ) -> RoleAssignment:
        resp = self._request(
            "POST", "/v1/projects/{project}/role-assignments", json=request
        )
        return RoleAssignment.model_validate(resp.json())

    def delete_role_assignment(self, assignment_id: str) -> None:
        self._request(
            "DELETE",
            f"/v1/projects/{{project}}/role-assignments/{quote(assignment_id, safe='')}",
        )

    def get_agent_memory(self, agent_id: str) -> AgentMemory:
        """Return a summary of an agent's private memory."""
        resp = self._request(
            "GET", f"/v1/projects/{{project}}/agents/{quote(agent_id, safe='')}/memory"
        )
        return AgentMemory.model_validate(resp.json())

    def list_agent_memory_entries(
        self, agent_id: str, opts: ListAgentMemoryEntriesOptions | None = None
    ) -> AgentMemoryEntryListResponse:
        """List or search an agent's memory entries.

        The response preserves ``search_coverage`` so callers can see when
        semantic or hybrid results ranked only a partially indexed subset.
        """
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/agents/{quote(agent_id, safe='')}/memory/entries",
            params=_params(opts),
        )
        return AgentMemoryEntryListResponse.model_validate(resp.json())

    def save_agent_memory_entry(
        self, agent_id: str, key: str, request: SaveAgentMemoryEntryRequest
    ) -> AgentMemoryEntry:
        """Create or update the memory entry stored under ``key``."""
        resp = self._request(
            "PUT",
            f"/v1/projects/{{project}}/agents/{quote(agent_id, safe='')}/memory/entries/{quote(key, safe='')}",
            json=request,
        )
        return AgentMemoryEntry.model_validate(resp.json())

    def delete_agent_memory_entry(self, agent_id: str, key: str) -> None:
        """Delete the memory entry stored under ``key``."""
        self._request(
            "DELETE",
            f"/v1/projects/{{project}}/agents/{quote(agent_id, safe='')}/memory/entries/{quote(key, safe='')}",
        )

    def list_agent_memory_changes(
        self, agent_id: str, *, after: str | None = None, limit: int | None = None
    ) -> AgentMemoryChangeListResponse:
        """Return one page of the append-only memory change feed.

        A 410 :class:`MobiusAPIError` means ``after`` predates retained
        history; recover with :meth:`sync_agent_memory` or by relisting
        entries.
        """
        params: dict[str, Any] = {}
        if after:
            params["after"] = after
        if limit:
            params["limit"] = limit
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/agents/{quote(agent_id, safe='')}/memory/changes",
            params=params or None,
        )
        return AgentMemoryChangeListResponse.model_validate(resp.json())

    def sync_agent_memory(
        self, agent_id: str, cursor: str | None = None
    ) -> MemorySyncResult:
        """Advance a memory change-feed consumer by one bounded step.

        Drains every change page after ``cursor`` and returns the new feed
        position to persist. When the cursor has expired (HTTP 410) it
        recovers explicitly — establishing a fresh feed position and
        returning a full entry snapshot with ``reset=True`` — instead of
        failing or silently replaying. Pass ``None`` on first use. Polling
        cadence and retry policy stay with the caller; this makes no timing
        decisions.
        """
        try:
            changes, next_cursor = self._drain_agent_memory_changes(agent_id, cursor)
            return MemorySyncResult(reset=False, next_cursor=next_cursor, changes=changes)
        except MobiusAPIError as err:
            if err.status != 410:
                raise
        # The cursor predates retained history. Take the fresh feed position
        # BEFORE the snapshot: a mutation racing the snapshot then replays
        # after the new cursor (versions make replays detectable) instead of
        # being lost.
        _, next_cursor = self._drain_agent_memory_changes(agent_id, None)
        entries: list[AgentMemoryEntry] = []
        entries_cursor: str | None = None
        while True:
            page = self.list_agent_memory_entries(
                agent_id, ListAgentMemoryEntriesOptions(cursor=entries_cursor)
            )
            entries.extend(page.items)
            if not page.has_more or not page.next_cursor:
                break
            entries_cursor = page.next_cursor
        return MemorySyncResult(reset=True, next_cursor=next_cursor, entries=entries)

    def _drain_agent_memory_changes(
        self, agent_id: str, after: str | None
    ) -> tuple[list[AgentMemoryChange], str]:
        changes: list[AgentMemoryChange] = []
        cursor = after
        while True:
            page = self.list_agent_memory_changes(agent_id, after=cursor)
            changes.extend(page.items)
            cursor = page.next_cursor
            if not page.has_more:
                return changes, cursor

    def list_organization_actions(
        self, opts: ListOrganizationActionsOptions | None = None
    ) -> OrganizationActionListResponse:
        """List signed HTTP actions owned by the active organization.

        Requires Admin or Owner membership.
        """
        resp = self._request(
            "GET", "/v1/organization/actions", params=_params(opts)
        )
        return OrganizationActionListResponse.model_validate(resp.json())

    def create_organization_action(
        self, request: CreateOrganizationActionRequest
    ) -> OrganizationActionSecretMaterial:
        """Create an organization-owned signed HTTP action.

        Returns the action's one-time secret material. The signing key is
        revealed only in this response; persist ``key_bytes`` before
        discarding the result.
        """
        resp = self._request("POST", "/v1/organization/actions", json=request)
        action = OrganizationAction.model_validate(resp.json())
        return _organization_action_secret_material(
            "create organization action", action, "active"
        )

    def get_organization_action(self, action_id: str) -> OrganizationAction:
        """Return one organization action. Reads never include secret material."""
        resp = self._request(
            "GET", f"/v1/organization/actions/{quote(action_id, safe='')}"
        )
        return OrganizationAction.model_validate(resp.json())

    def update_organization_action(
        self, action_id: str, request: UpdateOrganizationActionRequest
    ) -> OrganizationAction:
        """Update the shared definition or enable/disable invocation."""
        resp = self._request(
            "PATCH",
            f"/v1/organization/actions/{quote(action_id, safe='')}",
            json=request,
        )
        return OrganizationAction.model_validate(resp.json())

    def delete_organization_action(self, action_id: str) -> None:
        """Delete the shared definition from future project catalogs."""
        self._request(
            "DELETE", f"/v1/organization/actions/{quote(action_id, safe='')}"
        )

    def rotate_organization_action_secret(
        self, action_id: str
    ) -> OrganizationActionSecretMaterial:
        """Create a pending key version and return its one-time material.

        Mobius keeps signing with the current active version until
        :meth:`activate_organization_action_secret_version` promotes the
        pending one — distribute the new key to verifiers first, then
        activate.
        """
        resp = self._request(
            "POST",
            f"/v1/organization/actions/{quote(action_id, safe='')}/secret/rotate",
        )
        action = OrganizationAction.model_validate(resp.json())
        return _organization_action_secret_material(
            "rotate organization action secret", action, "pending"
        )

    def activate_organization_action_secret_version(
        self, action_id: str, version: int, *, overlap_seconds: int | None = None
    ) -> OrganizationAction:
        """Atomically make a pending version active.

        The previous active version, if any, keeps verifying for
        ``overlap_seconds`` (server default 24 hours; pass 0 to cut over
        immediately).
        """
        body = (
            ActivateOrganizationActionSecretRequest(overlap_seconds=overlap_seconds)
            if overlap_seconds is not None
            else ActivateOrganizationActionSecretRequest()
        )
        resp = self._request(
            "POST",
            f"/v1/organization/actions/{quote(action_id, safe='')}/secret/versions/{version}/activate",
            json=body,
        )
        return OrganizationAction.model_validate(resp.json())

    def revoke_organization_action_secret_version(
        self, action_id: str, version: int
    ) -> OrganizationAction:
        """Immediately revoke a non-active key version.

        The active signing version can be revoked only after another version
        is activated or the action is disabled.
        """
        resp = self._request(
            "POST",
            f"/v1/organization/actions/{quote(action_id, safe='')}/secret/versions/{version}/revoke",
        )
        return OrganizationAction.model_validate(resp.json())

    def start_run(self, loop_id: str, opts: StartRunOptions | None = None) -> LoopRun:
        opts = opts or StartRunOptions()
        values = _drop_none(opts.__dict__)
        idempotency_key = _normalize_idempotency_key(
            values.pop("idempotency_key", None)
        )
        external_id = _normalize_idempotency_key(values.pop("external_id", None))
        if external_id is not None:
            if idempotency_key not in (None, external_id):
                raise ValueError(
                    "idempotency_key and deprecated external_id must match when both are set"
                )
            idempotency_key = idempotency_key or external_id
        if idempotency_key is not None:
            values["idempotency_key"] = idempotency_key
        body = StartLoopRunRequest(**values)
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}/runs",
            json=body,
            idempotency_key=body.idempotency_key,
        )
        return LoopRun.model_validate(resp.json())

    def list_runs(self, opts: ListRunsOptions | None = None) -> LoopRunListResponse:
        resp = self._request("GET", "/v1/projects/{project}/runs", params=_params(opts))
        return LoopRunListResponse.model_validate(resp.json())

    def get_run(self, run_id: str) -> LoopRun:
        resp = self._request("GET", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}")
        return LoopRun.model_validate(resp.json())

    def cancel_run(self, run_id: str, reason: str | None = None) -> LoopRun:
        body = CancelLoopRunRequest(reason=reason)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/cancel", json=body)
        return LoopRun.model_validate(resp.json())

    def signal_run(
        self,
        run_id: str,
        step_key: str,
        result: dict[str, Any] | None = None,
    ) -> LoopRun:
        body = SignalLoopRunRequest(step_key=step_key, result=result)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/signals", json=body)
        return LoopRun.model_validate(resp.json())

    # Resolves (or creates) a session, appends opts.content as the caller's
    # input message, and starts an agent turn in one retryable call. Returns
    # once the turn is accepted. The returned TurnTranscript carries the
    # turn's identity (id, session_id, status) immediately and its live
    # transcript on demand: the stream is lazy, so iterate the handle to
    # render the turn as it runs, or never iterate for fire-and-forget. Use
    # invoke_agent_stream instead to observe the turn's activity inline on
    # the same connection with v1 session-stream framing.
    def invoke_agent(self, opts: InvokeAgentOptions) -> TurnTranscript:
        body = _invoke_agent_request(opts)
        resp = self._request(
            "POST",
            "/v1/projects/{project}/agents/invoke",
            json=body,
            idempotency_key=_invoke_agent_replay_key(body),
        )
        ack = TurnAck.model_validate(resp.json())
        return self._turn_transcript(ack, "invoke agent")

    def start_turn(self, session_id: str, opts: StartTurnOptions) -> TurnTranscript:
        """Append caller input to an existing session and start a turn."""
        if not opts.content:
            raise ValueError("mobius: start turn: content is required")
        idempotency_key = _normalize_idempotency_key(opts.idempotency_key)
        body = StartTurnRequest(
            role="user",
            content=opts.content,
            context=(
                RuntimeContext(root=opts.context) if opts.context is not None else None
            ),
            idempotency_key=idempotency_key,
            operation=opts.operation,
            output=opts.output,
            metadata=opts.metadata,
        )
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/turns",
            json=body,
            idempotency_key=body.idempotency_key,
        )
        ack = TurnAck.model_validate(resp.json())
        return self._turn_transcript(ack, "start turn")

    def _turn_transcript(self, ack: TurnAck, operation: str) -> TurnTranscript:
        if not ack.resume_cursor.strip():
            raise ValueError(f"mobius: {operation} response missing resume_cursor")
        transcript = SessionTranscript()
        transcript.seed(ack)
        return TurnTranscript(self, ack, transcript)

    # Behaves like invoke_agent but streams the turn's activity inline on
    # the same connection instead of waiting for a TurnAck.
    def invoke_agent_stream(self, opts: InvokeAgentOptions) -> Iterator[SessionStreamEvent]:
        body = _invoke_agent_request(opts)
        payload = _model_dump(body)
        path = self._path("/v1/projects/{project}/agents/invoke")
        with self._client.stream(
            "POST",
            path,
            json=payload,
            headers=_idempotency_headers(
                _invoke_agent_replay_key(body),
                accept="text/event-stream",
            ),
        ) as resp:
            _raise_for_response(resp)
            buf = ""
            for chunk in resp.iter_text():
                buf += chunk
                while "\n\n" in buf:
                    raw, buf = buf.split("\n\n", 1)
                    lines = raw.splitlines()
                    data = "\n".join(
                        line.removeprefix("data:").lstrip()
                        for line in lines
                        if line.startswith("data:")
                    )
                    event_type = next(
                        (
                            line.removeprefix("event:").lstrip()
                            for line in lines
                            if line.startswith("event:")
                        ),
                        None,
                    )
                    if not data or not event_type:
                        continue
                    yield SessionStreamEvent(event_type=event_type, data=json.loads(data))

    def list_sessions(self, opts: ListSessionsOptions | None = None) -> SessionListResponse:
        resp = self._request("GET", "/v1/projects/{project}/sessions", params=_params(opts))
        return SessionListResponse.model_validate(resp.json())

    def get_session(self, session_id: str) -> Session:
        resp = self._request(
            "GET", f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}"
        )
        return Session.model_validate(resp.json())

    def cancel_session(self, session_id: str, *, force: bool = False) -> Session:
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/cancel",
            params={"force": "true"} if force else None,
        )
        return Session.model_validate(resp.json())

    def compact_session(self, session_id: str) -> Session:
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/compact",
        )
        return Session.model_validate(resp.json())

    def list_session_messages(
        self, session_id: str, opts: ListSessionMessagesOptions | None = None
    ) -> SessionMessageListResponse:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/messages",
            params=_params(opts),
        )
        return SessionMessageListResponse.model_validate(resp.json())

    def nudge_session(self, session_id: str, opts: NudgeSessionOptions) -> SessionNudgeAck:
        values = _drop_none(opts.__dict__)
        idempotency_key = _normalize_idempotency_key(
            values.pop("idempotency_key", None)
        )
        if idempotency_key is not None:
            values["idempotency_key"] = idempotency_key
        body = NudgeSessionRequest(**values)
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/nudges",
            json=body,
            idempotency_key=body.idempotency_key,
        )
        return SessionNudgeAck.model_validate(resp.json())

    def list_session_nudges(
        self, session_id: str, opts: ListSessionNudgesOptions | None = None
    ) -> SessionNudgeListResponse:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/nudges",
            params=_params(opts),
        )
        return SessionNudgeListResponse.model_validate(resp.json())

    def get_session_nudge(self, session_id: str, nudge_id: str) -> SessionNudge:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/nudges/"
            f"{quote(nudge_id, safe='')}",
        )
        return SessionNudge.model_validate(resp.json())

    def cancel_nudge(self, session_id: str, nudge_id: str) -> SessionNudge:
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/nudges/"
            f"{quote(nudge_id, safe='')}/cancel",
        )
        return SessionNudge.model_validate(resp.json())

    def list_session_turns(
        self, session_id: str, opts: ListSessionTurnsOptions | None = None
    ) -> AgentTurnListResponse:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/turns",
            params=_params(opts),
        )
        return AgentTurnListResponse.model_validate(resp.json())

    def get_session_turn(self, session_id: str, turn_id: str) -> AgentTurn:
        resp = self._request(
            "GET",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/turns/"
            f"{quote(turn_id, safe='')}",
        )
        return AgentTurn.model_validate(resp.json())

    def cancel_turn(self, session_id: str, turn_id: str) -> AgentTurn:
        resp = self._request(
            "POST",
            f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/turns/"
            f"{quote(turn_id, safe='')}/cancel",
        )
        return AgentTurn.model_validate(resp.json())

    # Fetch a session transcript snapshot (session-stream v2). Without a cursor
    # this is a bootstrap tail (latest final page + all live rows and turns);
    # with a cursor it drains everything after it toward a fixed upper cut —
    # continue with the returned next_page_token until has_more is false. Fold
    # each page into a SessionTranscript with apply_snapshot.
    def get_session_transcript(
        self,
        session_id: str,
        opts: GetSessionTranscriptOptions | None = None,
    ) -> SessionTranscriptSnapshot:
        params: dict[str, Any] = {}
        if opts is not None:
            if opts.cursor:
                params["cursor"] = opts.cursor
            if opts.page_token:
                params["page_token"] = opts.page_token
            if opts.limit:
                params["limit"] = opts.limit
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript"
        resp = self._request("GET", path, params=params)
        data = resp.json()
        # The interactions projection was added to transcript snapshots in an
        # additive rollout. Accept snapshots from pre-rollout servers while the
        # generated model keeps the current contract strict.
        data.setdefault("interactions", [])
        return SessionTranscriptSnapshot.model_validate(data)

    # Open one session-transcript SSE connection and yield each decoded frame
    # with its resume cursor (TranscriptStreamEvent.id). Apply the events to a
    # SessionTranscript, or use watch_session_transcript for the managed
    # connection loop (reconnect on rotate, stop on idle).
    def stream_session_transcript(
        self,
        session_id: str,
        opts: StreamSessionTranscriptOptions | None = None,
    ) -> Iterator[TranscriptStreamEvent]:
        params = {"cursor": opts.cursor} if opts and opts.cursor else None
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript/stream"
        with self._client.stream(
            "GET", self._path(path, params=params), headers={"Accept": "text/event-stream"}
        ) as resp:
            _raise_for_stream_status(resp)
            yield from _iter_transcript_frames(resp)

    # Follow a session's live transcript across the full connection lifecycle.
    # The returned watcher owns the connection loop and the view; iterate it to
    # fold frames in, and read messages() between steps. The stream is lazy —
    # iteration opens it. Reconnects with the current cursor on a stream.end
    # rotate (and after a dropped connection), and stops on a stream.end idle.
    # On idle the caller can poll get_session_transcript and reopen when
    # resume_cursor moves.
    def watch_session_transcript(
        self,
        session_id: str,
        opts: WatchSessionTranscriptOptions | None = None,
    ) -> TranscriptWatcher:
        transcript = (opts.transcript if opts else None) or SessionTranscript()
        if opts and opts.cursor and not transcript.cursor:
            transcript.cursor = opts.cursor
        delay = opts.reconnect_delay if opts is not None else 1.0
        follow = opts.follow if opts is not None else False
        return TranscriptWatcher(self, session_id, transcript, delay, follow)

    # _watch_transcript is the reconnecting engine behind TurnTranscript and
    # TranscriptWatcher: it folds every state frame into the view and yields
    # after each change. stop_turn_id, when set, ends iteration after that
    # turn's terminal turn.upsert (already applied).
    def _watch_transcript(
        self,
        session_id: str,
        transcript: SessionTranscript,
        stop_turn_id: str | None,
        delay: float,
        follow: bool = False,
    ) -> Iterator[SessionTranscript]:
        for update in self._watch_transcript_updates(
            session_id, transcript, stop_turn_id, delay, follow
        ):
            yield update.transcript

    def _watch_transcript_updates(
        self,
        session_id: str,
        transcript: SessionTranscript,
        stop_turn_id: str | None,
        delay: float,
        follow: bool = False,
    ) -> Iterator[TranscriptUpdate]:
        path = f"/v1/projects/{{project}}/sessions/{quote(session_id, safe='')}/transcript/stream"
        reconnect_count = 0
        while True:
            params = {"cursor": transcript.cursor} if transcript.cursor else None
            rotate = False
            transcript._reset_ready()
            try:
                with self._client.stream(
                    "GET",
                    self._path(path, params=params),
                    headers={"Accept": "text/event-stream"},
                ) as resp:
                    _raise_for_stream_status(resp)
                    self._logger.debug(
                        "mobius transcript stream opened",
                        extra={
                            "session_id": session_id,
                            "reconnect_count": reconnect_count,
                        },
                    )
                    for event in _iter_transcript_frames(resp):
                        frame = event.frame
                        event_type = frame.get("event_type")
                        if event_type == "stream.end":
                            if frame.get("reason") == "idle":
                                self._logger.debug(
                                    "mobius transcript stream idle",
                                    extra={"session_id": session_id},
                                )
                                if not follow:
                                    return
                                rotate = False
                                reconnect_count += 1
                                break
                            rotate = True  # reconnect immediately
                            reconnect_count += 1
                            self._logger.debug(
                                "mobius transcript stream rotating",
                                extra={
                                    "session_id": session_id,
                                    "reconnect_count": reconnect_count,
                                },
                            )
                            break
                        transcript.apply(event)
                        yield TranscriptUpdate(
                            frame=frame,
                            cursor=transcript.cursor,
                            transcript=transcript,
                            connection="open",
                            reconnect_count=reconnect_count,
                        )
                        if (
                            event_type == "turn.upsert"
                            and stop_turn_id
                            and frame.get("id") == stop_turn_id
                            and is_terminal_turn_status(frame.get("status", ""))
                        ):
                            return
            except httpx.HTTPStatusError as exc:
                # A permanent status (401/403/404, or a 5xx other than 503) is
                # not transient — surface it instead of reconnecting forever.
                if not _is_retryable_stream_status(exc.response.status_code):
                    raise
                rotate = False  # 429/503 — reconnect after the delay
                reconnect_count += 1
            except MobiusAPIError as exc:
                if not _is_retryable_stream_status(exc.status):
                    raise
                rotate = False
                reconnect_count += 1
            except httpx.TransportError as exc:
                rotate = False  # dropped connection / EOF — reconnect after delay
                reconnect_count += 1
                self._logger.debug(
                    "mobius transcript transport error",
                    extra={
                        "session_id": session_id,
                        "reconnect_count": reconnect_count,
                        "error_type": type(exc).__name__,
                    },
                )
            if not rotate:
                time.sleep(delay)

    def watch_run(self, run_id: str, since: int = 0) -> Iterator[RunEvent]:
        params = {"after_sequence": since} if since > 0 else None
        path = f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/events.stream"
        with self._client.stream("GET", self._path(path, params=params)) as resp:
            resp.raise_for_status()
            buf = ""
            for chunk in resp.iter_text():
                buf += chunk
                while True:
                    sep = _SSE_FRAME_SEP.search(buf)
                    if sep is None:
                        break
                    raw, buf = buf[: sep.start()], buf[sep.end() :]
                    data = "\n".join(
                        line.removeprefix("data:").lstrip()
                        for line in raw.splitlines()
                        if line.startswith("data:")
                    )
                    if not data:
                        continue
                    event = LoopRunEvent.model_validate_json(data)
                    yield RunEvent(
                        id=event.id,
                        run_id=event.run_id,
                        event_type=event.event_type,
                        sequence=event.sequence,
                        payload=event.payload,
                    )

    def wait_run(self, run_id: str, opts: WaitRunOptions | None = None) -> LoopRun:
        opts = opts or WaitRunOptions()
        since = opts.since
        deadline = time.monotonic() + opts.timeout if opts.timeout else None
        while True:
            run = self.get_run(run_id)
            if is_terminal_run_status(run.status):
                return run
            for event in self.watch_run(run_id, since=since):
                since = max(since, event.sequence)
                status = (event.payload or {}).get("status")
                if isinstance(status, str) and is_terminal_run_status(status):
                    return self.get_run(run_id)
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(f"timed out waiting for run {run_id}")
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"timed out waiting for run {run_id}")
            time.sleep(opts.reconnect_delay)

    def _request(
        self,
        method: str,
        path: str,
        *,
        json: Any | None = None,
        params: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
        files: Any | None = None,
        data: dict[str, Any] | None = None,
    ) -> httpx.Response:
        payload = _model_dump(json) if json is not None else None
        request_path = self._path(path, params=params)
        started = time.monotonic()
        resp = self._client.request(
            method,
            request_path,
            json=payload,
            files=files,
            data=data,
            headers=_idempotency_headers(idempotency_key),
        )
        self._logger.debug(
            "mobius request complete",
            extra={
                "method": method,
                "path": request_path.split("?", 1)[0],
                "status": resp.status_code,
                "duration_ms": round((time.monotonic() - started) * 1000, 3),
            },
        )
        if resp.status_code == 401:
            raise AuthRevokedError()
        _raise_for_response(resp)
        return resp

    def _path(self, path: str, params: dict[str, Any] | None = None) -> str:
        out = path.replace("{project}", quote(self.project, safe=""))
        if params:
            clean = {k: v for k, v in params.items() if v not in (None, "")}
            if clean:
                out = f"{out}?{urlencode(clean, doseq=True)}"
        return out


class TurnTranscript:
    """A started agent turn and its live transcript, from ``invoke_agent``.

    The identity attributes (``id``, ``session_id``, …) are available
    immediately; the transcript stream is lazy — iteration opens it, so a
    caller that never iterates pays for nothing beyond the invoke itself.

    Iterate the handle to render the turn as it streams::

        turn = client.invoke_agent(opts)
        for t in turn:
            render(t.messages())
        turn.status  # "completed"

    Iteration yields after every state change and stops once this turn
    reaches a terminal ``turn.upsert``, reconnecting through stream rotations
    and dropped connections along the way. Before yielding that terminal
    update it reconciles the incremental durable snapshot, so :meth:`messages`
    includes the rows committed with completion; permanent stream or snapshot
    errors raise. The full session stream is consumed internally so the resume
    cursor stays valid when other turns interleave; :meth:`messages` is scoped
    to this turn, and ``transcript`` exposes the whole session view.
    """

    def __init__(self, client: Client, ack: TurnAck, transcript: SessionTranscript) -> None:
        self._client = client
        # Turn id.
        self.id: str = ack.turn.id
        # Id of the session this turn runs in.
        self.session_id: str = ack.session.id
        # Durable v1 stream cursor from the turn-start response; pass it as
        # after_sequence to GET …/sessions/{id}/stream to follow this turn on
        # the v1 session stream instead.
        self.after_sequence: int = ack.after_sequence
        # True when a repeated idempotency key returned an existing turn
        # without restarting it.
        self.deduped: bool = bool(ack.deduped)
        # Full session view the stream folds into.
        self.transcript: SessionTranscript = transcript
        # Immutable invocation boundary for initial replay and terminal
        # settlement. The transcript cursor keeps moving for reconnects.
        self._invocation_cursor = ack.resume_cursor
        # Set when deduplication returned an already-terminal turn. There is
        # nothing to stream, so iteration fetches
        # the snapshot (all pages) instead, making messages() complete either
        # way.
        self._hydrate = is_terminal_turn_status(str(ack.turn.status))
        self._diagnostics = TranscriptDiagnostics(
            status=str(ack.turn.status),
            cursor=transcript.cursor,
            ready=transcript.ready,
            reconnect_count=0,
            last_frame_type=None,
            last_frame_at=None,
            connection="idle",
        )

    @property
    def status(self) -> str:
        """The turn's lifecycle status ("queued", "running", "completed", …).

        Live: each applied turn.upsert updates it.
        """
        return (self.transcript.turn(self.id) or {}).get("status", "")

    @property
    def error_type(self) -> str | None:
        """Live Mobius-owned failure category, when the turn failed."""
        value = (self.transcript.turn(self.id) or {}).get("error_type")
        return str(value) if value else None

    @property
    def error_message(self) -> str | None:
        """Live human-readable terminal failure message."""
        value = (self.transcript.turn(self.id) or {}).get("error_message")
        return str(value) if value else None

    @property
    def output(self) -> dict[str, Any] | None:
        """The turn's validated structured output.

        Present only on a completed turn that declared an output contract
        (see InvokeAgentOptions.output); None until the terminal turn.upsert
        frame is applied. Read this instead of parsing the transcript messages.
        """
        value = (self.transcript.turn(self.id) or {}).get("output")
        return value if isinstance(value, dict) else None

    @property
    def output_source(self) -> str | None:
        """Where output came from: "tool" (reserved mobius_submit_output tool)
        or "text" (schema-valid final-message fallback); None when the turn
        produced no structured output.
        """
        value = (self.transcript.turn(self.id) or {}).get("output_source")
        return str(value) if value else None

    @property
    def error(self) -> Exception | None:
        """Combined turn failure, distinct from transcript transport errors."""
        if self.status != "failed":
            return None
        message = self.error_message or self.error_type or "mobius: turn failed"
        if self.error_type and self.error_message:
            message = f"{self.error_type}: {self.error_message}"
        return RuntimeError(message)

    def messages(self) -> list[dict[str, Any]]:
        """This turn's rows, in render order."""
        return self.transcript.messages_for_turn(self.id)

    def renderable_messages(self) -> list[dict[str, Any]]:
        """This turn's UI-oriented transcript projection."""
        return self.transcript.renderable_messages_for_turn(self.id)

    def diagnostics(self) -> TranscriptDiagnostics:
        """Observed transport and turn facts; backend state is not inferred."""
        return TranscriptDiagnostics(
            status=self.status,
            cursor=self.transcript.cursor,
            ready=self.transcript.ready,
            reconnect_count=self._diagnostics.reconnect_count,
            last_frame_type=self._diagnostics.last_frame_type,
            last_frame_at=self._diagnostics.last_frame_at,
            connection=self._diagnostics.connection,
        )

    def updates(self) -> Iterator[TranscriptUpdate]:
        """Yield applied protocol frames and their accumulated view."""
        if self._hydrate:
            self._hydrate_snapshot()
            return
        turn = self.transcript.turn(self.id)
        if turn and is_terminal_turn_status(turn.get("status", "")):
            return
        try:
            for update in self._client._watch_transcript_updates(
                self.session_id, self.transcript, self.id, 1.0
            ):
                turn = update.transcript.turn(self.id)
                terminal = bool(turn) and is_terminal_turn_status(
                    str(turn.get("status") or "")
                )
                if terminal:
                    self._reconcile_snapshot(self._invocation_cursor)
                    update = TranscriptUpdate(
                        frame=update.frame,
                        cursor=self.transcript.cursor,
                        transcript=self.transcript,
                        connection="ended",
                        reconnect_count=update.reconnect_count,
                    )
                self._diagnostics = TranscriptDiagnostics(
                    status=self.status,
                    cursor=update.cursor,
                    ready=self.transcript.ready,
                    reconnect_count=update.reconnect_count,
                    last_frame_type=str(update.frame.get("event_type") or ""),
                    last_frame_at=time.time(),
                    connection=update.connection,
                )
                yield update
                if terminal:
                    return
        finally:
            self._diagnostics = TranscriptDiagnostics(
                status=self.status,
                cursor=self.transcript.cursor,
                ready=self.transcript.ready,
                reconnect_count=self._diagnostics.reconnect_count,
                last_frame_type=self._diagnostics.last_frame_type,
                last_frame_at=self._diagnostics.last_frame_at,
                connection="ended",
            )

    def __iter__(self) -> Iterator[TurnTranscript]:
        if self._hydrate:
            self._hydrate_snapshot()
            yield self
            return
        # Already terminal (a completed prior iteration): nothing left to stream.
        turn = self.transcript.turn(self.id)
        if turn and is_terminal_turn_status(turn.get("status", "")):
            return
        for _ in self.updates():
            yield self

    def _hydrate_snapshot(self) -> None:
        self._hydrate = False
        self._reconcile_snapshot(self._invocation_cursor)
        self._diagnostics = TranscriptDiagnostics(
            status=self.status,
            cursor=self.transcript.cursor,
            ready=self.transcript.ready,
            reconnect_count=0,
            last_frame_type=None,
            last_frame_at=None,
            connection="ended",
        )

    def _reconcile_snapshot(self, cursor: str) -> None:
        opts = GetSessionTranscriptOptions(cursor=cursor)
        while True:
            snap = self._client.get_session_transcript(self.session_id, opts)
            self.transcript.apply_snapshot(snap)
            if not snap.has_more or not snap.next_page_token:
                break
            opts = GetSessionTranscriptOptions(page_token=snap.next_page_token)


class TranscriptWatcher:
    """A session's live transcript feed, from ``watch_session_transcript``.

    Iterate the handle to fold frames into ``transcript`` (yielded after
    every state change); the stream is lazy — iteration opens it. Iteration
    stops on a stream.end idle; permanent stream errors raise. Read the
    resume position from ``transcript.cursor`` to resume a later watch.
    """

    def __init__(
        self,
        client: Client,
        session_id: str,
        transcript: SessionTranscript,
        reconnect_delay: float,
        follow: bool,
    ) -> None:
        self._client = client
        self.session_id = session_id
        # The session view the stream folds into.
        self.transcript: SessionTranscript = transcript
        self._reconnect_delay = reconnect_delay
        self._follow = follow
        self._diagnostics = TranscriptDiagnostics(
            status="",
            cursor=transcript.cursor,
            ready=transcript.ready,
            reconnect_count=0,
            last_frame_type=None,
            last_frame_at=None,
            connection="idle",
        )

    def diagnostics(self) -> TranscriptDiagnostics:
        return self._diagnostics

    def updates(self) -> Iterator[TranscriptUpdate]:
        try:
            for update in self._client._watch_transcript_updates(
                self.session_id,
                self.transcript,
                None,
                self._reconnect_delay,
                self._follow,
            ):
                self._diagnostics = TranscriptDiagnostics(
                    status="",
                    cursor=update.cursor,
                    ready=self.transcript.ready,
                    reconnect_count=update.reconnect_count,
                    last_frame_type=str(update.frame.get("event_type") or ""),
                    last_frame_at=time.time(),
                    connection=update.connection,
                )
                yield update
        finally:
            self._diagnostics = TranscriptDiagnostics(
                status="",
                cursor=self.transcript.cursor,
                ready=self.transcript.ready,
                reconnect_count=self._diagnostics.reconnect_count,
                last_frame_type=self._diagnostics.last_frame_type,
                last_frame_at=self._diagnostics.last_frame_at,
                connection="ended",
            )

    def __iter__(self) -> Iterator[SessionTranscript]:
        for update in self.updates():
            yield update.transcript


# SSE events are separated by a blank line, which may be framed with LF or
# CRLF — both are valid per the spec. Match either so a CRLF-framed stream
# still splits into frames instead of buffering forever.
_SSE_FRAME_SEP = re.compile(r"\r?\n\r?\n")


def _raise_for_stream_status(resp: httpx.Response) -> None:
    # Mirror _request: a revoked credential is surfaced as AuthRevokedError so
    # callers get the same typed error whether they stream or not.
    if resp.status_code == 401:
        raise AuthRevokedError()
    _raise_for_response(resp)


def _raise_for_response(resp: httpx.Response) -> None:
    if resp.is_success:
        return
    try:
        payload = resp.json()
    except (ValueError, json.JSONDecodeError):
        resp.raise_for_status()
        return
    error = payload.get("error") if isinstance(payload, dict) else None
    if isinstance(error, dict) and isinstance(error.get("code"), str):
        retry_after: float | None = None
        try:
            if resp.headers.get("Retry-After"):
                retry_after = float(resp.headers["Retry-After"])
        except ValueError:
            pass
        raise MobiusAPIError(
            status=resp.status_code,
            code=error["code"],
            message=str(error.get("message") or resp.reason_phrase),
            details=error.get("details") if isinstance(error.get("details"), dict) else None,
            request_id=resp.headers.get("X-Request-ID") or resp.headers.get("Request-ID"),
            retry_after=retry_after,
        )
    resp.raise_for_status()


def _is_retryable_stream_status(status: int) -> bool:
    # Mirror the transport retry policy (docs/retries.md): only 429 and 503 are
    # transient. Every other status — including 401/403/404 and the other 5xx —
    # is surfaced to the caller instead of triggering an endless reconnect.
    return status in (429, 503)


def _iter_transcript_frames(resp: httpx.Response) -> Iterator[TranscriptStreamEvent]:
    """Parse a transcript SSE body into TranscriptStreamEvents.

    Per the SSE spec the last-event-id persists across events until an ``id:``
    line changes it — the transcript stream relies on this so a frame that
    repeats already-delivered state can omit ``id:`` without regressing the
    resume cursor.
    """
    buf = ""
    last_id: str | None = None
    for chunk in resp.iter_text():
        buf += chunk
        while True:
            sep = _SSE_FRAME_SEP.search(buf)
            if sep is None:
                break
            raw, buf = buf[: sep.start()], buf[sep.end() :]
            lines = raw.splitlines()
            data = "\n".join(
                line.removeprefix("data:").lstrip()
                for line in lines
                if line.startswith("data:")
            )
            if not data:
                continue
            event_type = next(
                (line.removeprefix("event:").lstrip() for line in lines if line.startswith("event:")),
                None,
            )
            id_line = next(
                (line.removeprefix("id:").lstrip() for line in lines if line.startswith("id:")),
                None,
            )
            if id_line is not None:
                last_id = id_line
            frame = json.loads(data)
            yield TranscriptStreamEvent(
                event_type=event_type or frame.get("event_type", ""),
                frame=frame,
                id=last_id,
            )


def _extract_handle_from_api_key(api_key: str) -> str | None:
    if not (api_key.startswith("mbx_") or api_key.startswith("mbc_")):
        return None
    if "." not in api_key:
        return None
    handle = api_key.rsplit(".", 1)[1]
    if not handle:
        return None
    if not _HANDLE_RE.match(handle):
        raise ValueError(f"invalid project handle suffix in API key: {handle!r}")
    return handle


def _model_dump(value: Any) -> Any:
    if hasattr(value, "model_dump"):
        # by_alias is required so fields aliased off python-reserved names
        # (e.g. TurnOutputSpec.schema_ -> "schema", condition "if_" -> "if")
        # serialize under their wire names.
        return value.model_dump(mode="json", exclude_none=True, by_alias=True)
    if isinstance(value, dict):
        return {k: _model_dump(v) for k, v in value.items() if v is not None}
    if isinstance(value, list):
        return [_model_dump(v) for v in value]
    return value


def _drop_none(values: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in values.items() if v is not None}


def _organization_action_secret_material(
    op: str, action: OrganizationAction, want_status: str
) -> OrganizationActionSecretMaterial:
    """Extract the one-time secret from a create or rotate response.

    The revealed signing_secret always belongs to the newest entry in
    secret_versions, whose status must match ``want_status``; any other
    shape means the response is internally inconsistent. Errors never
    include the secret itself.
    """
    if not action.signing_secret:
        raise ValueError(
            f"mobius: {op}: response is missing the one-time signing_secret"
        )
    if not action.secret_versions:
        raise ValueError(
            f"mobius: {op}: response has no secret_versions for the revealed secret"
        )
    newest = max(action.secret_versions, key=lambda v: v.version)
    if newest.status != want_status:
        raise ValueError(
            f"mobius: {op}: newest secret version {newest.version} has status"
            f" {newest.status.value!r}, want {want_status!r}"
        )
    try:
        key_bytes = base64.b64decode(action.signing_secret, validate=True)
    except (ValueError, TypeError):
        raise ValueError(
            f"mobius: {op}: signing_secret is not valid base64"
        ) from None
    return OrganizationActionSecretMaterial(
        action=action.model_copy(update={"signing_secret": None}),
        secret_ref=action.secret_ref,
        version=newest.version,
        key_bytes=key_bytes,
    )


def _invoke_agent_request(opts: InvokeAgentOptions) -> InvokeAgentRequest:
    if not opts.agent_id and not opts.agent_name:
        raise ValueError("mobius: invoke agent: agent_id or agent_name is required")
    if not opts.content:
        raise ValueError("mobius: invoke agent: content is required")
    return InvokeAgentRequest(
        agent_ref=AgentRef(id=opts.agent_id, name=opts.agent_name),
        input=InvokeInput(
            content=opts.content,
            context=(
                RuntimeContext(root=opts.context) if opts.context is not None else None
            ),
            idempotency_key=_normalize_idempotency_key(opts.idempotency_key),
            metadata=opts.input_metadata,
        ),
        session=opts.session,
        config=opts.config,
        operation=opts.operation,
        output=opts.output,
        channel_context=opts.channel_context,
    )


def _normalize_idempotency_key(value: str | None) -> str | None:
    normalized = value.strip() if value is not None else ""
    return normalized or None


def _idempotency_headers(
    value: str | None,
    *,
    accept: str | None = None,
) -> dict[str, str]:
    headers: dict[str, str] = {}
    key = _normalize_idempotency_key(value)
    if key is not None:
        headers["Idempotency-Key"] = key
    if accept is not None:
        headers["Accept"] = accept
    return headers


def _invoke_agent_replay_key(body: InvokeAgentRequest) -> str | None:
    if body.session is not None and body.session.mode is not None:
        mode = body.session.mode.value
        if mode == "new":
            return None
    return _normalize_idempotency_key(body.input.idempotency_key)


def _merge_loop_fields(opts: Any) -> dict[str, Any]:
    """Flatten loop options into loop request fields.

    The loop spec (steps, event, config, triggers, ...) lives inline on the
    loop, so the ``spec`` mapping is merged into the top-level request fields.
    Explicit option fields take precedence over the same keys in ``spec``.
    """
    fields = dict(opts.__dict__)
    spec = fields.pop("spec", None) or {}
    return {**spec, **_drop_none(fields)}


def _params(opts: Any | None) -> dict[str, Any] | None:
    if opts is None:
        return None
    if hasattr(opts, "__dict__"):
        values = opts.__dict__
    else:
        values = dict(opts)
    return {k: _query_value(v) for k, v in _drop_none(values).items()}


def _query_value(value: Any) -> Any:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (list, tuple)):
        return [_query_value(item) for item in value]
    return value.value if hasattr(value, "value") else value


def is_terminal_run_status(status: LoopRunStatus | str) -> bool:
    value = status.value if hasattr(status, "value") else str(status)
    return value in {"completed", "failed", "cancelled"}
