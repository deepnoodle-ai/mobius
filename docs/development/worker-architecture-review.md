# Worker / LLM generation / job processing — architecture review

Scope: the Go client-side worker stack in this repo — `mobius/worker.go`,
`worker_pool.go`, `runtime.go`, `hold.go`, `sprite_hold.go`, `transport.go`,
`context.go`, `action/`, and the `mobius worker` CLI entrypoint
(`cmd/mobius/worker.go`, `ollama.go`, `main.go`) — with particular attention
to correct operation inside a Sprites.dev sandbox, including keep-warm
behaviour. Review only; no fixes applied.

A second pass incorporated the Sprites.dev lifecycle, idle-detection, and
Tasks API documentation. Facts from those docs are cited inline where they
confirm or sharpen a finding (notably H2 and H5), and the "Keep-warm modes"
section proposes a configuration design for the three keep-alive behaviours
the product wants: recent-work window, on-demand server wake, and
always-on.

## Implementation status

A third pass implemented the recommendations. Everything client-side in this
repo is now fixed on this branch; line numbers in the findings below refer to
the tree at review time (pre-fix). Per finding:

| Finding | Status |
|---|---|
| H1 signal handling | ✅ `signal.NotifyContext` in `cmd/mobius/main.go` |
| H2 socket deadlines | ✅ write deadlines on every frame, client ping/pong with a rolling read deadline, bounded + ctx-aware registration read |
| H3 report acks | ✅ ack-driven delivery: pending-report map, flush on re-register and on the tick, cleared only by `job.report.ack` |
| H4 shared task name | ✅ per-instance task names (`mobius-worker-<worker_instance_id>`) |
| H5 pause/resume | ✅ client side: wall-clock wake detector drops the socket and re-`poke`s the hold on resume. ❌ still server-side: wake-on-dispatch + reaper tolerance for paused-but-wakeable workers; the Sprite Service definition is still not shipped from this repo |
| M1 slot accounting | ✅ register advertises `Concurrency - len(slots)`; claimed overflow is failed back immediately (`WorkerOverCapacity`) instead of dropped |
| M2 shutdown reports | ✅ bounded shutdown drain (5s) delivers cancelled reports over the still-open socket |
| M3 reconnect policy | ✅ exponential backoff with ±20% jitter capped at 60s; 403 and 404 dials are terminal (`ErrAuthRevoked` / `ErrProjectNotFound`) |
| M4 hold-error race | ✅ error parked on the channel before `cancelRun()` |
| M5 keep-warm telemetry | ✅ `keep-warm:established` only advertised by a real hold (`hold.pins()`); required holds retry 3× in-process before failing closed |
| M6 artifact timeouts | ✅ dedicated transfer client (no whole-exchange timeout; response-header timeout on the transport) for artifact upload/download |
| M7 cancelled status | ✅ cancelled jobs report `status=cancelled`; `job_report_cancelled` contract fixture added (passes in all three SDKs) |
| Keep-warm modes | ✅ `WorkerConfig.KeepWarmWindow` (on-demand / window / forever) + orthogonal `KeepWarmRequired`; CLI `--keep-warm=on-demand\|5m\|forever`; `KeepWarmForLifetime` kept as a deprecated alias |
| Low: lease-derived timing | ✅ claim timeout derived from the registered cadence (2×, 30s floor); user heartbeat interval can only tighten the server cadence |
| Low: Ollama discovery | ✅ restart requirement documented in the flag help (periodic re-list not implemented) |
| Low: emitter thread-safety | ✅ atomic delta sequence |
| Low: generation spec schemas | ❌ not done — `WorkerSocketLLMGenerationSpec/Result` need to be defined in `openapi.yaml` in coordination with the server team, then generated across the three SDKs |
| Low: drain stickiness | ✅ documented as per-connection by design (server re-drains on re-register) |
| Low: lost read error | ✅ closed-channel path now drains `readErr` |
| Low: bash grandchildren | ✅ process-group kill (`Setpgid` + group SIGKILL) and `WaitDelay` on bash/git commands |
| Low: `tailFile` memory | ✅ bounded end-of-file scan (4 MiB) |
| Low: `workspacePath` symlinks | ✅ deepest-existing-ancestor resolution |
| Low: credential exposure | ✅ stated explicitly in SECURITY.md |

