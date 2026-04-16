from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import httpx
from pydantic import BaseModel

# Generated models from make generate-py
from ._api.models import (
    JobClaim,
    JobClaimDataResponse,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    JobHeartbeat,
    JobHeartbeatDataResponse,
)

DEFAULT_BASE_URL = "https://api.mobiusops.ai"


@dataclass
class ClientOptions:
    api_key: str
    base_url: str = DEFAULT_BASE_URL
    namespace: str = "default"
    timeout: float = 60.0


class LeaseLostError(Exception):
    def __init__(self, job_id: str) -> None:
        super().__init__(f"lease lost for job {job_id}")
        self.job_id = job_id


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
        namespace: str = "default",
        timeout: float = 60.0,
    ) -> None:
        if isinstance(opts, ClientOptions):
            resolved = opts
        else:
            resolved = ClientOptions(
                api_key=opts, base_url=base_url, namespace=namespace, timeout=timeout
            )

        self._opts = resolved
        self._http = httpx.Client(
            base_url=resolved.base_url,
            headers={"Authorization": f"Bearer {resolved.api_key}"},
            timeout=resolved.timeout,
        )

    @property
    def namespace(self) -> str:
        return self._opts.namespace

    # --- Runtime API ---------------------------------------------------------

    def claim_task(self, req: JobClaimRequest) -> JobClaim | None:
        resp = self._http.post(
            f"/namespaces/{self.namespace}/jobs/claim",
            json={"data": _dump(req)},
        )
        if resp.status_code == 204:
            return None
        resp.raise_for_status()
        return JobClaimDataResponse.model_validate(resp.json()).data

    def heartbeat_task(
        self, job_id: str, req: JobFenceRequest
    ) -> JobHeartbeat:
        resp = self._http.post(
            f"/namespaces/{self.namespace}/jobs/{job_id}/heartbeat",
            json={"data": _dump(req)},
        )
        if resp.status_code == 409:
            raise LeaseLostError(job_id)
        resp.raise_for_status()
        return JobHeartbeatDataResponse.model_validate(resp.json()).data

    def complete_task(self, job_id: str, req: JobCompleteRequest) -> None:
        resp = self._http.post(
            f"/namespaces/{self.namespace}/jobs/{job_id}/complete",
            json={"data": _dump(req)},
        )
        if resp.status_code == 409:
            raise LeaseLostError(job_id)
        resp.raise_for_status()

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()


def _dump(model: BaseModel) -> dict[str, Any]:
    return model.model_dump(mode="json", exclude_none=True)
