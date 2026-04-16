from __future__ import annotations

import base64
import json
import logging
import signal
import threading
import time
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
from .client import Client, LeaseLostError

logger = logging.getLogger(__name__)


@dataclass
class WorkerConfig:
    worker_id: str
    name: str = ""
    version: str = ""
    queues: list[str] = field(default_factory=list)
    actions: list[str] = field(default_factory=list)
    concurrency: int = 10
    poll_wait_seconds: int = 20
    # Fallback heartbeat interval used when the server does not advertise one
    # in the claim response. Value is in seconds.
    heartbeat_interval: float = 10.0


# ActionFunc receives JSON-decoded parameters and returns a JSON-serialisable result.
ActionFunc = Callable[[dict[str, Any]], Any]


class Worker:
    """Claims jobs from Mobius and dispatches each to a registered action.

    A *job* is a single action invocation on behalf of a workflow run. The
    backend owns the workflow engine; the worker only has to execute one
    action at a time and report its result.
    """

    def __init__(self, client: Client, config: WorkerConfig) -> None:
        self._client = client
        self._config = config
        self._actions: dict[str, ActionFunc] = {}
        self._stop_event = threading.Event()

    def register(self, name: str, fn: ActionFunc) -> None:
        """Register an action function under the given name."""
        self._actions[name] = fn

    def run(self) -> None:
        """Start the claim loop. Blocks until SIGINT/SIGTERM or stop() is called."""
        if threading.current_thread() is threading.main_thread():
            signal.signal(signal.SIGINT, lambda *_: self.stop())
            signal.signal(signal.SIGTERM, lambda *_: self.stop())

        logger.info("worker %s started", self._config.worker_id)
        claim_req = self._build_claim_request()

        with ThreadPoolExecutor(max_workers=self._config.concurrency) as pool:
            while not self._stop_event.is_set():
                try:
                    task = self._client.claim_task(claim_req)
                except Exception as exc:
                    logger.error("claim error: %s", exc)
                    time.sleep(2)
                    continue

                if task is None:
                    continue

                pool.submit(self._execute_task, task)

        logger.info("worker %s stopped", self._config.worker_id)

    def stop(self) -> None:
        """Signal the claim loop to stop after in-flight tasks complete."""
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

    def _execute_task(self, task: JobClaim) -> None:
        log: logging.LoggerAdapter[logging.Logger] = logging.LoggerAdapter(
            logger,
            {
                "job_id": task.job_id,
                "run_id": task.run_id,
                "step": task.step_name,
                "action": task.action,
                "attempt": task.attempt,
            },
        )
        log.info("job claimed (workflow=%s)", task.workflow_name)

        fn = self._actions.get(task.action)
        if fn is None:
            msg = f"action {task.action!r} not registered on this worker"
            log.error(msg)
            self._fail_task(task, "ActionNotRegistered", msg)
            return

        stop_hb = threading.Event()
        hb_thread = threading.Thread(
            target=self._heartbeat_loop,
            args=(task, stop_hb),
            daemon=True,
        )
        hb_thread.start()

        try:
            result = fn(dict(task.parameters or {}))
        except Exception as exc:
            stop_hb.set()
            hb_thread.join()
            log.error("action failed: %s", exc)
            self._fail_task(task, "Error", str(exc))
            return

        stop_hb.set()
        hb_thread.join()

        result_b64 = (
            base64.b64encode(json.dumps(result).encode()).decode()
            if result is not None
            else None
        )
        try:
            self._client.complete_task(
                task.job_id,
                JobCompleteRequest(
                    worker_id=self._config.worker_id,
                    attempt=task.attempt,
                    status=JobStatus.completed,
                    result_b64=result_b64,
                ),
            )
            log.info("job completed")
        except LeaseLostError:
            log.warning("lease lost during complete")
        except Exception as exc:
            log.error("failed to complete job: %s", exc)

    def _heartbeat_loop(self, task: JobClaim, stop: threading.Event) -> None:
        interval = (
            task.heartbeat_interval_seconds
            if task.heartbeat_interval_seconds
            else self._config.heartbeat_interval
        )
        fence = JobFenceRequest(
            worker_id=self._config.worker_id, attempt=task.attempt
        )
        while not stop.wait(timeout=float(interval)):
            try:
                envelope = self._client.heartbeat_task(task.job_id, fence)
                if envelope.directives.should_cancel:
                    logger.warning(
                        "cancel directive received for job %s", task.job_id
                    )
                    stop.set()
                    return
            except LeaseLostError:
                logger.warning(
                    "lease lost during heartbeat for job %s", task.job_id
                )
                stop.set()
                return
            except Exception as exc:
                logger.error(
                    "heartbeat error for job %s: %s", task.job_id, exc
                )

    def _fail_task(
        self, task: JobClaim, error_type: str, message: str
    ) -> None:
        try:
            self._client.complete_task(
                task.job_id,
                JobCompleteRequest(
                    worker_id=self._config.worker_id,
                    attempt=task.attempt,
                    status=JobStatus.failed,
                    error_type=error_type,
                    error_message=message,
                ),
            )
        except Exception as exc:
            logger.error(
                "failed to report job failure for %s: %s", task.job_id, exc
            )
