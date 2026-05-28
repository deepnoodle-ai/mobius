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
    Automation,
    AutomationListResponse,
    AutomationRun,
    AutomationRunEvent,
    AutomationRunListResponse,
    AutomationRunSource,
    AutomationRunStatus,
    AutomationStatus,
    AutomationTrigger,
    AutomationVersion,
    AutomationVersionListResponse,
    CancelAutomationRunRequest,
    CreateAutomationRequest,
    CreateAutomationVersionRequest,
    SignalAutomationRunRequest,
    StartAutomationRunRequest,
    TagMap,
    UpdateAutomationRequest,
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
class AutomationOptions:
    name: str
    handle: str
    description: str | None = None
    default_agent_id: str | None = None
    default_inputs: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    tags: TagMap | dict[str, str] | None = None
    triggers: list[AutomationTrigger] | None = None


@dataclass
class UpdateAutomationOptions:
    name: str | None = None
    description: str | None = None
    default_agent_id: str | None = None
    default_inputs: dict[str, Any] | None = None
    settings: dict[str, Any] | None = None
    status: AutomationStatus | None = None
    tags: TagMap | dict[str, str] | None = None
    triggers: list[AutomationTrigger] | None = None


@dataclass
class AutomationVersionOptions:
    compiled_plan: dict[str, Any] | None = None
    publish: bool = False


@dataclass
class ListAutomationsOptions:
    status: AutomationStatus | None = None
    cursor: str | None = None
    limit: int | None = None


@dataclass
class StartRunOptions:
    inputs: dict[str, Any] | None = None
    source: AutomationRunSource | None = None
    external_id: str | None = None


@dataclass
class ListRunsOptions:
    status: AutomationRunStatus | None = None
    automation_id: str | None = None
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
    """Mobius public API client for automations, runs, and workers."""

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

    def list_automations(self, opts: ListAutomationsOptions | None = None) -> AutomationListResponse:
        resp = self._request("GET", "/v1/projects/{project}/automations", params=_params(opts))
        return AutomationListResponse.model_validate(resp.json())

    def get_automation(self, handle: str) -> Automation:
        resp = self._request("GET", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}")
        return Automation.model_validate(resp.json())

    def create_automation(self, opts: AutomationOptions) -> Automation:
        body = CreateAutomationRequest(**_drop_none(opts.__dict__))
        resp = self._request("POST", "/v1/projects/{project}/automations", json=body)
        return Automation.model_validate(resp.json())

    def update_automation(self, handle: str, opts: UpdateAutomationOptions) -> Automation:
        body = UpdateAutomationRequest(**_drop_none(opts.__dict__))
        resp = self._request("PATCH", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}", json=body)
        return Automation.model_validate(resp.json())

    def delete_automation(self, handle: str) -> None:
        self._request("DELETE", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}")

    def list_automation_versions(self, handle: str) -> AutomationVersionListResponse:
        resp = self._request("GET", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}/versions")
        return AutomationVersionListResponse.model_validate(resp.json())

    def create_automation_version(
        self,
        handle: str,
        spec: dict[str, Any],
        opts: AutomationVersionOptions | None = None,
    ) -> AutomationVersion:
        opts = opts or AutomationVersionOptions()
        body = CreateAutomationVersionRequest(
            spec=spec,
            compiled_plan=opts.compiled_plan,
        )
        resp = self._request("POST", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}/versions", json=body)
        version = AutomationVersion.model_validate(resp.json())
        if opts.publish:
            self.publish_automation_version(handle, version.version)
        return version

    def publish_automation_version(self, handle: str, version: int) -> Automation:
        resp = self._request("POST", f"/v1/projects/{{project}}/automations/{quote(handle, safe='')}/versions/{version}/publication")
        return Automation.model_validate(resp.json())

    def start_run(self, automation_handle: str, opts: StartRunOptions | None = None) -> AutomationRun:
        return self.start_automation_run(automation_handle, opts)

    def start_automation_run(self, automation_handle: str, opts: StartRunOptions | None = None) -> AutomationRun:
        opts = opts or StartRunOptions()
        body = StartAutomationRunRequest(**_drop_none(opts.__dict__))
        resp = self._request("POST", f"/v1/projects/{{project}}/automations/{quote(automation_handle, safe='')}/runs", json=body)
        return AutomationRun.model_validate(resp.json())

    def list_runs(self, opts: ListRunsOptions | None = None) -> AutomationRunListResponse:
        resp = self._request("GET", "/v1/projects/{project}/runs", params=_params(opts))
        return AutomationRunListResponse.model_validate(resp.json())

    def get_run(self, run_id: str) -> AutomationRun:
        resp = self._request("GET", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}")
        return AutomationRun.model_validate(resp.json())

    def cancel_run(self, run_id: str, reason: str | None = None) -> AutomationRun:
        body = CancelAutomationRunRequest(reason=reason)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/cancellations", json=body)
        return AutomationRun.model_validate(resp.json())

    def signal_run(
        self,
        run_id: str,
        step_key: str,
        result: dict[str, Any] | None = None,
    ) -> AutomationRun:
        body = SignalAutomationRunRequest(step_key=step_key, result=result)
        resp = self._request("POST", f"/v1/projects/{{project}}/runs/{quote(run_id, safe='')}/signals", json=body)
        return AutomationRun.model_validate(resp.json())

    def watch_run(self, run_id: str, since: int = 0) -> Iterator[RunEvent]:
        params = {"since_sequence": since} if since > 0 else None
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
                    event = AutomationRunEvent.model_validate_json(data)
                    yield RunEvent(
                        id=event.id,
                        run_id=event.run_id,
                        event_type=event.event_type,
                        sequence=event.sequence,
                        payload=event.payload,
                    )

    def wait_run(self, run_id: str, opts: WaitRunOptions | None = None) -> AutomationRun:
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


def is_terminal_run_status(status: AutomationRunStatus | str) -> bool:
    value = status.value if hasattr(status, "value") else str(status)
    return value in {"completed", "failed", "cancelled"}
