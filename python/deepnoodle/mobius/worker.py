from __future__ import annotations

import base64
import inspect
import json
import logging
import queue
import signal
import threading
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable

from ._api.models import (
    JobClaim,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    Status as JobStatus,
)
from ._instance import resolve_instance_id
from .client import (
    Client,
    JobEventEntry,
    JobEventsRequest,
    LeaseLostError,
    PayloadTooLargeError,
)
from .errors import AuthRevokedError, RateLimitError, WorkerInstanceConflictError

logger = logging.getLogger(__name__)


@dataclass
class WorkerConfig:
    # worker_instance_id identifies this worker process and is the row
    # key used by the saturation views in the admin UI. Optional —
    # when empty the SDK auto-detects the platform-native identifier
    # (Cloud Run revision instance, Kubernetes HOSTNAME, Fly machine,
    # Railway replica, Render instance, OS hostname) and falls back
    # to a per-boot UUID. Set explicitly only for stable singleton
    # workers; two live processes using the same override in the
    # same project will collide and the second will fail with
    # WorkerInstanceConflictError.
    worker_instance_id: str = ""
    # Maximum number of jobs this worker holds in flight simultaneously.
    # Defaults to 1; raise to claim several jobs from one worker process
    # while still surfacing as a single row on the workers page.
    concurrency: int = 1
    name: str = ""
    version: str = ""
    queues: list[str] = field(default_factory=list)
    actions: list[str] = field(default_factory=list)
    poll_wait_seconds: int = 20
    # Fallback heartbeat interval used when the server does not advertise one
    # in the claim response. Value is in seconds.
    heartbeat_interval: float = 10.0
    # Max buffered custom events per in-flight job before we drop the oldest.
    event_queue_size: int = 256
    # Max custom events to send in one HTTP request.
    event_batch_size: int = 20


# ActionFunc receives JSON-decoded parameters and returns a JSON-serialisable result.
ActionFunc = Callable[[dict[str, Any]], Any]


@dataclass
class ActionContext:
    job_id: str
    run_id: str
    project_id: str | None
    worker_instance_id: str
    attempt: int
    queue: str | None
    workflow_name: str | None
    step_name: str | None
    action: str | None
    _event_queue: "queue.Queue[JobEventEntry]"
    # Set when the worker needs the action to exit early (currently only
    # on mid-flight credential revocation, since Python threads can't be
    # pre-empted). Well-behaved long-running actions should poll
    # ``ctx.cancelled.is_set()`` and return promptly when it becomes True.
    cancelled: threading.Event = field(default_factory=threading.Event)

    def emit_event(self, type: str, data: dict[str, Any]) -> None:
        event = JobEventEntry(type=type, payload=data)
        try:
            self._event_queue.put_nowait(event)
            return
        except queue.Full:
            pass

        try:
            self._event_queue.get_nowait()
        except queue.Empty:
            pass
        try:
            self._event_queue.put_nowait(event)
        except queue.Full:
            logger.warning("dropping custom event for job %s", self.job_id)


