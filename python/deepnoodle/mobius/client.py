from __future__ import annotations

import json
import re
import time
from dataclasses import dataclass
from collections.abc import Iterator
from typing import Any
from urllib.parse import quote

import httpx
from pydantic import BaseModel

# Generated models from make generate-py
from ._api.models import (
    ConfigEntries,
    CreateWorkflowRequest,
    JobClaim,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    JobHeartbeat,
    RunSignal,
    SendRunSignalRequest,
    StartBoundRunRequest,
    StartInlineRunRequest,
    TagMap,
    UpdateWorkflowRequest,
    WorkflowDefinition,
    WorkflowDefinitionListResponse,
    WorkflowDefinitionSummary,
    WorkflowRun,
    WorkflowRunDetail,
    WorkflowRunListResponse,
    WorkflowRunStatus,
    WorkflowSpec,
)
from .errors import AuthRevokedError, RateLimitError, WorkerInstanceConflictError
from .retry import DEFAULT_MAX_RETRIES, RetryingTransport

DEFAULT_BASE_URL = "https://api.mobiusops.ai"

# Mirrors the server-side handle regex in domain/validate.go so we can
# reject malformed handles at construction time — a project-pinned
# credential embeds the handle as "<handle>/mbx_<secret>".
_HANDLE_RE = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


@dataclass
class ClientOptions:
    api_key: str
    base_url: str = DEFAULT_BASE_URL
    project: str = "default"
    namespace: str | None = None
    timeout: float = 60.0
    # Number of retries for 429/503 responses. 0 disables retries; 429
    # responses then surface as RateLimitError immediately. See
    # ../../docs/retries.md for the shared retry policy.
    retry: int = DEFAULT_MAX_RETRIES

    def __post_init__(self) -> None:
        if self.namespace and self.project == "default":
            self.project = self.namespace
        self.namespace = self.project


@dataclass
class StartRunOptions:
    queue: str | None = None
    inputs: dict[str, Any] | None = None
    metadata: dict[str, str] | None = None
    tags: TagMap | None = None
    external_id: str | None = None
    config: ConfigEntries | None = None


@dataclass
class ListRunsOptions:
    status: WorkflowRunStatus | None = None
    workflow_type: str | None = None
    queue: str | None = None
    parent_run_id: str | None = None
    initiated_by: str | None = None
    external_id: str | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class WaitRunOptions:
    since: int = 0
    reconnect_delay: float = 1.0
    timeout: float | None = None


@dataclass
class WorkflowOptions:
    name: str | None = None
    handle: str | None = None
    description: str | None = None
    published_as_tool: bool | None = None
    tags: TagMap | dict[str, str] | None = None


@dataclass
class UpdateWorkflowOptions:
    name: str | None = None
    description: str | None = None
    published_as_tool: bool | None = None
    spec: WorkflowSpec | None = None
    tags: TagMap | dict[str, str] | None = None


@dataclass
class ListWorkflowsOptions:
    cursor: str | None = None
    limit: int | None = None
    tag: list[str] | None = None


@dataclass
class WorkflowSyncResult:
    definition: WorkflowDefinition
    created: bool = False
    updated: bool = False


@dataclass
class WorkflowDefinitionConfig:
    spec: WorkflowSpec
    options: WorkflowOptions | None = None


@dataclass
class RunEvent:
    type: str
    run_id: str
    seq: int
    timestamp: str
    data: dict[str, Any]


class LeaseLostError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"lease lost for job {job_id}")
        self.job_id = job_id


class PayloadTooLargeError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"custom event payload too large for job {job_id}")
        self.job_id = job_id


class RateLimitedError(RateLimitError):
    """Legacy per-job rate-limit error raised by :meth:`Client.emit_job_events`.

    Subclass of :class:`RateLimitError` so callers catching the newer,
    transport-raised ``RateLimitError`` also catch this. New code should
    prefer ``RateLimitError``.
    """

    def __init__(self, job_id: str, retry_after: int | None = None) -> None:
        msg = f"custom event rate limited for job {job_id}"
        if retry_after:
            msg += f" (retry after {retry_after}s)"
        super().__init__(retry_after=float(retry_after or 0), message=msg)
        self.job_id = job_id
        self.retry_after = retry_after


class JobEventEntry(BaseModel):
    type: str
    payload: dict[str, Any]


class JobEventsRequest(BaseModel):
    worker_instance_id: str | None = None
    worker_session_token: str | None = None
    attempt: int
    events: list[JobEventEntry]


