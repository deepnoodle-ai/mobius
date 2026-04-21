from __future__ import annotations

from dataclasses import dataclass
from typing import Any
from urllib.parse import quote

import httpx
from pydantic import BaseModel

# Generated models from make generate-py
from ._api.models import (
    JobClaim,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    JobHeartbeat,
)
from .errors import RateLimitError
from .retry import DEFAULT_MAX_RETRIES, RetryingTransport

DEFAULT_BASE_URL = "https://api.mobiusops.ai"


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
    worker_id: str
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
        resp.raise_for_status()
        return JobClaim.model_validate(resp.json())

    def heartbeat_job(
        self, job_id: str, req: JobFenceRequest
    ) -> JobHeartbeat:
        project = quote(self.namespace, safe="")
        job = quote(job_id, safe="")
        resp = self._http.post(
            f"/v1/projects/{project}/jobs/{job}/heartbeat",
            json=_dump(req),
        )
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
        worker_id: str,
        attempt: int,
        type: str,
        payload: dict[str, Any],
    ) -> None:
        self.emit_job_events(
            job_id,
            JobEventsRequest(
                worker_id=worker_id,
                attempt=attempt,
                events=[JobEventEntry(type=type, payload=payload)],
            ),
        )

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()


def _dump(model: BaseModel) -> dict[str, Any]:
    return model.model_dump(mode="json", exclude_none=True)