New regression tests cover: unacked-report redelivery, shutdown-time
cancelled reports, claimed overflow, register-never-answered, per-instance
hold independence, on-demand window, hold re-establishment after `poke`,
keep-warm config mapping, backoff bounds, and lease-derived claim timeouts.

Overall the design is thoughtful: job execution decoupled from socket
lifetime, a single claim in flight with a response deadline, lease-fenced
frames, a refcounted Sprite hold with a release grace, and fail-closed
lifetime holds are all good calls, and the hardest behaviours have tests.
The findings below are the gaps between that design and what the code does
today, ordered by severity. Line numbers reference the tree at the time of
review.

---

## High severity

### H1. The CLI has no signal handling — graceful shutdown is unreachable

`main.go` calls `app.Execute()`, which runs commands under a bare
`context.Background()` (wonton `cli.go:412`); nothing in the CLI or the
framework installs `signal.NotifyContext`. On SIGTERM/SIGINT the Go runtime
kills the process immediately.

Consequences, all of which matter most in a Sprite:

- The SDK's entire shutdown path — `cancelAllInflight`, `wg.Wait`, the
  keep-warm maintainer's `deleteTask` on context cancel
  (`sprite_hold.go:166-171`) — is dead code for the CLI. It only ever runs
  in SDK embedders that wire their own signal context.
- The Sprite keep-warm task is orphaned and keeps the microVM pinned warm
  (billing) for up to the task expiry (5m) after the worker is gone.
- In-flight jobs die silently: no `Cancelled` report, no `worker.draining`
  frame. The server only finds out via lease expiry / the dead-worker
  reaper, which delays retries by the full lease TTL.

**Recommendation:** wrap execution in `signal.NotifyContext(context.Background(),
os.Interrupt, syscall.SIGTERM)` in `main.go` (or add it to the wonton app if
other CLIs want it). This is a ~5-line change that activates a lot of
carefully written shutdown code. Worth pairing with H2/M2 so shutdown
actually delivers terminal reports.

### H2. No WebSocket write deadlines (or register/dial read bounds) — a stalled TCP connection wedges the worker

`workerSocket.writeJSON` (`runtime.go:63-67`) calls gorilla `WriteJSON` with
no `SetWriteDeadline`, ever. There is also no client-initiated ping/pong or
read deadline; liveness detection rests entirely on the application-level
claim-response timeout.

Failure scenario: the peer stalls (half-open connection after a NAT/LB drop,
or a paused/hibernated network path — precisely the kind of thing that
happens around Sprite pauses). A write then blocks indefinitely *while
holding `s.mu`*:

- the run loop blocks inside `claim()` before `armClaimDeadline()` runs
  (`worker.go:376-384`), so the claim-response timeout never arms;
- `heartbeatLoop` blocks in `sendFrame` on the same mutex, so job
  heartbeats stop;
- nothing times out until the OS TCP stack gives up (minutes to hours).

The worker's `last_seen` stops advancing and Mobius Cloud's reaper fails the
run's jobs as `environment_worker_unavailable`, even though the worker
process is healthy and its jobs are still running.

The registration path has the same shape: `register` does a bare
`socket.conn.ReadMessage()` loop (`worker.go:573-575`) with no deadline and
no ctx awareness — a server handler that accepts the upgrade but never
answers `worker.register` hangs the worker forever (the claim deadline only
covers claims), and ctx cancellation cannot unblock it because
`socket.close()` is deferred until `runSocket` returns.