class Client:
    """Low-level Mobius runtime API client.

    Request and response shapes are Pydantic models generated from
    ../openapi.yaml via ``make generate-py``. Workers should typically use the
    high-level :class:`~deepnoodle.mobius.Worker` rather than calling these
    methods directly.

    Workers claim individual *jobs* — one action invocation on behalf of a
    workflow run — from the runtime API and execute the corresponding
    registered action locally.
    """

    def __init__(
        self,
        opts: ClientOptions | str,
        *,
        base_url: str = DEFAULT_BASE_URL,
        project: str = "default",
        namespace: str | None = None,
        timeout: float = 60.0,
        retry: int = DEFAULT_MAX_RETRIES,
    ) -> None:
        if isinstance(opts, ClientOptions):
            resolved = opts
        else:
            resolved = ClientOptions(
                api_key=opts,
                base_url=base_url,
                project=project,
                namespace=namespace,
                timeout=timeout,
                retry=retry,
            )

        # Project-pinned credentials are issued as "<handle>/mbx_<secret>".
        # Split the optional handle prefix so the caller can stay with a
        # single environment variable — the handle is already in the
        # token. The full token still rides on the Authorization header
        # and the server re-validates prefix against the key's pinned
        # project as defence in depth.
        handle_in_key = _extract_handle_from_api_key(resolved.api_key)
        if handle_in_key is not None:
            # Derive the explicit project from the resolved options so a
            # caller-supplied ClientOptions(project=...) is honoured even
            # when __init__'s project/namespace kwargs stayed at their
            # defaults.
            explicit = (
                resolved.project if resolved.project != "default" else resolved.namespace
            )
            if explicit and explicit != "default" and explicit != handle_in_key:
                raise ValueError(
                    f"project={explicit!r} conflicts with the handle embedded "
                    f"in the API key ({handle_in_key!r})"
                )
            resolved.project = handle_in_key
            resolved.namespace = handle_in_key

        self._opts = resolved
        transport = RetryingTransport(
            httpx.HTTPTransport(),
            max_retries=resolved.retry,
        )
        self._http = httpx.Client(
            base_url=resolved.base_url,
            headers={"Authorization": f"Bearer {resolved.api_key}"},
            timeout=resolved.timeout,
            transport=transport,
        )

    @property
    def project(self) -> str:
        return self._opts.project

    @property
    def namespace(self) -> str:
        return self._opts.project

    # --- Runtime API ---------------------------------------------------------

    def claim_job(self, req: JobClaimRequest) -> JobClaim | None:
        project = quote(self.namespace, safe="")
        resp = self._http.post(
            f"/v1/projects/{project}/jobs/claim",
            json=_dump(req),
        )
        if resp.status_code == 204:
            return None
        if resp.status_code == 401:
            raise AuthRevokedError()
        if resp.status_code == 409:
            self._raise_instance_conflict(resp, req)
        resp.raise_for_status()
        return JobClaim.model_validate(resp.json())

    def _raise_instance_conflict(
        self, resp: httpx.Response, req: JobClaimRequest
    ) -> None:
        try:
            body = resp.json()
        except json.JSONDecodeError:
            body = {}
        if isinstance(body, dict) and body.get("code") == "worker_instance_conflict":
            raise WorkerInstanceConflictError(
                worker_instance_id=req.worker_instance_id,
                project_handle=self.project,
                message=body.get("message"),
            )
        # Any other 409 on claim is unexpected; surface the raw body so
        # an operator can diagnose without stripping detail.
        resp.raise_for_status()

    def heartbeat_job(
        self, job_id: str, req: JobFenceRequest
    ) -> JobHeartbeat:
        project = quote(self.namespace, safe="")
        job = quote(job_id, safe="")
        resp = self._http.post(
            f"/v1/projects/{project}/jobs/{job}/heartbeat",
            json=_dump(req),
        )
        if resp.status_code == 401:
            raise AuthRevokedError(job_id)
        if resp.status_code == 409:
            raise LeaseLostError(job_id)
        resp.raise_for_status()
        return JobHeartbeat.model_validate(resp.json())

    def complete_job(self, job_id: str, req: JobCompleteRequest) -> None:
        project = quote(self.namespace, safe="")
        job = quote(job_id, safe="")
        resp = self._http.post(
            f"/v1/projects/{project}/jobs/{job}/complete",
            json=_dump(req),
        )
        if resp.status_code == 401:
            raise AuthRevokedError(job_id)
        if resp.status_code == 409:
            raise LeaseLostError(job_id)
        resp.raise_for_status()

    def emit_job_events(self, job_id: str, req: JobEventsRequest) -> None:
        project = quote(self.namespace, safe="")
        job = quote(job_id, safe="")
        resp = self._http.post(
            f"/v1/projects/{project}/jobs/{job}/events",
            json=_dump(req),
        )
        if resp.status_code == 409:
            raise LeaseLostError(job_id)
        if resp.status_code == 413:
            raise PayloadTooLargeError(job_id)
        if resp.status_code == 429:
            retry_after = None
            if "Retry-After" in resp.headers:
                try:
                    retry_after = int(resp.headers["Retry-After"])
                except ValueError:
                    retry_after = None
            raise RateLimitedError(job_id, retry_after)
        resp.raise_for_status()

    def emit_job_event(
        self,
        job_id: str,
        *,
        worker_instance_id: str | None = None,
        worker_session_token: str | None = None,
        attempt: int,
        type: str,
        payload: dict[str, Any],
    ) -> None:
        self.emit_job_events(
            job_id,
            JobEventsRequest(
                worker_instance_id=worker_instance_id,
                worker_session_token=worker_session_token,
                attempt=attempt,
                events=[JobEventEntry(type=type, payload=payload)],
            ),
        )

    def start_run(
        self,
        spec: WorkflowSpec,
        opts: StartRunOptions | None = None,
    ) -> WorkflowRun:
        req = StartInlineRunRequest(mode="inline", spec=spec, **_start_opts(opts))
        project = quote(self.namespace, safe="")
        resp = self._http.post(f"/v1/projects/{project}/runs", json=_dump(req))
        resp.raise_for_status()
        return WorkflowRun.model_validate(resp.json())

    def start_workflow_run(
        self,
        workflow_id: str,
        opts: StartRunOptions | None = None,
    ) -> WorkflowRun:
        project = quote(self.namespace, safe="")
        workflow = quote(workflow_id, safe="")
        req = StartBoundRunRequest(**_start_opts(opts))
        resp = self._http.post(
            f"/v1/projects/{project}/workflows/{workflow}/runs",
            json=_dump(req),
        )
        resp.raise_for_status()
        return WorkflowRun.model_validate(resp.json())

    def list_runs(
        self,
        opts: ListRunsOptions | None = None,
    ) -> WorkflowRunListResponse:
        project = quote(self.namespace, safe="")
        resp = self._http.get(
            f"/v1/projects/{project}/runs",
            params=_list_runs_params(opts),
        )
        resp.raise_for_status()
        return WorkflowRunListResponse.model_validate(resp.json())

    def get_run(self, run_id: str) -> WorkflowRunDetail:
        project = quote(self.namespace, safe="")
        run = quote(run_id, safe="")
        resp = self._http.get(f"/v1/projects/{project}/runs/{run}")
        resp.raise_for_status()
        return WorkflowRunDetail.model_validate(resp.json())

    def cancel_run(self, run_id: str) -> None:
        project = quote(self.namespace, safe="")
        run = quote(run_id, safe="")
        resp = self._http.post(f"/v1/projects/{project}/runs/{run}/cancellations")
        resp.raise_for_status()

    def resume_run(self, run_id: str) -> None:
        project = quote(self.namespace, safe="")
        run = quote(run_id, safe="")
        resp = self._http.post(f"/v1/projects/{project}/runs/{run}/resumptions")
        resp.raise_for_status()

    def send_run_signal(
        self,
        run_id: str,
        name: str,
        payload: dict[str, Any] | None = None,
    ) -> RunSignal:
        project = quote(self.namespace, safe="")
        run = quote(run_id, safe="")
        req = SendRunSignalRequest(name=name, payload=payload)
        resp = self._http.post(
            f"/v1/projects/{project}/runs/{run}/signals",
            json=_dump(req),
        )
        resp.raise_for_status()
        return RunSignal.model_validate(resp.json())

    def watch_run(self, run_id: str, since: int = 0) -> Iterator[RunEvent]:
        project = quote(self.namespace, safe="")
        run = quote(run_id, safe="")
        params = {"since": since} if since > 0 else None
        with self._http.stream(
            "GET",
            f"/v1/projects/{project}/runs/{run}/events",
            params=params,
        ) as resp:
            resp.raise_for_status()
            yield from _parse_sse(resp.iter_lines())

    def wait_run(
        self,
        run_id: str,
        opts: WaitRunOptions | None = None,
    ) -> WorkflowRunDetail:
        opts = opts or WaitRunOptions()
        since = opts.since
        deadline = time.monotonic() + opts.timeout if opts.timeout is not None else None
        while True:
            run = self.get_run(run_id)
            if is_terminal_run_status(run.status):
                return run
            try:
                for event in self.watch_run(run_id, since=since):
                    since = max(since, event.seq)
                    if event.type != "run_updated":
                        continue
                    status = event.data.get("status")
                    if status == "completed" or status == "failed":
                        return self.get_run(run_id)
            except httpx.HTTPError:
                if deadline is not None and time.monotonic() >= deadline:
                    raise
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"timed out waiting for Mobius run {run_id}")
            time.sleep(opts.reconnect_delay)

    def list_workflows(
        self,
        opts: ListWorkflowsOptions | None = None,
    ) -> WorkflowDefinitionListResponse:
        project = quote(self.namespace, safe="")
        resp = self._http.get(
            f"/v1/projects/{project}/workflows",
            params=_list_workflows_params(opts),
        )
        resp.raise_for_status()
        return WorkflowDefinitionListResponse.model_validate(resp.json())

    def get_workflow(self, workflow_id: str) -> WorkflowDefinition:
        project = quote(self.namespace, safe="")
        workflow = quote(workflow_id, safe="")
        resp = self._http.get(f"/v1/projects/{project}/workflows/{workflow}")
        resp.raise_for_status()
        return WorkflowDefinition.model_validate(resp.json())

    def create_workflow(
        self,
        spec: WorkflowSpec,
        opts: WorkflowOptions | None = None,
    ) -> WorkflowDefinition:
        project = quote(self.namespace, safe="")
        req = _create_workflow_request(spec, opts)
        resp = self._http.post(
            f"/v1/projects/{project}/workflows",
            json=_dump(req),
        )
        resp.raise_for_status()
        return WorkflowDefinition.model_validate(resp.json())

    def update_workflow(
        self,
        workflow_id: str,
        opts: UpdateWorkflowOptions | None = None,
    ) -> WorkflowDefinition:
        project = quote(self.namespace, safe="")
        workflow = quote(workflow_id, safe="")
        req = _update_workflow_request(opts)
        resp = self._http.patch(
            f"/v1/projects/{project}/workflows/{workflow}",
            json=_dump(req),
        )
        resp.raise_for_status()
        return WorkflowDefinition.model_validate(resp.json())

    def ensure_workflow(
        self,
        spec: WorkflowSpec,
        opts: WorkflowOptions | None = None,
    ) -> WorkflowSyncResult:
        desired = _normalize_workflow_options(spec, opts)
        existing = self._find_workflow(desired)
        if existing is None:
            return WorkflowSyncResult(
                definition=self.create_workflow(spec, desired),
                created=True,
            )

        current = self.get_workflow(existing.id)
        update = _workflow_update_for_diff(current, spec, desired)
        if update is None:
            return WorkflowSyncResult(definition=current)
        return WorkflowSyncResult(
            definition=self.update_workflow(current.id, update),
            updated=True,
        )

    def sync_workflows(
        self,
        defs: list[WorkflowDefinitionConfig],
    ) -> list[WorkflowSyncResult]:
        return [
            self.ensure_workflow(defn.spec, defn.options)
            for defn in defs
        ]

    def _find_workflow(
        self,
        desired: WorkflowOptions,
    ) -> WorkflowDefinitionSummary | None:
        if not desired.handle and not desired.name:
            raise ValueError("mobius: ensure workflow requires a handle, name, or spec name")
        cursor = ""
        while True:
            page = self.list_workflows(ListWorkflowsOptions(cursor=cursor, limit=100))
            for item in page.items:
                if desired.handle and item.handle == desired.handle:
                    return item
                if not desired.handle and item.name == desired.name:
                    return item
            if not page.has_more or not page.next_cursor:
                return None
            cursor = page.next_cursor

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()


