from __future__ import annotations

import base64
import json
from types import SimpleNamespace

from deepnoodle.mobius._api.models import JobCompleteRequest, Status
from deepnoodle.mobius.worker import Worker, WorkerConfig


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
            worker_id="worker-1",
            concurrency=1,
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
