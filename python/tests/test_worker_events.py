from __future__ import annotations

import asyncio
import json

import httpx
import pytest

from deepnoodle.mobius import Client, ClientOptions
from deepnoodle.mobius._api.models import WorkerSocketClaimedJob
from deepnoodle.mobius.errors import AuthRevokedError, WorkerInstanceConflictError
from deepnoodle.mobius.worker import (
    ModelCapability,
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
        origin="loop_action_step",
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


def test_register_frame_advertises_registered_generators() -> None:
    worker = Worker(
        _client(),
        WorkerConfig(
            worker_instance_id="worker-1",
            models=[ModelCapability(provider="ollama", model="llama3")],
        ),
    )
    # Same pair as config.models -> deduped; a distinct concrete model is added;
    # a "*" wildcard is not advertised.
    worker.register_generator("ollama", "llama3", lambda ctx, job, emit: {})
    worker.register_generator("ollama", "qwen2", lambda ctx, job, emit: {})
    worker.register_generator("ollama", "*", lambda ctx, job, emit: {})

    frame = worker._register_frame().model_dump(mode="json", exclude_none=True)

    assert frame["models"] == [
        {"provider": "ollama", "model": "llama3"},
        {"provider": "ollama", "model": "qwen2"},
    ]


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


def test_terminal_protocol_error_classifies_codes() -> None:
    worker = Worker(_client(), WorkerConfig(worker_instance_id="dup"))

    conflict = worker._terminal_protocol_error(
        {"code": "worker_instance_conflict", "message": "already registered"}
    )
    assert isinstance(conflict, WorkerInstanceConflictError)
    assert conflict.worker_instance_id == "dup"
    assert conflict.project_handle == "test-project"
    assert str(conflict) == "already registered"

    assert isinstance(
        worker._terminal_protocol_error({"code": "invalid_actor"}), AuthRevokedError
    )
    assert worker._terminal_protocol_error({"code": "register_failed"}) is None


@pytest.mark.asyncio
async def test_run_reraises_instance_conflict_without_reconnecting() -> None:
    worker = Worker(
        _client(), WorkerConfig(worker_instance_id="dup", reconnect_delay=0.01)
    )
    calls = 0

    async def fake_run_socket() -> None:
        nonlocal calls
        calls += 1
        if calls > 1:
            # Regression guard: a reconnect means the fix is broken. Stop the
            # loop so the test fails fast (run returns) instead of hanging.
            worker._stopping = True
            return
        raise WorkerInstanceConflictError(
            worker_instance_id="dup", project_handle="test-project"
        )

    worker._run_socket = fake_run_socket  # type: ignore[method-assign]

    with pytest.raises(WorkerInstanceConflictError):
        await worker.run()
    assert calls == 1


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


class _ServeSocket:
    """An async-iterable stand-in for a websockets connection.

    Frames pushed via :meth:`emit` are yielded by ``async for``; :meth:`close`
    ends the iteration. :meth:`send` records the JSON frames the worker writes.
    Used to drive :meth:`Worker._serve_socket` deterministically.
    """

    _STOP = object()

    def __init__(self) -> None:
        self.sent: list[dict[str, object]] = []
        self.closed = False
        self._queue: asyncio.Queue[object] = asyncio.Queue()

    async def send(self, message: str) -> None:
        self.sent.append(json.loads(message))

    async def close(self) -> None:
        if not self.closed:
            self.closed = True
            await self._queue.put(self._STOP)

    async def emit(self, frame: dict[str, object]) -> None:
        await self._queue.put(json.dumps(frame))

    def claims(self) -> list[dict[str, object]]:
        return [f for f in self.sent if f["type"] == "jobs.claim"]

    def __aiter__(self) -> "_ServeSocket":
        return self

    async def __anext__(self) -> str:
        item = await self._queue.get()
        if item is self._STOP:
            raise StopAsyncIteration
        return item  # type: ignore[return-value]


async def _drain() -> None:
    # Let the serve loop consume queued frames and run its claim coroutines.
    for _ in range(10):
        await asyncio.sleep(0)


def _serve_worker(claim_response_timeout: float) -> Worker:
    worker = Worker(_client(), WorkerConfig(worker_instance_id="worker-1"))
    worker._claim_response_timeout = claim_response_timeout
    return worker


@pytest.mark.asyncio
async def test_serve_socket_reconnects_when_claim_unanswered() -> None:
    worker = _serve_worker(0.02)
    ws = _ServeSocket()
    serve = asyncio.create_task(worker._serve_socket(ws))

    await ws.emit({"type": "worker.registered", "worker_session_token": "tok"})
    # The claim is never answered; the watchdog must close the socket and the
    # serve loop must raise so run() reconnects.
    with pytest.raises(RuntimeError, match="went unanswered"):
        await serve
    assert len(ws.claims()) == 1
    assert ws.closed


@pytest.mark.asyncio
async def test_serve_socket_clears_outstanding_claim_on_matching_error() -> None:
    worker = _serve_worker(5.0)
    ws = _ServeSocket()
    serve = asyncio.create_task(worker._serve_socket(ws))

    await ws.emit({"type": "worker.registered", "worker_session_token": "tok"})
    await _drain()
    claim_id = ws.claims()[0]["message_id"]

    # A nonterminal error answering the claim clears the outstanding flag, so
    # the next work.available re-claims.
    await ws.emit(
        {"type": "error", "message_id": claim_id, "error": {"code": "claim_failed"}}
    )
    await _drain()
    await ws.emit({"type": "work.available"})
    await _drain()

    assert len(ws.claims()) == 2
    await ws.close()
    await serve


@pytest.mark.asyncio
async def test_serve_socket_keeps_claim_outstanding_on_unmatched_error() -> None:
    worker = _serve_worker(5.0)
    ws = _ServeSocket()
    serve = asyncio.create_task(worker._serve_socket(ws))

    await ws.emit({"type": "worker.registered", "worker_session_token": "tok"})
    await _drain()

    # An error for some other message must not clear our outstanding claim, so
    # work.available stays a no-op.
    await ws.emit(
        {"type": "error", "message_id": "msg_other", "error": {"code": "claim_failed"}}
    )
    await _drain()
    await ws.emit({"type": "work.available"})
    await _drain()

    assert len(ws.claims()) == 1
    await ws.close()
    await serve


@pytest.mark.asyncio
async def test_serve_socket_does_not_reclaim_after_empty_claimed() -> None:
    worker = _serve_worker(5.0)
    ws = _ServeSocket()
    serve = asyncio.create_task(worker._serve_socket(ws))

    await ws.emit({"type": "worker.registered", "worker_session_token": "tok"})
    await _drain()
    await ws.emit({"type": "jobs.claimed", "jobs": []})
    await _drain()

    # No job ran, so nothing re-claims; an empty response must not hot-poll.
    assert len(ws.claims()) == 1
    await ws.close()
    await serve
