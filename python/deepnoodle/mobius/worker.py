from __future__ import annotations

import asyncio
import inspect
import json
import logging
import os
import socket
import uuid
from dataclasses import dataclass, field, fields
from typing import Any, Awaitable, Callable

from ._api.models import (
    WorkerSocketClaimedJob,
    WorkerSocketJobHeartbeatFrame,
    WorkerSocketJobReportFrame,
    WorkerSocketJobsClaimFrame,
    WorkerSocketRegisterFrame,
)
from .client import Client

ActionFunc = Callable[..., Any]
GenerationFunc = Callable[..., Any]


@dataclass
class ModelCapability:
    provider: str
    model: str


@dataclass
class WorkerConfig:
    worker_instance_id: str | None = None
    concurrency: int = 1
    name: str | None = None
    version: str | None = None
    queues: list[str] = field(default_factory=list)
    actions: list[str] = field(default_factory=list)
    models: list[ModelCapability] = field(default_factory=list)
    reconnect_delay: float = 2.0
    heartbeat_interval: float | None = None
    logger: logging.Logger | None = None


@dataclass
class ActionContext:
    job_id: str
    worker_instance_id: str
    run_id: str | None = None
    session_id: str | None = None
    agent_turn_id: str | None = None
    tool_call_id: str | None = None
    project_id: str | None = None
    attempt: int = 1
    queue: str | None = None
    step_id: str | None = None
    action: str | None = None

    def emit_event(self, _type: str, _payload: dict[str, Any]) -> None:
        # The WebSocket worker protocol currently has generation.delta for
        # token streams, but no general custom-event frame.
        raise NotImplementedError(
            "ActionContext.emit_event is not supported on the WebSocket worker "
            "protocol; the only streaming frame is generation.delta"
        )


@dataclass
class GenerationJob:
    job_id: str
    spec: dict[str, Any]
    provider: str | None = None
    model: str | None = None
    run_id: str | None = None
    session_id: str | None = None
    agent_turn_id: str | None = None
    tool_call_id: str | None = None