The Sprites lifecycle docs make this a hot path, not an edge case: *"open
TCP connections drop on the pause, even on warm"*. Every warm wake therefore
resumes the worker process mid-thought on a socket whose remote end is gone
— usually without a FIN/RST ever reaching the guest, so the blocked
`ReadMessage` doesn't return promptly. Recovery today rides on Go's default
TCP keepalive (~2-3 minutes to declare the peer dead) or on the claim
deadline if the 10s ticker happens to get a claim written (~70s). Both eat
directly into the dead-worker reaper's TTL budget on every single wake.

**Recommendation:** set a write deadline inside `writeJSON` (a single
`s.conn.SetWriteDeadline(time.Now().Add(10*time.Second))` before each write)
and a read deadline around registration. A simpler structural alternative:
run all liveness through the standard gorilla ping/pong pattern — client
pings on a ticker, `SetPongHandler` extends a rolling read deadline — which
would let the claim-deadline machinery shrink to "claims are answered
within the long-poll window" and would bound reads and writes uniformly.

### H3. Terminal reports are considered delivered on write success — the `job.report.ack` frame exists but is never used

`deliverReport` (`worker.go:680-699`) retries across reconnects *until a
write succeeds*, then returns. But a successful `WriteJSON` only means the
frame reached the kernel's socket buffer. If the connection drops before the
server processes it (the exact window in which reconnects happen), the
report is lost, the worker believes the job is done, and the slot is freed.
The server eventually fails the job via lease expiry despite the work having
completed — user-visible as a spurious job failure/retry, and for
non-idempotent actions a retry is a double execution.

The protocol already solved this: `WorkerSocketJobReportAckFrame`
(`openapi.yaml`, `job.report.ack`, with a `duplicate` flag for redelivery)
— and the spec's comment in `runtime.go:175-181` even notes reports are
idempotent server-side. The SDK simply never reads the ack (the frame type
falls through the `runSocket` switch silently). `generation.delta.ack` and
`worker.ready` are likewise defined but unused; deltas are declared
best-effort so that's fine, but the report ack is the one that matters.

**Recommendation:** make report delivery ack-driven: keep a small
`pending map[messageID]reportFrame` on the Worker, re-send pending reports
after each re-register, and delete on `job.report.ack`. This *replaces* the
current bespoke park-until-socket-changes loop in `deliverReport` rather
than adding to it — the reconnect-resend logic collapses into "flush
pending on register", which is simpler than what exists today and is what
the `duplicate` flag was designed for.

### H4. All Sprite holds share one task name — pool workers (or two processes) release each other's hold

`spriteTaskName = "mobius-worker"` is a package constant
(`sprite_hold.go:23`) and every `spriteHold` instance PUTs/DELETEs
`/v1/tasks/mobius-worker`. The refcount protecting the task is *per hold
instance*, but the task is effectively global to the Sprite.

