from __future__ import annotations

import base64
import json
import threading
import time
from types import SimpleNamespace

from deepnoodle.mobius._api.models import JobCompleteRequest, Status
from deepnoodle.mobius.worker import Worker, WorkerConfig, WorkerPool, WorkerPoolConfig


class FakeClient:
    def __init__(self, job) -> None:
        self.job = job
        self.project = "prj_1"
        self.claims = 0
        self.emitted: list[tuple[str, str, dict[str, object]]] = []
        self.completed: list[JobCompleteRequest] = []

    def claim_job(self, _req):
        self.claims += 1
        if self.claims == 1:
            return self.job
        return None

    def heartbeat_job(self, *_args, **_kwargs):
        class Envelope:
            directives = type("Directives", (), {"should_cancel": False})()

        return Envelope()

    def emit_job_events(self, job_id, req):
        for event in req.events:
            self.emitted.append((job_id, event.type, event.payload))

    def complete_job(self, _job_id, req):
        self.completed.append(req)


def test_worker_supports_action_context_emit_event() -> None:
    job = SimpleNamespace(
        job_id="job_1",
        run_id="run_1",
        workflow_name="demo",
        step_name="scrape",
        action="demo.action",
        parameters={"url": "https://example.com"},
        attempt=1,
        queue="default",
        heartbeat_interval_seconds=60,
    )
    client = FakeClient(job)
    worker = Worker(
        client,  # type: ignore[arg-type]
        WorkerConfig(
            worker_instance_id="worker-1",
            poll_wait_seconds=0,
            event_batch_size=10,
        ),
    )

    def action(ctx, params):
        ctx.emit_event("scrape.started", {"url": params["url"]})
        ctx.emit_event("scrape.done", {"pages": 1})
        worker.stop()
        return {"ok": True}

    worker.register("demo.action", action)
    worker.run()

    assert client.emitted == [
        ("job_1", "scrape.started", {"url": "https://example.com"}),
        ("job_1", "scrape.done", {"pages": 1}),
    ]
    assert len(client.completed) == 1
    assert client.completed[0].status == Status.completed
    result = json.loads(base64.b64decode(client.completed[0].result_b64))
    assert result == {"ok": True}


class SequencedClient(FakeClient):
    def __init__(self, jobs) -> None:
        super().__init__(None)
        self.jobs = list(jobs)
        self.lock = threading.Lock()
        self.worker_ids: list[str] = []
        # Capture the new fence + capacity fields too so any future
        # drift in the claim contract surfaces in this test suite.
        self.session_tokens: list[str | None] = []
        self.concurrency_limits: list[int] = []

    def claim_job(self, req):
        with self.lock:
            self.claims += 1
            self.worker_ids.append(req.worker_instance_id)
            self.session_tokens.append(req.worker_session_token)
            self.concurrency_limits.append(req.concurrency_limit)
            if self.jobs:
                return self.jobs.pop(0)
        return None


def _block_job(job_id: str):
    return SimpleNamespace(
        job_id=job_id,
        run_id="run_1",
        workflow_name="demo",
        step_name="scrape",
        action="demo.block",
        parameters={},
        attempt=1,
        queue="default",
        heartbeat_interval_seconds=60,
    )


def test_worker_claims_next_job_only_after_current_completes() -> None:
    client = SequencedClient([_block_job("job_1")])
    worker = Worker(
        client,  # type: ignore[arg-type]
        WorkerConfig(worker_instance_id="worker-1", poll_wait_seconds=0),
    )
    started = threading.Event()
    release = threading.Event()

    def action(_params):
        started.set()
        release.wait(timeout=2)
        worker.stop()
        return {"ok": True}

    worker.register("demo.block", action)
    thread = threading.Thread(target=worker.run)
    thread.start()
    assert started.wait(timeout=2)
    time.sleep(0.05)
    assert client.claims == 1
    release.set()
    thread.join(timeout=2)
    assert not thread.is_alive()


def test_worker_pool_uses_distinct_single_job_workers() -> None:
    client = SequencedClient([
        _block_job("job_1"),
        _block_job("job_2"),
        _block_job("job_3"),
    ])
    pool = WorkerPool(
        client,  # type: ignore[arg-type]
        WorkerPoolConfig(
            worker_instance_id_prefix="pool-worker",
            count=3,
            poll_wait_seconds=0,
        ),
    )
    started = threading.Barrier(4)
    release = threading.Event()

    def action(_params):
        started.wait(timeout=2)
        release.wait(timeout=2)
        return {"ok": True}

    pool.register("demo.block", action)
    thread = threading.Thread(target=pool.run)
    thread.start()
    started.wait(timeout=2)
    assert set(client.worker_ids[:3]) == {
        "pool-worker-1",
        "pool-worker-2",
        "pool-worker-3",
    }
    # Each pool child generates its own per-boot session token; three
    # children → three distinct tokens, all non-empty.
    tokens = client.session_tokens[:3]
    assert all(tok for tok in tokens)
    assert len(set(tokens)) == 3
    # concurrency_limit defaults to 1 per child unless overridden.
    assert client.concurrency_limits[:3] == [1, 1, 1]
    release.set()
    pool.stop()
    thread.join(timeout=2)
    assert not thread.is_alive()