class Worker:
    """Claims jobs from Mobius and dispatches each to a registered action.

    A *job* is a single action invocation on behalf of a workflow run. The
    backend owns the workflow engine; the worker only has to execute one
    action at a time and report its result.
    """

    def __init__(
        self,
        client: Client,
        config: WorkerConfig,
        actions: dict[str, ActionFunc] | None = None,
    ) -> None:
        self._client = client
        resolved, source = resolve_instance_id(config.worker_instance_id or None)
        config.worker_instance_id = resolved
        logger.info(
            "mobius worker: instance id %s (source: %s)", resolved, source
        )
        if config.concurrency <= 0:
            config.concurrency = 1
        self._config = config
        self._session_token = str(uuid.uuid4())
        self._actions: dict[str, ActionFunc] = actions if actions is not None else {}
        self._stop_event = threading.Event()
        self._auth_revoked = threading.Event()
        self._slots = threading.BoundedSemaphore(config.concurrency)

    def register(self, name: str, fn: ActionFunc) -> None:
        """Register an action function under the given name."""
        self._actions[name] = fn

    def run(self) -> None:
        """Start the claim loop. Blocks until SIGINT/SIGTERM, stop(), an
        [AuthRevokedError][deepnoodle.mobius.errors.AuthRevokedError]
        from the server, or a
        [WorkerInstanceConflictError][deepnoodle.mobius.errors.WorkerInstanceConflictError]
        rejection from claim. Conflict crashes loudly so the operator
        notices the duplicate worker_instance_id rather than the worker
        polling into a black hole.

        With ``concurrency > 1`` the worker holds up to N jobs in flight
        simultaneously, all reporting the same ``worker_instance_id`` and
        session token; a per-job thread runs each action.
        """
        if threading.current_thread() is threading.main_thread():
            signal.signal(signal.SIGINT, lambda *_: self.stop())
            signal.signal(signal.SIGTERM, lambda *_: self.stop())

        logger.info("worker %s started", self._config.worker_instance_id)
        claim_req = self._build_claim_request()
        in_flight: list[threading.Thread] = []

        while not self._stop_event.is_set():
            if self._auth_revoked.is_set():
                raise AuthRevokedError()
            # Block until a slot is free; this is what makes
            # concurrency=N a hard ceiling on in-flight jobs.
            if not self._slots.acquire(timeout=0.5):
                in_flight = [t for t in in_flight if t.is_alive()]
                continue
            try:
                job = self._client.claim_job(claim_req)
            except AuthRevokedError:
                self._slots.release()
                logger.error("claim rejected: credential revoked")
                self._auth_revoked.set()
                raise
            except WorkerInstanceConflictError:
                self._slots.release()
                logger.error(
                    "claim rejected: worker_instance_id %s is already in use",
                    self._config.worker_instance_id,
                )
                raise
            except Exception as exc:
                self._slots.release()
                logger.error("claim error: %s", exc)
                time.sleep(2)
                continue

            if job is None:
                self._slots.release()
                continue

            thread = threading.Thread(
                target=self._run_job_with_slot, args=(job,), daemon=True
            )
            thread.start()
            in_flight.append(thread)
            in_flight = [t for t in in_flight if t.is_alive()]

        for t in in_flight:
            t.join()

        logger.info("worker %s stopped", self._config.worker_instance_id)

    def stop(self) -> None:
        """Signal the claim loop to stop after in-flight jobs complete."""
        self._stop_event.set()

    # -------------------------------------------------------------------------

    def _build_claim_request(self) -> JobClaimRequest:
        return JobClaimRequest(
            worker_instance_id=self._config.worker_instance_id,
            worker_session_token=self._session_token,
            concurrency_limit=self._config.concurrency,
            worker_name=self._config.name or None,
            worker_version=self._config.version or None,
            queues=list(self._config.queues) or None,
            actions=list(self._config.actions) or None,
            wait_seconds=self._config.poll_wait_seconds,
        )

    def _run_job_with_slot(self, job: JobClaim) -> None:
        try:
            self._execute_job(job)
        finally:
            self._slots.release()

    def _execute_job(self, job: JobClaim) -> None:
        log: logging.LoggerAdapter[logging.Logger] = logging.LoggerAdapter(
            logger,
            {
                "job_id": job.job_id,
                "run_id": job.run_id,
                "step": job.step_name,
                "action": job.action,
                "attempt": job.attempt,
            },
        )
        log.info("job claimed (workflow=%s)", job.workflow_name)

        fn = self._actions.get(job.action)
        if fn is None:
            msg = f"action {job.action!r} not registered on this worker"
            log.error(msg)
            self._fail_job(job, "ActionNotRegistered", msg)
            return

        stop_hb = threading.Event()
        event_queue: "queue.Queue[JobEventEntry]" = queue.Queue(
            maxsize=self._config.event_queue_size
        )
        stop_events = threading.Event()
        ctx = ActionContext(
            job_id=job.job_id,
            run_id=job.run_id,
            project_id=self._client.project,
            worker_instance_id=self._config.worker_instance_id,
            attempt=job.attempt,
            queue=job.queue,
            workflow_name=job.workflow_name,
            step_name=job.step_name,
            action=job.action,
            _event_queue=event_queue,
        )
        hb_thread = threading.Thread(
            target=self._heartbeat_loop,
            args=(job, stop_hb, ctx.cancelled),
            daemon=True,
        )
        event_thread = threading.Thread(
            target=self._event_loop,
            args=(job, event_queue, stop_events),
            daemon=True,
        )
        hb_thread.start()
        event_thread.start()

        try:
            result = self._invoke_action(fn, ctx, dict(job.parameters or {}))
        except Exception as exc:
            stop_hb.set()
            stop_events.set()
            hb_thread.join()
            event_thread.join()
            log.error("action failed: %s", exc)
            self._fail_job(job, "Error", str(exc))
            return

        stop_hb.set()
        stop_events.set()
        hb_thread.join()
        event_thread.join()

        result_b64 = (
            base64.b64encode(json.dumps(result).encode()).decode()
            if result is not None
            else None
        )
        try:
            self._client.complete_job(
                job.job_id,
                JobCompleteRequest(
                    worker_instance_id=self._config.worker_instance_id,
                    worker_session_token=self._session_token,
                    attempt=job.attempt,
                    status=JobStatus.completed,
                    result_b64=result_b64,
                ),
            )
            log.info("job completed")
        except AuthRevokedError:
            log.warning("complete: credential revoked; worker will exit")
            self._auth_revoked.set()
        except LeaseLostError:
            log.warning("lease lost during complete")
        except Exception as exc:
            log.error("failed to complete job: %s", exc)

    def _heartbeat_loop(
        self,
        job: JobClaim,
        stop: threading.Event,
        cancelled: threading.Event,
    ) -> None:
        interval = (
            job.heartbeat_interval_seconds
            if job.heartbeat_interval_seconds
            else self._config.heartbeat_interval
        )
        fence = JobFenceRequest(
            worker_instance_id=self._config.worker_instance_id,
            worker_session_token=self._session_token,
            attempt=job.attempt,
        )
        while not stop.wait(timeout=float(interval)):
            try:
                envelope = self._client.heartbeat_job(job.job_id, fence)
                if envelope.directives.should_cancel:
                    logger.warning(
                        "cancel directive received for job %s", job.job_id
                    )
                    stop.set()
                    return
            except AuthRevokedError:
                logger.warning(
                    "heartbeat: credential revoked for job %s; cancelling action",
                    job.job_id,
                )
                self._auth_revoked.set()
                cancelled.set()
                stop.set()
                return
            except LeaseLostError:
                logger.warning(
                    "lease lost during heartbeat for job %s", job.job_id
                )
                stop.set()
                return
            except Exception as exc:
                logger.error(
                    "heartbeat error for job %s: %s", job.job_id, exc
                )

    def _fail_job(
        self, job: JobClaim, error_type: str, message: str
    ) -> None:
        try:
            self._client.complete_job(
                job.job_id,
                JobCompleteRequest(
                    worker_instance_id=self._config.worker_instance_id,
                    worker_session_token=self._session_token,
                    attempt=job.attempt,
                    status=JobStatus.failed,
                    error_type=error_type,
                    error_message=message,
                ),
            )
        except AuthRevokedError:
            logger.warning(
                "fail: credential revoked for job %s; worker will exit",
                job.job_id,
            )
            self._auth_revoked.set()
        except Exception as exc:
            logger.error(
                "failed to report job failure for %s: %s", job.job_id, exc
            )

    def _event_loop(
        self,
        job: JobClaim,
        event_queue: "queue.Queue[JobEventEntry]",
        stop: threading.Event,
    ) -> None:
        while not stop.is_set() or not event_queue.empty():
            try:
                first = event_queue.get(timeout=0.25)
            except queue.Empty:
                continue

            batch = [first]
            while len(batch) < self._config.event_batch_size:
                try:
                    batch.append(event_queue.get_nowait())
                except queue.Empty:
                    break

            try:
                self._client.emit_job_events(
                    job.job_id,
                    JobEventsRequest(
                        worker_instance_id=self._config.worker_instance_id,
                        worker_session_token=self._session_token,
                        attempt=job.attempt,
                        events=batch,
                    ),
                )
            except LeaseLostError:
                logger.warning(
                    "lease lost during custom-event emit for job %s",
                    job.job_id,
                )
                return
            except PayloadTooLargeError as exc:
                logger.warning("%s", exc)
            except RateLimitError as exc:
                logger.warning("%s", exc)
            except Exception as exc:
                logger.warning(
                    "failed to emit custom event batch for job %s: %s",
                    job.job_id,
                    exc,
                )

    def _invoke_action(
        self,
        fn: ActionFunc,
        ctx: ActionContext,
        params: dict[str, Any],
    ) -> Any:
        positional = [
            p
            for p in inspect.signature(fn).parameters.values()
            if p.kind
            in (
                inspect.Parameter.POSITIONAL_ONLY,
                inspect.Parameter.POSITIONAL_OR_KEYWORD,
            )
        ]
        if len(positional) >= 2:
            return fn(ctx, params)  # type: ignore[misc]
        return fn(params)