def _dump(model: BaseModel) -> dict[str, Any]:
    return model.model_dump(mode="json", exclude_none=True)


def _start_opts(opts: StartRunOptions | None) -> dict[str, Any]:
    if opts is None:
        return {}
    return {
        k: v
        for k, v in {
            "queue": opts.queue,
            "inputs": opts.inputs,
            "metadata": opts.metadata,
            "tags": opts.tags,
            "external_id": opts.external_id,
            "config": opts.config,
        }.items()
        if v is not None
    }


def _list_runs_params(opts: ListRunsOptions | None) -> dict[str, Any]:
    if opts is None:
        return {}
    return {
        k: v.value if isinstance(v, WorkflowRunStatus) else v
        for k, v in {
            "status": opts.status,
            "workflow_type": opts.workflow_type,
            "queue": opts.queue,
            "parent_run_id": opts.parent_run_id,
            "initiated_by": opts.initiated_by,
            "external_id": opts.external_id,
            "cursor": opts.cursor,
            "limit": opts.limit,
        }.items()
        if v is not None and v != ""
    }


def _create_workflow_request(
    spec: WorkflowSpec,
    opts: WorkflowOptions | None,
) -> CreateWorkflowRequest:
    normalized = _normalize_workflow_options(spec, opts)
    return CreateWorkflowRequest(
        name=normalized.name or spec.name,
        handle=normalized.handle,
        description=normalized.description,
        published_as_tool=normalized.published_as_tool,
        spec=spec,
        tags=normalized.tags,
    )