Failure scenario: `mobius worker --workers 2` on a Sprite (or any two worker
processes, or a worker plus a future second holder). Each child `Worker`
gets its own `spriteHold` via `detectHold` (`worker.go:154`). Worker A goes
idle, its grace elapses, and its maintainer `deleteTask`s — killing the hold
while worker B is mid-`git clone`. B's next refresh re-creates the task up
to 60s later, but in that window the Sprite is free to pause and freeze B's
job (the exact #1028 wedge the hold exists to prevent). The same applies on
worker A's clean exit (`releaseOnExit`).

**Recommendation:** two options, in order of preference:

1. **Per-instance task names** — `mobius-worker-<worker_instance_id>`.
   Holds become independent; no coordination needed. Costs one task row per
   worker on the Sprite.
2. A process-wide singleton hold shared by all workers in the process
   (moves the refcount up a level) — fixes the pool case but not the
   two-process case.

Given the pool is documented as rare, (1) is both simpler and more correct.

### H5. A paused Sprite is unreachable over the worker socket — waking it must be an explicit server-side action, and the client isn't hardened for the resume path

The Sprites docs settle what was an open question in the first pass, and
the answers make this concrete rather than hypothetical:

- *"Open TCP connections drop on the pause, even on warm."* The worker's
  WebSocket to Mobius Cloud does not survive any pause. `work.available`
  can never reach a paused Sprite over the existing socket — the socket no
  longer exists.
- Outbound connections don't count as wake triggers or idle activity: the
  docs' idle-activity list covers exec/console, *inbound* TCP to the
  Sprite's URL, TTY sessions, and services with open connections — and
  "queue workers waiting on jobs from an external broker" is given as a
  canonical *Tasks API* use case, i.e. an outbound-connected worker will
  pause (~30s idle window) unless a task holds it.
- Wake happens on inbound traffic: a request to `https://<name>.sprites.app`
  or a `sprite exec` / SDK command (100-500ms warm, 1-2s cold).
- A warm wake resumes the worker process mid-thought; a cold wake kills it.
  Only a Sprite **Service** restarts the worker after a cold wake — which
  `worker.go:243-246` already assumes ("Mobius Cloud runs the worker as a
  Sprite Service") but nothing in this repo provisions or documents.

So for any mode where the Sprite is allowed to pause between jobs, the
dispatch contract must be: **Mobius Cloud explicitly wakes the Sprite**
(HTTP poke to its URL or a Sprites-API exec) **before or upon dispatching an
environment-pinned job, and the dead-worker reaper must not fail jobs for a
worker whose Sprite is paused-but-wakeable.** If that server-side wake does
not exist today, every reused (lease/explicit) environment wedges after its
first 2-minute idle grace: jobs fail as `environment_worker_unavailable`
with nothing in the worker logs (it was paused).

The client side then needs to treat resume-from-pause as a first-class,
*common* event rather than an error path:

1. **Assume the socket is dead on every wake** (the docs guarantee it).
   Without H2's deadlines, the worker takes ~70s-3min to notice; after a
   100-500ms warm wake that delay dominates job latency and eats the reaper
   TTL.
2. **Detect the wake directly** instead of waiting for I/O to fail: a
   cheap suspend detector (a 1s ticker comparing wall-clock deltas; a gap
   ≫ the interval means the VM was suspended) should immediately drop the
   socket, reconnect, re-register, re-claim, and re-`ensure` the keep-warm
   hold. That turns wake→first-claim into a sub-second path.
3. **Ship the Service definition.** Document (or emit via the CLI, e.g.
   `mobius worker install-service`) the `sprite-env services create`
   invocation so the cold-wake restart path is a stated part of the design
   rather than an assumption in a comment.

See "Keep-warm modes" below for how this slots into the configuration
surface.

---

## Medium severity

### M1. Slot accounting can silently drop claimed jobs

Two related problems:

- `register` always advertises `available_slots = Concurrency`
  (`worker.go:553-554`), even when reconnecting with jobs still in flight.
  On a reconnect with a full worker, the server is told N slots are free.
- When `jobs.claimed` carries more jobs than there are free slots, the
  excess job is logged and *dropped* (`worker.go:439-445`): it was leased to
  this worker server-side, but the worker never runs it and never reports
  it. It sits in `claimed` until the lease expires — a silent
  lease-TTL-long stall for that run, invisible except for a Warn log.

**Recommendation:** compute registration availability as
`Concurrency - len(slots)` (the data is already there), and treat claimed
overflow as a reportable condition — deliver an immediate failure report
("worker over capacity, requeue") rather than ghosting the job so the
server can requeue in seconds instead of a lease TTL.

### M2. Shutdown always drops terminal reports for cancelled jobs

On worker shutdown, `runJob`'s failure report for a cancelled job is
delivered via `deliverReport(lifeCtx, ...)` where `lifeCtx` is already
cancelled — so it takes the `ctx.Done()` branch and drops every report
(`worker.go:693-695`), and `setSocket(nil)` has usually run first anyway.
The server must rediscover every in-flight job's fate via lease expiry.
Combined with H1 this means a routine Sprite service restart turns all
in-flight jobs into slow `environment_worker_unavailable` failures rather
than fast, clean `Cancelled` reports.

**Recommendation:** implement a bounded drain: on shutdown, keep the socket
open for a grace window (a few seconds), cancel jobs, and deliver their
`Cancelled` reports before closing. This is also the natural place to send
`worker.draining` proactively. Alternatively — simpler — give `deliverReport`
a short independent timeout context (`context.WithTimeout(context.Background(),
5s)`) for the final flush instead of the already-dead lifetime context, the
same trick `deleteTask` already uses (`sprite_hold.go:270-272`).

### M3. Reconnect policy: fixed 2s delay, no backoff/jitter, and non-401 HTTP rejections retry forever

`Run` sleeps a flat `ReconnectDelay` (default 2s) between attempts
(`worker.go:287`). Two issues:

- No exponential backoff or jitter: during a server incident every worker
  in a fleet hammers the endpoint every 2s in lockstep.
- `dialWorkerSocket` treats only 401 as terminal (`worker.go:314-316`). A
  403 (revoked project access), 404 (deleted project / wrong handle), or a
  misconfigured `--api-url` produce an eternal 2-second retry loop with a
  Warn log each time. (Per the Sprites idle-detection docs this outbound
  churn at least doesn't hold the VM awake — outbound connections aren't
  activity — but as a Service the process spins forever with no operator
  signal beyond logs.)

**Recommendation:** exponential backoff with jitter capped at ~30-60s
(there is already `expBackoff` in `transport.go` to reuse), and treat 403/404
on dial as terminal alongside 401.

### M4. Required-hold failure race can mask the real error

In `Run`, the hold goroutine does `cancelRun()` *before* `holdErr <- err`
(`worker.go:228-234`). The main loop, unblocked by the cancellation, calls
`takeHoldError` — a non-blocking receive (`worker.go:296-305`) — which can
run before the send lands, in which case `Run` returns `context.Canceled`
instead of the hold-failure error. The worker exits with a generic
"context canceled" and the operator loses the actionable message (and any
supervisor logic keyed on the error).

**Recommendation:** send to `holdErr` before calling `cancelRun()` — a
two-line reorder. (Or make the final `takeHoldError` calls blocking-with-
timeout, but the reorder is simpler.)

### M5. Keep-warm telemetry lies off-Sprite, and the required hold has no startup retry

- `noopHold.ensure` returns `true` (`hold.go:57`), so a worker started with
  `--keep-warm-for-lifetime` on a non-Sprite host advertises
  `keep-warm:established` (`worker.go:640-649`) — exactly the signal that
  capability was created to make trustworthy ("did the run-scoped worker
  actually pin its Sprite warm?"). An operator checking the worker session
  sees "established" when there is no hold at all.
- In required mode, a single failed PUT at startup (`worker.go:247-252`) or
  a single failed refresh (`sprite_hold.go:197-203`) is fatal. Fail-closed
  is right, but one transient Tasks-API blip → worker exit → Sprite Service
  restart → repeat. A couple of quick in-process retries before giving up
  would cut restart churn without weakening the invariant.

**Recommendation:** make `ensure`/`capabilities` distinguish "no hold
needed" from "hold established" (e.g. only advertise `keep-warm:established`
when the hold is a real `spriteHold`), and wrap the required-mode PUT in a
short retry (3 attempts, 1-2s apart).

### M6. The 60s HTTP client timeout kills large artifact transfers

`NewClient` builds `http.Client{Timeout: 60s}` (`client.go:15,112`), and
`http.Client.Timeout` covers the *entire* exchange including body transfer.
`DownloadArtifactToFile` (default cap 100MB) and the streaming multipart
upload in `createArtifactFromFile` both ride this client
(`environment.go:241,313`). Any artifact that takes >60s to move —
100MB needs a sustained ~14Mbps — fails mid-transfer with a confusing
`context deadline exceeded`. These are exactly the operations the
`environment.artifact.publish/download` worker actions expose to agents.

**Recommendation:** use a transfer-specific client (no overall timeout, but
with dial/TLS/response-header timeouts via the transport) for the two
artifact body paths, or switch those calls to per-request contexts sized to
the payload. The general-purpose 60s default is fine for JSON APIs.

### M7. The report protocol's `cancelled` status is never used

`WorkerSocketJobReportFrame.status` admits `completed|failed|cancelled`, but
the worker maps a cancelled job to `status=failed` with
`error_type="Cancelled"` (`worker.go:778-781`, `classifyError`
`worker.go:865-870`, `failureReportFrame` `runtime.go:198-209`). If the
server (or any dashboard) distinguishes cancellations from failures by
status — which is why the enum value exists — client-cancelled jobs are
misclassified as failures. It also couples semantics to a magic string.

**Recommendation:** emit `status=cancelled` when
`errors.Is(err, context.Canceled)` after a server cancel directive, and keep
`failed` for real errors. (If the server keys off `error_type` today, this
is a spec-fidelity cleanup to coordinate cross-SDK — the contract fixtures
cover `job_report_completed` and `job_report_failed` but have no cancelled
fixture, which is itself a gap.)

---

## Low severity / polish

- **Claim timeout and heartbeat cadence ignore the server's lease config.**
  The server tells the worker its cadence in `worker.registered`
  (`lease.heartbeat_cadence_seconds`, logged at `worker.go:590-593`) and
  per-job (`heartbeat_cadence_seconds`), but `claimResponseTimeout` is a
  fixed 60s constant whose correctness "must sit above the server's
  long-poll window and below the reaper's stale TTL" (`worker.go:113-125`)
  — a cross-service invariant maintained by hope. Similarly a user-set
  `WorkerConfig.HeartbeatInterval` silently overrides the server cadence
  (`worker.go:832-834`) and can exceed the lease TTL. Deriving both from
  the registered lease config (e.g. claim timeout = 2× cadence, heartbeat
  = min(user, cadence)) removes two footguns and a config knob.
- **Ollama model discovery is startup-only** (`cmd/mobius/worker.go:109-120`).
  Models pulled/removed after start are never (de)advertised until restart.
  A periodic re-list + re-register would fix it; at minimum document the
  restart requirement.
- **`GenerationEmitter` thread-safety is unspecified.** The `emit` closure
  does an unsynchronized `seq++` (`worker.go:803-810`); a generator that
  emits from multiple goroutines races. Document single-goroutine use or
  make the counter atomic.
- **`WorkerSocketLLMGenerationSpec`/`...Result` are referenced but not
  defined.** The spec text points at them (`openapi.yaml:6755,6861`) but no
  such schemas exist, so the `{"llm_response": ...}` envelope and the
  `request`/`route`/`mobius` spec nesting live only as prose comments in
  `cmd/mobius/ollama.go:136-148,214-223`. This is the cross-SDK generation
  contract — it should be schemas + contract fixtures like the other frames
  (`internal/testdata/contract/` has no generation-spec/result fixture).
- **Drain is not sticky.** `worker.drain` sets a per-connection flag
  (`worker.go:486-491`); any reconnect (including the claim-deadline one)
  silently resumes claiming. If the server re-drains on re-register this is
  fine — worth a comment; if not, it's a bug.
- **A dropped read error can be lost.** `readSocketFrame` sends the error
  and closes the frame channel; if the select picks the closed channel
  first, `runSocket` returns nil and the log line about *why* the socket
  died never appears (`worker.go:427-429`, `runtime.go:110-136`). Cosmetic,
  but it makes field debugging harder.
- **`environment.bash` children can outlive the timeout.**
  `exec.CommandContext` kills only the direct `bash` process
  (`action/environment.go:166-181`); grandchildren survive and keep running
  in the sandbox. Set `SysProcAttr{Setpgid: true}` and signal the process
  group, and consider `cmd.WaitDelay`.
- **`tailFile` reads the whole log into memory** (`action/environment.go:748-761`)
  before taking the tail — a multi-GB worker log OOMs the action. Seek from
  the end instead.
- **`workspacePath` symlink check only applies to existing targets**
  (`action/environment.go:626-632`): writing through a symlinked *parent*
  directory that points outside the workspace passes the lexical check.
  Resolve the deepest existing ancestor instead.
- **The worker's own credential is visible to sandboxed commands.**
  `environment.bash` inherits `os.Environ()` including `MOBIUS_API_KEY`.
  Inside a single-tenant Sprite that may be intended (the CLI needs it),
  but it deserves an explicit statement in SECURITY.md; `logs.tail` already
  redacts `mbx_` tokens, which suggests the exposure is known.

---

## Keep-warm modes: a configuration design

Product intent (from discussion): the worker should support, driven by
configuration, three keep-alive behaviours on a Sprite —

- **A. Recent-work window** — guaranteed awake while the server has had
  work for it in the last X minutes (e.g. 5m), free to pause after that;
- **B. On-demand** — pause aggressively when idle; the server wakes the
  Sprite (Sprites API / URL poke) when it has work;
- **C. Always-on** — hold the Sprite awake for the worker's whole lifetime.

The key observation is that these are not three mechanisms. They are one
mechanism — the existing refcounted task hold — with three values of a
single parameter: *how long after the last job does the hold persist?*

| Mode | Hold window after last job | Today's equivalent |
|------|---------------------------|--------------------|
| B. On-demand | 0 (job-scoped only) | per-job hold with `releaseGrace` forced to ~0 |
| A. Recent-work | X minutes (config) | per-job hold + `spriteReleaseGrace`, currently hardcoded 2m (`sprite_hold.go:37`) |
| C. Always-on | infinite | `KeepWarmForLifetime` |

**Proposed surface:** replace the `KeepWarmForLifetime bool` +
hardcoded-grace pair with one knob, e.g. `WorkerConfig.KeepWarmWindow
time.Duration` (`0` = on-demand/job-scoped, `>0` = recent-work window,
negative or a sentinel = lifetime), exposed as
`--keep-warm=on-demand|5m|forever` / `MOBIUS_WORKER_KEEP_WARM`. This
*deletes* a mode flag and a constant rather than adding configuration, and
the `spriteHold` maintainer needs no structural change — the grace timer
already implements the window; it just reads the config instead of the
constant. Make fail-closed (`Required`) orthogonal and available in modes A
and C: "guaranteed" in mode A means a failed hold refresh inside the window
should be loud (today per-job/grace holds are best-effort, Warn-only —
`sprite_hold.go:229-242`).

Notes per mode, informed by the Sprites docs:

- **Mode A** — the window today starts when the last job *finishes*
  (`release` arms the grace). That matches "work available in the last X
  minutes" closely enough, since work arriving resets it via the next
  claim's `acquire`. Keep the task expiry (`5m`) ≥ the refresh interval as
  now; the docs' recommended heartbeat pattern (5m expiry, 60s refresh,
  delete on exit, expiry as crash backstop) is exactly what `spriteHold`
  already implements — worth saying explicitly: the wire-level usage (PUT
  upsert, DELETE on release, short expiry) is textbook per the Tasks API
  docs. The one required fix in this layer is per-instance task names (H4).
- **Mode B** — cheapest to run but has hard prerequisites, because the
  ~30s idle window means the Sprite pauses between *every* pair of jobs:
  (1) the server-side wake call on dispatch and reaper tolerance for
  paused workers (H5); (2) client hardening so wake→claim is fast — write
  deadlines (H2) and the suspend detector (H5.2); (3) the worker installed
  as a Sprite Service so cold wakes restart it (H5.3). Job-scoped holds
  must remain even in this mode — a job mid-`git clone` still can't
  survive a pause — and they should arguably be fail-closed here too,
  since a pause mid-job in this mode strands the job until the reaper.
  Until (1)-(3) exist, mode B should not be selectable.
- **Mode C** — works today (`KeepWarmForLifetime`), with the caveats in
  M4/M5. The Tasks API's 1-hour max expiry is comfortably handled by the
  60s refresh cadence. This is the right default for run-scoped
  environments, as the code already documents.

The three-mode framing also resolves the open design tension in
`spriteHold` (see Simplifications #5): the grace machinery isn't an
either/or against lifetime pinning — it's the middle point on the dial, and
all three points share one code path.

---

## Simplifications & alternative approaches

Recurring theme: the worker has grown several bespoke liveness/delivery
mechanisms where the protocol or the standard library already provides one.
Each item below *removes* code or config rather than adding it.

1. **One liveness mechanism instead of three.** Today: app-level
   `keepalive` frames, the claim-response deadline, and (implicitly) the
   connection max-age. Standard WS ping/pong with a rolling read deadline
   (H2) subsumes dead-socket detection; the claim deadline then only guards
   against a wedged-but-alive server handler and could likely be folded
   into it.

2. **Ack-driven report delivery instead of the parked-report loop** (H3).
   The `socketChanged` broadcast channel, the re-lock/re-check loop in
   `deliverReport`, and the "reports are idempotent so re-send is safe"
   comment all exist to approximate what a pending-map flushed on
   re-register does exactly, with the server's `duplicate` flag closing the
   loop.

3. **Derive timing from the server's lease config** instead of the
   `claimResponseTimeout` constant and the `HeartbeatInterval` override
   (see Low). Fewer knobs, and the cross-service invariant becomes
   self-maintaining.

4. **Per-instance Sprite task names** (H4) eliminate the only cross-worker
   coordination problem the hold has, without adding coordination.

5. **Collapse the hold's modes onto one dial.** The hold currently has
   per-job refcounting + release grace + optional lifetime pinning, each
   with its own failure semantics. As laid out in "Keep-warm modes" above,
   these are one policy — *hold while any job is in flight or finished
   within the last W* — with W ∈ {0, X minutes, ∞}. Replacing
   `KeepWarmForLifetime` + the `spriteReleaseGrace` constant with a single
   configurable window removes a bool, a constant, and the special-cased
   lifetime `acquire()` in `Run` (`worker.go:240`), and gives the product
   its requested on-demand / recent-work / always-on switch for free.

6. **Let the generated spec carry the generation contract** (Low): define
   `WorkerSocketLLMGenerationSpec/Result` schemas and generate the Go/TS/Py
   types, replacing the hand-walked `map[string]any` plumbing in
   `ollama.go` (`llmGenerationRequest`, `systemPromptText`, `toInt`,
   `toFloat`) with generated types plus one `decodeJSON` call.

## Test coverage gaps worth closing

Existing coverage is good on claim/reconnect/cancel paths
(`runtime_test.go`) and hold basics (`sprite_hold_test.go`). Missing:

- shutdown-time report delivery (would have caught M2);
- a stalled-write / blocked-`WriteJSON` scenario (H2) — feasible with a
  connected-but-unread test socket;
- pool-on-Sprite hold interference (H4) — two holds against one fake Tasks
  API;
- register-never-answered hang (H2);
- claimed-overflow behaviour asserting what the server sees (M1);
- a `job.report.ack`-based redelivery test once H3 lands;
- a `cancelled`-status contract fixture (M7).