class Worker:
    def __init__(
        self,
        client: Client,
        config: WorkerConfig | None = None,
        actions: dict[str, ActionFunc] | None = None,
    ) -> None:
        self.client = client
        self.config = config or WorkerConfig()
        self.actions: dict[str, ActionFunc] = actions or {}
        self.generators: dict[str, GenerationFunc] = {}
        self.session_token = ""
        self._stopping = False
        self._claim_outstanding = False
        self.logger = self.config.logger or logging.getLogger("mobius.worker")

    def register(self, name: str, fn: ActionFunc) -> Worker:
        self.actions[name] = fn
        return self

    def register_generator(self, provider: str, model: str, fn: GenerationFunc) -> Worker:
        self.generators[f"{provider}/{model}"] = fn
        return self

    def stop(self) -> None:
        self._stopping = True

    async def run(self) -> None:
        self.config.worker_instance_id = self.config.worker_instance_id or resolve_instance_id()
        while not self._stopping:
            try:
                await self._run_socket()
            except Exception:
                if self._stopping:
                    return
                self.logger.exception("worker socket disconnected; reconnecting")
                await asyncio.sleep(self.config.reconnect_delay)

    async def _run_socket(self) -> None:
        try:
            import websockets
        except ImportError as exc:  # pragma: no cover - dependency/environment guard
            raise RuntimeError("install the 'websockets' package to run Python workers") from exc

        headers = {"Authorization": f"Bearer {self.client.api_key}"}
        async with websockets.connect(self.client.worker_socket_url(), additional_headers=headers) as ws:
            await ws.send(self._register_frame().model_dump_json(exclude_none=True))
            running: dict[str, asyncio.Task[None]] = {}
            self._claim_outstanding = False

            def _on_job_done(task: asyncio.Task[None], job_id: str) -> None:
                running.pop(job_id, None)
                if self._stopping:
                    return
                asyncio.create_task(self._reclaim_after_done(ws, running))

            async for raw in ws:
                frame = json.loads(raw)
                kind = frame.get("type")
                if kind == "worker.registered":
                    self.session_token = frame["worker_session_token"]
                    self._claim_outstanding = await self._claim(ws, running, self._claim_outstanding)
                elif kind == "jobs.claimed":
                    self._claim_outstanding = False
                    for raw_job in frame.get("jobs", []):
                        if len(running) >= max(1, self.config.concurrency):
                            break
                        job = WorkerSocketClaimedJob.model_validate(raw_job)
                        task = asyncio.create_task(self._execute_job(ws, job))
                        running[job.id] = task
                        task.add_done_callback(lambda t, job_id=job.id: _on_job_done(t, job_id))
                elif kind == "work.available":
                    self._claim_outstanding = await self._claim(ws, running, self._claim_outstanding)
                elif kind == "job.heartbeat.ack":
                    cancel = frame.get("cancel")
                    task = running.get(frame.get("job_id")) if cancel else None
                    if task:
                        task.cancel()
                elif kind == "job.cancel":
                    task = running.get(frame.get("job_id"))
                    if task:
                        task.cancel()
                elif kind == "worker.drain":
                    self._stopping = True
                    await ws.send(json.dumps({"type": "worker.draining", "message_id": _msg_id()}))
                if self._stopping and not running:
                    return

    def _register_frame(self) -> WorkerSocketRegisterFrame:
        concurrency = max(1, self.config.concurrency)
        action_names = self.config.actions or sorted(self.actions)
        return WorkerSocketRegisterFrame(
            type="worker.register",
            message_id=_msg_id(),
            worker_instance_id=self.config.worker_instance_id or "",
            worker_session_token=self.session_token or None,
            concurrency_limit=concurrency,
            available_slots=concurrency,
            name=self.config.name,
            version=self.config.version,
            queues=self.config.queues or None,
            action_names=action_names or None,
            models=[m.__dict__ for m in self.config.models] or None,
        )

    async def _reclaim_after_done(self, ws: Any, running: dict[str, asyncio.Task[None]]) -> None:
        if self._claim_outstanding or self._stopping:
            return
        self._claim_outstanding = await self._claim(ws, running, self._claim_outstanding)

    async def _claim(self, ws: Any, running: dict[str, asyncio.Task[None]], outstanding: bool) -> bool:
        if outstanding or self._stopping:
            return outstanding
        available = max(1, self.config.concurrency) - len(running)
        if available <= 0:
            return False
        frame = WorkerSocketJobsClaimFrame(
            type="jobs.claim",
            message_id=_msg_id(),
            available_slots=available,
            queues=self.config.queues or None,
            action_names=(self.config.actions or sorted(self.actions)) or None,
            models=[m.__dict__ for m in self.config.models] or None,
        )
        await ws.send(frame.model_dump_json(exclude_none=True))
        return True

    async def _execute_job(self, ws: Any, job: WorkerSocketClaimedJob) -> None:
        hb = asyncio.create_task(self._heartbeat_loop(ws, job))
        ctx = ActionContext(
            job_id=job.id,
            worker_instance_id=self.config.worker_instance_id or "",
            run_id=job.run_id,
            session_id=job.session_id,
            agent_turn_id=job.agent_turn_id,
            tool_call_id=job.tool_call_id,
            project_id=self.client.project,
            attempt=job.claim_attempt,
            queue=job.queue,
            step_id=job.step_id,
            action=job.action_name,
        )
        try:
            if job.kind == "action_execution":
                action_name = job.action_name or str(job.spec.get("action_name") or "")
                fn = self.actions[action_name]
                result = await _call_action(fn, ctx, _parameters(job))
            elif job.kind == "llm_generation":
                fn = self.generators[f"{job.provider}/{job.model}"]
                result = await _maybe_await(fn(ctx, GenerationJob(job_id=job.id, spec=job.spec, provider=job.provider, model=job.model)))
            else:
                raise RuntimeError(f"unsupported job kind {job.kind}")
            await self._report(ws, job, "completed", result=result)
        except asyncio.CancelledError:
            await self._report(ws, job, "cancelled", error_type="Cancelled", error_message="job cancelled")
            raise
        except Exception as exc:
            await self._report(ws, job, "failed", error_type="Error", error_message=str(exc))
        finally:
            hb.cancel()

    async def _heartbeat_loop(self, ws: Any, job: WorkerSocketClaimedJob) -> None:
        interval = self.config.heartbeat_interval or job.heartbeat_cadence_seconds
        while True:
            await asyncio.sleep(interval)
            frame = WorkerSocketJobHeartbeatFrame(
                type="job.heartbeat",
                message_id=_msg_id(),
                job_id=job.id,
                lease_token=job.lease_token,
            )
            await ws.send(frame.model_dump_json(exclude_none=True))

    async def _report(
        self,
        ws: Any,
        job: WorkerSocketClaimedJob,
        status: str,
        *,
        result: Any | None = None,
        error_type: str | None = None,
        error_message: str | None = None,
    ) -> None:
        frame = WorkerSocketJobReportFrame(
            type="job.report",
            message_id=_msg_id(),
            job_id=job.id,
            lease_token=job.lease_token,
            status=status,
            result=_result_map(result) if status == "completed" else None,
            error_type=error_type,
            error_message=error_message,
        )
        await ws.send(frame.model_dump_json(exclude_none=True))


