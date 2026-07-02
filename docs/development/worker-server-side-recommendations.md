# Mobius Cloud (server-side) recommendations for the worker stack

Companion to [worker-architecture-review.md](worker-architecture-review.md).
That review's client-side findings are all fixed in this repo; this document
captures the concrete server-side work needed to close the loop. Each item
says what to build, why the hardened client now depends on it, and how to
verify it. Items are ordered by how directly they block product behaviour.

Context the whole document rests on (from the Sprites.dev lifecycle docs):
**every Sprite pause drops open TCP connections, even a warm pause, and
outbound connections neither hold a Sprite awake nor wake it.** A paused
Sprite's worker socket does not exist; only inbound traffic (a request to
`https://<name>.sprites.app` or a Sprites-API exec) wakes the VM. Everything
the server does for Sprite-hosted workers has to be designed against that
fact.

---

## 1. Wake paused Sprites on dispatch

**What:** when dispatching a job pinned to an environment hosted on a Sprite,
and no live worker socket exists for that environment's worker, explicitly
wake the Sprite before (or concurrently with) making the job claimable:
either an HTTP poke to the Sprite's URL or a Sprites-API exec. Then let the
normal flow run — the woken worker reconnects, re-registers, and claims
(the client now detects resume-from-pause in ~1s and reconnects immediately,
so wake→claim is fast).

**Why:** `work.available` can never reach a paused Sprite — the socket it
would ride is gone. Without server-side wake, any keep-warm mode that lets
the Sprite pause between jobs (the on-demand mode, or a recent-work window
that has elapsed) wedges on the first idle gap: the job sits in `queued`,
the worker sleeps, and the run eventually fails for no visible reason.
This is the single prerequisite gating the on-demand keep-warm mode the
product wants; the client already supports `--keep-warm=on-demand` but warns
it is unusable until this exists.

**Verify:** integration test — worker on a Sprite with a short keep-warm
window, let the Sprite pause, dispatch a job, assert the job is claimed
within a few seconds (warm wake is 100–500ms; the client reconnect adds
~1–2s) rather than failing via the reaper.

## 2. Teach the dead-worker reaper about paused-but-wakeable workers

**What:** before failing a worker's jobs as `environment_worker_unavailable`
because `last_seen` went stale, check whether the worker's environment is a
Sprite that is merely paused (or was recently woken and is reconnecting).
Concretely:

- treat "stale `last_seen` + environment pause is an expected state for this
  worker's keep-warm mode" as *paused*, not *dead*;
- on dispatch to a paused worker, attempt the wake (item 1) and start a
  bounded wake timer (a few seconds warm, ~10s cold-with-Service) before the
  reaper may act;
- only fail jobs when a wake attempt did not produce a re-registered worker
  within that budget.

**Why:** the client can go dark at any moment through no fault of its own —
that is the designed behaviour of the platform it runs on. A reaper that
equates silence with death converts every allowed pause into spurious
`environment_worker_unavailable` job failures with nothing in the worker
logs (it was suspended). Note the worker's keep-warm posture is already
visible to the server: the register frame's `capabilities` carry
`keep-warm:lifetime` / `keep-warm:established`, and it would be natural to
extend that to advertise the configured window (e.g. `keep-warm:window=5m`
or `keep-warm:on-demand`) so the reaper knows which workers are allowed to
sleep. If that label is wanted, add it to the register-frame contract and
the client will advertise it.

**Verify:** unit test on the reaper's decision function: (stale, sprite
paused, mode=on-demand) → wake+wait, not fail; (stale, wake attempted, no
re-register within budget) → fail as today.

## 3. Ship the Sprite Service definition for the worker

**What:** provision the `mobius worker` process as a Sprite **Service**
(restart-on-cold-wake) when Mobius Cloud creates or adopts a Sprite
environment, rather than as a bare process. The service definition should:

- run `mobius worker` with the environment's credential and
  `MOBIUS_WORKER_ENVIRONMENT_ID`;
- set `MOBIUS_WORKER_KEEP_WARM` per the environment's lifetime (item 5);
- restart on exit — the client is deliberately fail-closed in required
  keep-warm modes and exits on unrecoverable conditions, expecting a
  supervisor;
- write logs where the stock `environment.logs.tail` action looks:
  `$MOBIUS_WORKER_LOG_DIR/{worker,worker.stdout,worker.stderr,bootstrap}.log`.

**Why:** a *cold* wake kills every process in the Sprite; only a Service
comes back. The client code has assumed "Mobius Cloud runs the worker as a
Sprite Service" in a comment since the keep-warm work landed, but nothing
provisions it — meaning today a cold wake produces a Sprite with no worker
in it, which no amount of client hardening can fix. This is prerequisite (2)
of the on-demand mode and also the crash-recovery story for modes A/C.

**Verify:** cold-stop a test Sprite (or let it age out), wake it, assert the
worker re-registers without human action.

## 4. Honor the protocol obligations the hardened client now leans on

The client-side fixes made the SDK rely on behaviours the spec already
describes. Confirm each is true in the server implementation (they may all
already be — this is a checklist, not necessarily a work list):

1. **`job.report.ack` on every `job.report`**, echoing the report's
   `message_id`, with `duplicate: true` on redelivery. The client now keeps
   a report pending — and re-sends it after every re-register and every 10s
   tick — until the ack arrives. A server that processes reports but never
   acks turns every completed job into a slow re-send loop for the rest of
   the connection. Also ack reports whose lease has already expired or been
   reclaimed (any terminal disposition), so the client stops re-sending;
   pair it with an `error` frame if the report was rejected.
