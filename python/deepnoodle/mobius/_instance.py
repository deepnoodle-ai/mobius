"""Worker instance ID auto-detection.

The Mobius SDK identifies each running worker process by a stable
``worker_instance_id``. Operators can configure one explicitly, but the
common case is "let the SDK figure it out from the runtime platform" so
that two replicas of the same image surface as two distinct rows.

Resolution order matches the Go and TypeScript SDKs:

1. explicit (caller-configured value)
2. ``K_REVISION`` + Cloud Run instance metadata
3. ``HOSTNAME`` (Kubernetes pod, Docker container)
4. ``FLY_MACHINE_ID``
5. ``RAILWAY_REPLICA_ID``
6. ``RENDER_INSTANCE_ID``
7. UUID per boot
"""

from __future__ import annotations

import os
import uuid
from typing import Literal

import httpx

InstanceIDSource = Literal[
    "configured",
    "cloud_run_revision_instance",
    "hostname",
    "fly_machine_id",
    "railway_replica_id",
    "render_instance_id",
    "generated_uuid",
]

# Cap the metadata-server probe so a non-Cloud-Run host doesn't pay a
# full TCP timeout on startup.
_CLOUD_RUN_METADATA_TIMEOUT = 1.0


def resolve_instance_id(explicit: str | None) -> tuple[str, InstanceIDSource]:
    """Resolve the per-process ``worker_instance_id`` plus its source label.

    The source label is informational only — workers log it once at
    startup so operators can confirm the right platform was picked up.
    """
    if explicit:
        return explicit, "configured"
    cloud_run = _cloud_run_instance_id()
    if cloud_run:
        return cloud_run, "cloud_run_revision_instance"
    hostname = (os.environ.get("HOSTNAME") or "").strip()
    if hostname:
        return hostname, "hostname"
    fly = (os.environ.get("FLY_MACHINE_ID") or "").strip()
    if fly:
        return fly, "fly_machine_id"
    railway = (os.environ.get("RAILWAY_REPLICA_ID") or "").strip()
    if railway:
        return railway, "railway_replica_id"
    render = (os.environ.get("RENDER_INSTANCE_ID") or "").strip()
    if render:
        return render, "render_instance_id"
    return str(uuid.uuid4()), "generated_uuid"


def _cloud_run_instance_id() -> str | None:
    revision = (os.environ.get("K_REVISION") or "").strip()
    if not revision:
        return None
    try:
        resp = httpx.get(
            "http://metadata.google.internal/computeMetadata/v1/instance/id",
            headers={"Metadata-Flavor": "Google"},
            timeout=_CLOUD_RUN_METADATA_TIMEOUT,
        )
    except httpx.HTTPError:
        return revision
    if resp.status_code != 200:
        return revision
    instance = (resp.text or "").strip()
    if not instance:
        return revision
    return f"{revision}-{instance}"