@dataclass
class WorkerPoolConfig(WorkerConfig):
    """Configures an in-process pool of worker instances.

    Most callers do not need a pool. To run several jobs from one
    process, set :attr:`WorkerConfig.concurrency` on a single
    :class:`Worker` and the admin UI will show one row with a
    saturation bar. Reach for a pool only when each child should
    surface as its own row — for independent draining or in-flight
    isolation.
    """

    count: int = 1
    worker_instance_id_prefix: str = ""


class WorkerPool:
    """Runs multiple worker instances in one process.

    Prefer :attr:`WorkerConfig.concurrency` on a single :class:`Worker`
    for raw throughput. Use this only when each child must surface as
    its own row on the workers page.
    """

    def __init__(self, client: Client, config: WorkerPoolConfig) -> None:
        self._client = client
        self._config = config
        if self._config.count <= 0:
            self._config.count = 1
        if not self._config.worker_instance_id_prefix:
            self._config.worker_instance_id_prefix = f"worker-{uuid.uuid4()}"
        self._actions: dict[str, ActionFunc] = {}
        self._workers: list[Worker] = []
        self._auth_revoked = threading.Event()

    def register(self, name: str, fn: ActionFunc) -> None:
        """Register an action function for every worker in the pool."""
        self._actions[name] = fn

    def run(self) -> None:
        """Start all workers and block until stop() or credential revocation."""
        if threading.current_thread() is threading.main_thread():
            signal.signal(signal.SIGINT, lambda *_: self.stop())
            signal.signal(signal.SIGTERM, lambda *_: self.stop())

        errors: list[BaseException] = []
        lock = threading.Lock()
        self._workers = []

        def run_worker(worker: Worker) -> None:
            try:
                worker.run()
            except AuthRevokedError as exc:
                self._auth_revoked.set()
                self.stop()
                with lock:
                    errors.append(exc)
            # Catch process-level interruption exceptions here so one worker
            # can still trigger an orderly pool-wide stop before run() returns.
            except BaseException as exc:
                self.stop()
                with lock:
                    errors.append(exc)

        threads: list[threading.Thread] = []
        for i in range(1, self._config.count + 1):
            worker = Worker(
                self._client,
                WorkerConfig(
                    worker_instance_id=f"{self._config.worker_instance_id_prefix}-{i}",
                    concurrency=self._config.concurrency,
                    name=self._config.name,
                    version=self._config.version,
                    queues=list(self._config.queues),
                    actions=list(self._config.actions),
                    poll_wait_seconds=self._config.poll_wait_seconds,
                    heartbeat_interval=self._config.heartbeat_interval,
                    event_queue_size=self._config.event_queue_size,
                    event_batch_size=self._config.event_batch_size,
                ),
                self._actions,
            )
            self._workers.append(worker)
            thread = threading.Thread(target=run_worker, args=(worker,))
            thread.start()
            threads.append(thread)

        for thread in threads:
            thread.join()

        if self._auth_revoked.is_set():
            raise AuthRevokedError()
        if errors:
            raise errors[0]

    def stop(self) -> None:
        """Signal every worker in the pool to stop after in-flight jobs complete."""
        for worker in self._workers:
            worker.stop()
