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
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any, Callable

from ._api.models import (
    JobClaim,
    JobClaimRequest,
    JobCompleteRequest,
    JobFenceRequest,
    Status as JobStatus,
)
from .client import (
    Client,
    JobEventEntry,
    JobEventsRequest,
    LeaseLostError,
    PayloadTooLargeError,
)
from .errors import AuthRevokedError, RateLimitError

logger = logging.getLogger(__name__)


@dataclass
class WorkerConfig:
    # worker_id is optional — when empty, the Worker constructor fills
    # it with a per-boot UUID so two processes running the same image
    # surface as two distinct rows on the workers page. Set explicitly
    # only for singleton workers that want stable identity across
    # restarts; two processes with the same override collide on one row.
    worker_id: str = ""
    name: str = ""
    version: str = ""
    queues: list[str] = field(default_factory=list)
    actions: list[str] = field(default_factory=list)
    concurrency: int = 10
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
    worker_id: str
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

    def __init__(self, client: Client, config: WorkerConfig) -> None:
        self._client = client
        if not config.worker_id:
            config.worker_id = str(uuid.uuid4())
        self._config = config
        self._actions: dict[str, ActionFunc] = {}
        self._stop_event = threading.Event()
        self._auth_revoked = threading.Event()

    def register(self, name: str, fn: ActionFunc) -> None:
        """Register an action function under the given name."""
        self._actions[name] = fn

    def run(self) -> None:
        """Start the claim loop. Blocks until SIGINT/SIGTERM, stop(), or
        an [AuthRevokedError][deepnoodle.mobius.errors.AuthRevokedError]
        from the server — in the last case the credential is gone and
        the process needs to restart under a rotated credential, so
        we raise out of run() rather than silently looping.
        """
        if threading.current_thread() is threading.main_thread():
            signal.signal(signal.SIGINT, lambda *_: self.stop())
            signal.signal(signal.SIGTERM, lambda *_: self.stop())

        logger.info("worker %s started", self._config.worker_id)
        claim_req = self._build_claim_request()

        # Manage the pool explicitly so we can drop `wait=False,
        # cancel_futures=True` when a credential is revoked mid-flight —
        # pending submissions are dropped immediately, and running jobs
        # see ``ActionContext.cancelled`` flip so they can unwind rather
        # than being joined on by the default ``wait=True`` shutdown.
        pool = ThreadPoolExecutor(max_workers=self._config.concurrency)
        try:
            while not self._stop_event.is_set():
                if self._auth_revoked.is_set():
                    raise AuthRevokedError()
                try:
                    job = self._client.claim_job(claim_req)
                except AuthRevokedError:
                    logger.error("claim rejected: credential revoked")
                    self._auth_revoked.set()
                    raise
                except Exception as exc:
                    logger.error("claim error: %s", exc)
                    time.sleep(2)
                    continue

                if job is None:
                    continue

                pool.submit(self._execute_job, job)
        finally:
            if self._auth_revoked.is_set():
                pool.shutdown(wait=False, cancel_futures=True)
            else:
                pool.shutdown(wait=True)

        logger.info("worker %s stopped", self._config.worker_id)

    def stop(self) -> None:
        """Signal the claim loop to stop after in-flight jobs complete."""
        self._stop_event.set()

    # -------------------------------------------------------------------------

    def _build_claim_request(self) -> JobClaimRequest:
        return JobClaimRequest(
            worker_id=self._config.worker_id,
            worker_name=self._config.name or None,
            worker_version=self._config.version or None,
            queues=list(self._config.queues) or None,
            actions=list(self._config.actions) or None,
            wait_seconds=self._config.poll_wait_seconds,
        )

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
            worker_id=self._config.worker_id,
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
                    worker_id=self._config.worker_id,
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
            worker_id=self._config.worker_id, attempt=job.attempt
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
                    worker_id=self._config.worker_id,
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
                        worker_id=self._config.worker_id,
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