2. **`status: "cancelled"` accepted and distinguished.** The client now
   reports server-cancelled and shutdown-cancelled jobs with the protocol's
   `cancelled` status (previously `failed` + `error_type: "Cancelled"`).
   The server should record cancellations distinctly (not as failures in
   run metrics or retry accounting) and keep accepting the legacy shape
   from older SDKs during rollout. The cross-SDK fixture
   `internal/testdata/contract/job_report_cancelled.json` pins the wire
   shape.
3. **`error_type: "WorkerOverCapacity"` treated as an immediate requeue
   signal.** When `jobs.claimed` hands a worker more jobs than it has free
   slots (which honest slot accounting should now make rare), the client
   fails the excess job back instantly with this error type. The server
   should requeue such a job immediately without consuming a retry attempt
   or marking the step failed — the job was never executed.
4. **`worker.drain` re-issued on re-register while a drain is in force.**
   The client treats drain as per-connection state by design; a reconnect
   (including the claim-deadline reconnect) resumes claiming unless the
   server re-drains on the new connection.
5. **`lease.heartbeat_cadence_seconds` populated** in `worker.registered`
   and per-job. The client now derives its claim-response timeout as
   2× the registered cadence (30s floor) and treats the cadence as the
   ceiling for job heartbeat intervals. Keep the server's claim long-poll
   window ≤ the cadence, and keep the dead-worker reaper's stale TTL
   comfortably above 2× the cadence, or document the intended constants —
   this is the cross-service invariant the review flagged as "maintained by
   hope".
6. **Worker-socket dial status codes.** The client now exits terminally on
   401/403 (credential revoked / unauthorized) and 404 (project not found)
   instead of retrying forever. Reserve 404 on
   `/v1/projects/{handle}/workers/socket` strictly for "this project does
   not exist" — a routing layer that can return transient 404s during
   deploys would now kill workers. Use 503 for transient unavailability;
   the client backs off and retries it.

## 5. Drive the keep-warm mode from environment configuration

**What:** decide the keep-warm mode server-side, per environment lifetime,
and pass it to the worker at provision time via `MOBIUS_WORKER_KEEP_WARM`
(the CLI accepts `on-demand`, a duration like `5m`, or `forever`; old
boolean values still mean `forever`):

- **run-scoped environments → `forever`** (today's behaviour, now spelled
  as a window value). The Sprite lives exactly as long as the run needs it.
- **reused (lease/explicit) environments → a recent-work window** (e.g.
  `5m`), optionally with `MOBIUS_WORKER_KEEP_WARM_REQUIRED=true` if the
  product wants "guaranteed awake within the window" to be fail-closed.
- **on-demand → only after items 1–3 ship.**

**Why:** the mode is a property of the environment's dispatch pattern, which
the server knows and the worker doesn't. The client deliberately kept a
conservative default (2m window) so nothing changes until the server opts an
environment into a different mode.

**Verify:** provisioning tests asserting the env var lands per environment
kind; the worker's startup log line reports the resolved mode.

## 6. Define the LLM generation contract in the spec

**What:** add `WorkerSocketLLMGenerationSpec` and
`WorkerSocketLLMGenerationResult` schemas to `openapi.yaml`. The spec text
already references both names (the claimed-job `spec` for
`llm_generation` jobs, and the report `result` comment) but the schemas do
not exist. They should capture what the server actually sends and decodes
today:

- spec: the `request` (Anthropic-Messages-shaped model request), `route`,
  and `mobius` nesting the Ollama bridge documents in
  `cmd/mobius/ollama.go`;
- result: the `{"llm_response": <message>}` envelope `DecodeResult` expects.

Then regenerate the three SDKs (`make generate`) and add
generation-spec/result fixtures to `internal/testdata/contract/` so the
shape is contract-tested like every other frame.

**Why:** this is the one worker wire contract that lives only in prose
comments and hand-walked `map[string]any` code. It is cross-SDK (any
customer writing a generation handler in Python/TS reimplements the shape
from comments), and it cannot be fixed client-side because the server owns
the shape. Once the schemas exist, the Go bridge's manual field plumbing
collapses into generated types plus one decode call (review
"Simplifications" #6).

**Verify:** `make generate-check` plus the new contract fixtures passing in
all three SDKs.

## 7. Small spec/protocol cleanups

- **`worker.ready` and `generation.delta.ack`** are defined but unused by
  any SDK. Either give them a purpose (e.g. delta ack for backpressure) or
  remove them from the spec — an unused frame in a hand-implemented
  protocol is a standing invitation for drift.
- **Consider echoing the keep-warm window** back in `worker.registered` if
  the server starts caring about it (item 2), so misconfiguration is
  observable in one place.
- **Admin UI:** surface the `keep-warm:*` capability labels on the workers
  page; they exist precisely so an operator can see, without shelling in,
  whether a run-scoped worker actually pinned its Sprite.

## 8. Suggested server-side acceptance tests

The client's regression suite now covers its half of each contract. The
matching server-side tests worth adding:

1. dispatch-to-paused-Sprite wakes the VM and the job completes (item 1);
2. reaper does not fail jobs for a paused worker before a wake attempt
   (item 2);
3. cold wake restarts the worker via the Service and it re-registers
   (item 3);
4. every accepted/duplicate/expired `job.report` gets an ack echoing
   `message_id` (item 4.1);
5. a `cancelled` report neither increments failure metrics nor consumes a
   retry (item 4.2);
6. a `WorkerOverCapacity` failure requeues immediately without consuming a
   retry attempt (item 4.3);
7. re-register during an active drain re-issues `worker.drain` (item 4.4);
8. worker-socket endpoint never returns 404 for an existing project under
   deploy/failover (item 4.6).