def _update_workflow_request(
    opts: UpdateWorkflowOptions | None,
) -> UpdateWorkflowRequest:
    if opts is None:
        return UpdateWorkflowRequest()
    return UpdateWorkflowRequest(
        name=opts.name,
        description=opts.description,
        published_as_tool=opts.published_as_tool,
        spec=opts.spec,
        tags=opts.tags,
    )


def _list_workflows_params(opts: ListWorkflowsOptions | None) -> dict[str, Any]:
    if opts is None:
        return {}
    return {
        k: v
        for k, v in {
            "cursor": opts.cursor,
            "limit": opts.limit,
            "tag": opts.tag,
        }.items()
        if v is not None and v != ""
    }


def _normalize_workflow_options(
    spec: WorkflowSpec,
    opts: WorkflowOptions | None,
) -> WorkflowOptions:
    if opts is None:
        return WorkflowOptions(name=spec.name)
    return WorkflowOptions(
        name=opts.name or spec.name,
        handle=opts.handle,
        description=opts.description,
        published_as_tool=opts.published_as_tool,
        tags=opts.tags,
    )


def _workflow_update_for_diff(
    current: WorkflowDefinition,
    spec: WorkflowSpec,
    desired: WorkflowOptions,
) -> UpdateWorkflowOptions | None:
    update = UpdateWorkflowOptions()
    changed = False
    if desired.name and current.name != desired.name:
        update.name = desired.name
        changed = True
    if desired.description and current.description != desired.description:
        update.description = desired.description
        changed = True
    if (
        desired.published_as_tool is not None
        and current.published_as_tool != desired.published_as_tool
    ):
        update.published_as_tool = desired.published_as_tool
        changed = True
    if desired.tags is not None and _tag_dict(current.tags) != _tag_dict(desired.tags):
        update.tags = desired.tags
        changed = True
    if _model_json(current.spec) != _model_json(spec):
        update.spec = spec
        changed = True
    return update if changed else None