@dataclass
class WorkerPoolConfig(WorkerConfig):
    count: int = 1
    worker_instance_id_prefix: str | None = None


class WorkerPool:
    def __init__(self, client: Client, config: WorkerPoolConfig | None = None) -> None:
        self.client = client
        self.config = config or WorkerPoolConfig()
        self.actions: dict[str, ActionFunc] = {}
        self._workers: list[Worker] = []

    def register(self, name: str, fn: ActionFunc) -> WorkerPool:
        self.actions[name] = fn
        return self

    def stop(self) -> None:
        for worker in self._workers:
            worker.stop()

    async def run(self) -> None:
        prefix = self.config.worker_instance_id_prefix or f"worker-{uuid.uuid4()}"
        self._workers = []
        tasks = []
        for i in range(max(1, self.config.count)):
            cfg = WorkerConfig(**{**_worker_config_values(self.config), "worker_instance_id": f"{prefix}-{i + 1}"})
            worker = Worker(self.client, cfg, self.actions)
            self._workers.append(worker)
            tasks.append(worker.run())
        await asyncio.gather(*tasks)


def resolve_instance_id() -> str:
    for value in (
        os.environ.get("HOSTNAME"),
        os.environ.get("FLY_MACHINE_ID"),
        os.environ.get("RAILWAY_REPLICA_ID"),
        os.environ.get("RENDER_INSTANCE_ID"),
    ):
        if value:
            return value
    try:
        host = socket.gethostname()
        if host:
            return f"{host}-{uuid.uuid4().hex[:8]}"
    except Exception:
        pass
    return str(uuid.uuid4())


def _parameters(job: WorkerSocketClaimedJob) -> dict[str, Any]:
    raw = job.spec.get("parameters")
    return raw if isinstance(raw, dict) else {}


def _result_map(result: Any) -> dict[str, Any]:
    return result if isinstance(result, dict) else {"output": result}


async def _maybe_await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


async def _call_action(fn: ActionFunc, ctx: ActionContext, params: dict[str, Any]) -> Any:
    try:
        sig = inspect.signature(fn)
    except (TypeError, ValueError):
        return await _maybe_await(fn(params, ctx))
    positional = [
        p
        for p in sig.parameters.values()
        if p.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
    ]
    if len(positional) == 0:
        return await _maybe_await(fn())
    if len(positional) == 1:
        return await _maybe_await(fn(params))
    if positional[0].name in {"ctx", "context", "action_context"}:
        return await _maybe_await(fn(ctx, params))
    return await _maybe_await(fn(params, ctx))


def _worker_config_values(config: WorkerPoolConfig) -> dict[str, Any]:
    names = {f.name for f in fields(WorkerConfig)}
    return {name: getattr(config, name) for name in names}


def _msg_id() -> str:
    return f"msg_{uuid.uuid4()}"
