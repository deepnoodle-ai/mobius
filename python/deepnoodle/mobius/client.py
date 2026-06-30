from __future__ import annotations

import json
import re
import time
from dataclasses import dataclass
from collections.abc import Iterator
from typing import Any
from urllib.parse import quote, urlencode, urlparse, urlunparse

import httpx

from ._api.models import (
    CancelLoopRunRequest,
    CreateLoopRequest,
    Loop,
    LoopListResponse,
    LoopRun,
    LoopRunEvent,
    LoopRunListResponse,
    LoopRunSource,
    LoopRunStatus,
    LoopStatus,
    SignalLoopRunRequest,
    StartLoopRunRequest,
    TagMap,
    UpdateLoopRequest,
)
from .errors import AuthRevokedError, RateLimitError
from .retry import DEFAULT_MAX_RETRIES, RetryingTransport

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
        base_transport = transport or httpx.HTTPTransport()
        self._client = httpx.Client(
            base_url=self.base_url,
            timeout=opts.timeout,
            transport=RetryingTransport(base_transport, max_retries=opts.retry),
            headers={
                "Authorization": f"Bearer {opts.api_key}",
                "Content-Type": "application/json",
            },
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

    def start_run(self, loop_id: str, opts: StartRunOptions | None = None) -> LoopRun:
        opts = opts or StartRunOptions()
        values = _drop_none(opts.__dict__)
        if "external_id" in values:
            values["idempotency_key"] = values.pop("external_id")
        body = StartLoopRunRequest(**values)
        resp = self._request("POST", f"/v1/projects/{{project}}/loops/{quote(loop_id, safe='')}/runs", json=body)
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

    def watch_run(self, run_id: str, since: int = 0) -> Iterator[RunEvent]:
        params = {"after_sequence": since} if since > 0 else None
        path = f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/events.stream"
        with self._client.stream("GET", self._path(path, params=params)) as resp:
            resp.raise_for_status()
            buf = ""
            for chunk in resp.iter_text():
                buf += chunk
                while "\n\n" in buf:
                    raw, buf = buf.split("\n\n", 1)
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
    ) -> httpx.Response:
        payload = _model_dump(json) if json is not None else None
        resp = self._client.request(method, self._path(path, params=params), json=payload)
        if resp.status_code == 401:
            raise AuthRevokedError()
        resp.raise_for_status()
        return resp

    def _path(self, path: str, params: dict[str, Any] | None = None) -> str:
        out = path.replace("{project}", quote(self.project, safe=""))
        if params:
            clean = {k: v for k, v in params.items() if v not in (None, "")}
            if clean:
                out = f"{out}?{urlencode(clean)}"
        return out


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
        return value.model_dump(mode="json", exclude_none=True)
    if isinstance(value, dict):
        return {k: _model_dump(v) for k, v in value.items() if v is not None}
    if isinstance(value, list):
        return [_model_dump(v) for v in value]
    return value


def _drop_none(values: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in values.items() if v is not None}


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
    return value.value if hasattr(value, "value") else value


def is_terminal_run_status(status: LoopRunStatus | str) -> bool:
    value = status.value if hasattr(status, "value") else str(status)
    return value in {"completed", "failed", "cancelled"}