def _model_json(model: BaseModel) -> str:
    return model.model_dump_json(exclude_none=True)


def _tag_dict(tags: TagMap | dict[str, str] | None) -> dict[str, str] | None:
    if tags is None:
        return None
    if isinstance(tags, TagMap):
        return tags.root
    return tags


def _parse_sse(lines: Iterator[str]) -> Iterator[RunEvent]:
    event_type = ""
    data_lines: list[str] = []

    def dispatch() -> RunEvent | None:
        nonlocal event_type, data_lines
        if not data_lines:
            event_type = ""
            return None
        raw = "\n".join(data_lines)
        data_lines = []
        event_type = ""
        payload = json.loads(raw)
        return RunEvent(
            type=str(payload.get("type", "")),
            run_id=str(payload.get("run_id", "")),
            seq=int(payload.get("seq", 0)),
            timestamp=str(payload.get("timestamp", "")),
            data=dict(payload.get("data") or {}),
        )

    for raw_line in lines:
        line = raw_line.rstrip("\r")
        if line == "":
            evt = dispatch()
            if evt is not None:
                yield evt
            continue
        if line.startswith(":"):
            continue
        field, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if field == "event":
            event_type = value
        elif field == "data":
            data_lines.append(value)
    evt = dispatch()
    if evt is not None:
        yield evt


def is_terminal_run_status(status: WorkflowRunStatus | str) -> bool:
    value = status.value if isinstance(status, WorkflowRunStatus) else status
    return value == "completed" or value == "failed"


def _extract_handle_from_api_key(api_key: str | None) -> str | None:
    """Return the project handle prefix from a credential, or None if absent.

    Raises ValueError when the prefix is present but malformed, so a bad
    credential fails construction instead of surfacing as a 403 later.
    """
    if not api_key:
        return None
    slash = api_key.find("/")
    if slash < 0:
        return None
    handle = api_key[:slash]
    if not _HANDLE_RE.match(handle):
        raise ValueError(f"invalid project handle prefix in API key: {handle!r}")
    return handle
