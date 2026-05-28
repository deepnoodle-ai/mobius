from __future__ import annotations

import asyncio
import json

import httpx
import pytest

from deepnoodle.mobius import Client, ClientOptions
from deepnoodle.mobius._api.models import WorkerSocketClaimedJob
from deepnoodle.mobius.worker import (
    Worker,
    WorkerConfig,
    WorkerPool,
    WorkerPoolConfig,
    _worker_config_values,
)


class FakeWebSocket:
    def __init__(self) -> None:
        self.sent: list[dict[str, object]] = []

    async def send(self, message: str) -> None:
        self.sent.append(json.loads(message))


def _client() -> Client:
    return Client(
        ClientOptions(
            api_key="mbx_test",
            base_url="https://api.example.invalid",
            project="test-project",
        ),
        transport=httpx.MockTransport(lambda _: httpx.Response(500)),
    )


def _job(job_id: str = "job_1", action_name: str = "demo.action") -> WorkerSocketClaimedJob:
    return WorkerSocketClaimedJob(
        id=job_id,
        kind="action_execution",
        origin="automation_action_step",
        executor_kind="customer_worker",
        queue="default",
        action_name=action_name,
        run_id="run_1",
        step_id="step_1",
        spec={"parameters": {"topic": "sdk"}},
        lease_token="lease-1",
        claim_attempt=1,
        lease_duration_seconds=60,
        heartbeat_cadence_seconds=60,
    )


def test_register_frame_uses_configured_capacity_and_registered_actions() -> None:
    worker = Worker(
        _client(),
        WorkerConfig(worker_instance_id="worker-1", concurrency=8, queues=["gpu"]),
    )
    worker.register("demo.action", lambda params: params)

    frame = worker._register_frame().model_dump(mode="json", exclude_none=True)

    assert frame["type"] == "worker.register"
    assert frame["worker_instance_id"] == "worker-1"
    assert frame["concurrency_limit"] == 8
    assert frame["available_slots"] == 8
    assert frame["queues"] == ["gpu"]
    assert frame["action_names"] == ["demo.action"]


@pytest.mark.asyncio
async def test_worker_executes_action_job_and_reports_result() -> None:
    worker = Worker(_client(), WorkerConfig(worker_instance_id="worker-1"))
    seen: dict[str, object] = {}

    def action(params, ctx):
        seen["params"] = params
        seen["job_id"] = ctx.job_id
        return {"ok": True}

    worker.register("demo.action", action)
    ws = FakeWebSocket()
    await worker._execute_job(ws, _job())

    reports = [frame for frame in ws.sent if frame["type"] == "job.report"]
    assert seen == {"params": {"topic": "sdk"}, "job_id": "job_1"}
    assert reports == [
        {
            "type": "job.report",
            "message_id": reports[0]["message_id"],
            "job_id": "job_1",
            "lease_token": "lease-1",
            "status": "completed",
            "result": {"ok": True},
        }
    ]


@pytest.mark.asyncio
async def test_worker_reports_cancelled_when_action_task_is_cancelled() -> None:
    worker = Worker(_client(), WorkerConfig(worker_instance_id="worker-1"))
    started = asyncio.Event()

    async def action(_params):
        started.set()
        await asyncio.sleep(60)

    worker.register("demo.action", action)
    ws = FakeWebSocket()
    task = asyncio.create_task(worker._execute_job(ws, _job()))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    reports = [frame for frame in ws.sent if frame["type"] == "job.report"]
    assert reports[-1]["status"] == "cancelled"
    assert reports[-1]["error_type"] == "Cancelled"


def test_worker_pool_config_drops_pool_only_fields() -> None:
    config = WorkerPoolConfig(
        worker_instance_id_prefix="pool-worker",
        count=3,
        concurrency=2,
        queues=["default"],
    )

    values = _worker_config_values(config)

    assert values["concurrency"] == 2
    assert values["queues"] == ["default"]
    assert "count" not in values
    assert "worker_instance_id_prefix" not in values


def test_worker_pool_registers_actions_for_children() -> None:
    pool = WorkerPool(_client(), WorkerPoolConfig(count=2))

    def action(params):
        return params

    assert pool.register("demo.action", action) is pool
    assert pool.actions["demo.action"] is action
