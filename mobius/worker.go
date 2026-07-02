package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultWorkerReconnectDelay = 2 * time.Second

// maxWorkerReconnectDelay caps the exponential reconnect backoff so a worker
// riding out a long server incident still probes about once a minute.
const maxWorkerReconnectDelay = 60 * time.Second

// healthySocketAge is how long a registered connection must have lived for the
// reconnect backoff to reset. Registration alone is not enough: a server that
// crashes right after registering every connection would otherwise keep the
// whole fleet hammering at the base delay.
const healthySocketAge = 30 * time.Second

// defaultClaimResponseTimeout bounds how long an outstanding jobs.claim may go
// unanswered before the worker abandons the socket and reconnects. It is
// replaced by a value derived from the server's lease config on registration
// (see register); this default only governs the window before the first
// worker.registered arrives, or servers that report no cadence.
const defaultClaimResponseTimeout = 60 * time.Second

// shutdownReportDrain bounds how long a stopping worker keeps its socket open
// so cancelled jobs can unwind and deliver their terminal reports. Without
// this window every shutdown turns in-flight jobs into slow lease-expiry
// failures server-side instead of fast, clean cancelled reports.
const shutdownReportDrain = 5 * time.Second

// wakeGapThreshold is the wall-clock gap past which the worker assumes its
// host was suspended (e.g. a Sprite microVM pause). Per the Sprites lifecycle
// contract, open TCP connections drop on every pause — even a warm one — so
// on resume the socket is dead but the guest may not learn that for minutes
// (no FIN ever arrives). The wake watcher turns that into an immediate
// reconnect instead.
const wakeGapThreshold = 10 * time.Second

// Keep-warm window sentinels for [WorkerConfig.KeepWarmWindow].
const (
	// DefaultKeepWarmWindow is the keep-warm window used when
	// WorkerConfig.KeepWarmWindow is zero: long enough to bridge an agent
	// step's inter-tool-call think-time, short enough to bound idle billing.
	DefaultKeepWarmWindow = 2 * time.Minute
	// KeepWarmOnDemand releases the environment hold the instant the last
	// in-flight job finishes, letting the host pause between jobs. Requires
	// the server to wake the environment when dispatching new work — a paused
	// host is unreachable over the worker socket.
	KeepWarmOnDemand time.Duration = -1
	// KeepWarmForever pins the environment hold for the worker's entire
	// lifetime. The right choice for run-scoped environments, whose agent
	// steps have idle think-time between tool-call jobs.
	KeepWarmForever time.Duration = math.MaxInt64
)

// ModelCapability advertises one customer-managed model a worker can serve.
type ModelCapability struct {
	Provider string
	Model    string
}

// GenerationJob is the action-facing view of an LLM generation job.
type GenerationJob struct {
	JobID       string
	RunID       string
	SessionID   string
	AgentTurnID string
	ToolCallID  string
	Provider    string
	Model       string
	Spec        map[string]any
}

// GenerationEmitter streams best-effort model deltas back to Mobius Cloud.
// It is safe to call from multiple goroutines.
type GenerationEmitter func(delta map[string]any) error

// GenerationFunc executes one worker-backed LLM generation and returns the
// terminal generation result. Use the emitter for token deltas; the terminal
// result remains authoritative.
type GenerationFunc func(ctx Context, job GenerationJob, emit GenerationEmitter) (map[string]any, error)

// WorkerConfig configures a Worker.
type WorkerConfig struct {
	WorkerInstanceID string
	Concurrency      int
	Name             string
	Version          string
	EnvironmentID    string
	Queues           []string
	Actions          []string
	Models           []ModelCapability
	ReconnectDelay   time.Duration
	// HeartbeatInterval, when set, lowers the per-job heartbeat cadence below
	// the server-provided one. It can only tighten the cadence: a value above
	// the server's would silently outlive the job lease, so the effective
	// interval is min(HeartbeatInterval, server cadence).
	HeartbeatInterval time.Duration
	Logger            *slog.Logger

	// KeepWarmWindow controls how long the environment keep-warm hold (e.g. a
	// Sprites.dev task) persists after the last in-flight job finishes. It is
	// the single dial between the keep-warm modes:
	//
	//   KeepWarmOnDemand   release immediately; the host may pause between jobs
	//                      (requires server-side wake on dispatch)
	//   0                  DefaultKeepWarmWindow (2m)
	//   >0                 stay warm for this window after the last job, so
	//                      work arriving within it never finds a paused host
	//   KeepWarmForever    pin for the worker's whole lifetime (run-scoped
	//                      environments)
	KeepWarmWindow time.Duration

	// KeepWarmRequired makes a windowed keep-warm hold fail-closed: a hold
	// that cannot be established or refreshed (after in-process retries)
	// stops the worker so its supervisor restarts it, instead of silently
	// letting the environment pause mid-work. Forever mode is always
	// fail-closed regardless of this flag.
	KeepWarmRequired bool

	// KeepWarmForLifetime pins the environment keep-warm hold for the worker's
	// entire lifetime.
	//
	// Deprecated: set KeepWarmWindow to [KeepWarmForever] instead. This flag
	// is equivalent to that and is honored when KeepWarmWindow is unset.
	KeepWarmForLifetime bool
}

// keepWarmLifetime reports whether the keep-warm hold is pinned for the
// worker's whole lifetime (forever mode).
func (c WorkerConfig) keepWarmLifetime() bool {
	return c.KeepWarmWindow == KeepWarmForever
}

// keepWarmRequired reports whether keep-warm hold failures are fatal.
func (c WorkerConfig) keepWarmRequired() bool {
	return c.keepWarmLifetime() || c.KeepWarmRequired
}

// effectiveKeepWarmWindow maps the configured window onto the hold's release
// grace: 0 means the default window, negative means release immediately.
// Forever mode never releases (the worker pins a baseline acquire), so the
// grace value is irrelevant there.
func (c WorkerConfig) effectiveKeepWarmWindow() time.Duration {
	switch {
	case c.KeepWarmWindow == 0:
		return DefaultKeepWarmWindow
	case c.KeepWarmWindow < 0:
		return 0
	default:
		return c.KeepWarmWindow
	}
}

// Worker connects to Mobius Cloud over the worker WebSocket, claims jobs,
// executes registered local actions or model generations, and reports results.
//
// Job execution is decoupled from the socket lifetime. A claimed job runs
// under the worker's lifetime context (not the connection's), keeps a slot
// reserved, and sends its heartbeat and terminal report through whatever
// socket is currently live. So when the connection blips, in-flight jobs keep
// running, heartbeats resume on reconnect, and the terminal report is
// delivered over the new socket — rather than being abandoned in `claimed`.
type Worker struct {
	client     *Client
	config     WorkerConfig
	registry   *ActionRegistry
	generators map[string]GenerationFunc
	// registeredModels accumulates the concrete provider/model pairs passed to
	// RegisterGenerator (wildcards excluded). They are merged with config.Models
	// when advertising so a registered generator is always announced — the
	// advertised set and the handler set cannot drift.
	registeredModels []ModelCapability

	sessionToken string
	authRevoked  atomic.Bool
	// socketRegistered records that the current connection completed
	// registration; Run reads it to reset the reconnect backoff.
	socketRegistered atomic.Bool

	// keepWarm holds the worker's environment in an active state while jobs are
	// in flight (e.g. a Sprite microVM); a no-op on hosts that don't pause.
	keepWarm hold
	// keepWarmEstablished records that the lifetime keep-warm hold was
	// successfully established at startup. It is reported to Mobius Cloud as a
	// worker capability so operators can confirm — without shelling into the
	// environment — that a run-scoped worker actually pinned its Sprite warm
	// (vs. the env var being dropped or the hold silently failing). Only a
	// real environment hold sets it: a worker running off-Sprite has nothing
	// to establish and must not claim otherwise.
	keepWarmEstablished atomic.Bool
	// holdRetryDelay separates the startup attempts to establish a required
	// hold; a field so tests can shrink it.
	holdRetryDelay time.Duration

	// Job lifecycle state that survives socket reconnects.
	slots     chan struct{} // capacity = Concurrency; one token per in-flight job
	slotFreed chan struct{} // nudges the run loop to claim after a job finishes
	wg        sync.WaitGroup

	// claimResponseTimeout bounds how long an outstanding jobs.claim may go
	// unanswered before the worker treats the socket as a dead work channel and
	// reconnects. The server answers a claim within its long-poll window (the
	// heartbeat cadence, on the order of tens of seconds) even when idle —
	// returning an empty jobs.claimed — so a claim that goes unanswered well past
	// that means the frame or its response was lost, or a server-side connection
	// handler wedged. Without this bound a single lost claim/response strands the
	// worker: claimOutstanding stays set, the periodic ticker and work.available
	// both no-op, no further claims are sent, the worker's last_seen stops
	// advancing, and Mobius Cloud's dead-worker reaper fails the run's pending
	// jobs as environment_worker_unavailable. Must sit above the server's
	// long-poll window and below that reaper's stale TTL — so register derives
	// it from the lease config's heartbeat cadence rather than trusting the
	// default to straddle both.
	claimResponseTimeout time.Duration

	mu            sync.Mutex
	currentSocket *workerSocket     // the live socket, or nil while disconnected
	socketChanged chan struct{}     // closed+replaced whenever currentSocket changes
	inflight      map[string]func() // jobID -> cancel for in-flight jobs
	// pendingReports holds terminal job reports (keyed by message id) from
	// write until the server acknowledges them with job.report.ack. Reports
	// are idempotent server-side and the ack carries a `duplicate` flag, so
	// re-sending after a reconnect (or on the periodic tick) is safe — and
	// required: a write that reached the kernel's socket buffer proves
	// nothing about the server having processed it.
	pendingReports map[string]api.WorkerSocketJobReportFrame
}

func (c *Client) NewWorker(cfg WorkerConfig) *Worker {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	resolved, source := ResolveInstanceID(cfg.WorkerInstanceID)
	cfg.WorkerInstanceID = resolved
	cfg.Logger.Info("mobius worker: instance id resolved",
		"worker_instance_id", resolved,
		"source", string(source),
	)
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = defaultWorkerReconnectDelay
	}
	if cfg.KeepWarmForLifetime && cfg.KeepWarmWindow == 0 {
		cfg.KeepWarmWindow = KeepWarmForever
	}
	return &Worker{
		client:     c,
		config:     cfg,
		registry:   NewActionRegistry(),
		generators: map[string]GenerationFunc{},
		// The Sprite task name is scoped per worker instance so two workers on
		// one Sprite (a pool, or two processes) can't release each other's hold.
		keepWarm:             detectHold(cfg.Logger, spriteTaskPrefix+"-"+cfg.WorkerInstanceID, cfg.effectiveKeepWarmWindow()),
		holdRetryDelay:       time.Second,
		slots:                make(chan struct{}, cfg.Concurrency),
		slotFreed:            make(chan struct{}, cfg.Concurrency),
		socketChanged:        make(chan struct{}),
		inflight:             map[string]func(){},
		pendingReports:       map[string]api.WorkerSocketJobReportFrame{},
		claimResponseTimeout: defaultClaimResponseTimeout,
	}
}

func (c *Client) newWorkerWithRegistry(cfg WorkerConfig, registry *ActionRegistry) *Worker {
	w := c.NewWorker(cfg)
	if registry != nil {
		w.registry = registry
	}
	return w
}

// RegisterGenerator registers a local model-generation handler. Use model "*"
// to match any model for the provider.
//
// A concrete provider/model is automatically advertised to Mobius Cloud (as if
// added to WorkerConfig.Models), so it shows up in the worker-model catalog and
// is eligible for routing — registering a handler and advertising a model can't
// fall out of sync. A "*" wildcard has no concrete model id to advertise, so
// pair it with an explicit WorkerConfig.Models entry to make it routable.
func (w *Worker) RegisterGenerator(provider, model string, fn GenerationFunc) {
	if provider == "" || model == "" || fn == nil {
		panic("mobius: RegisterGenerator requires provider, model, and function")
	}
	w.generators[generationKey(provider, model)] = fn
	if model != "*" {
		w.registeredModels = append(w.registeredModels, ModelCapability{Provider: provider, Model: model})
	}
}

// advertisedModels is the set of models the worker announces to Mobius Cloud:
// the explicit WorkerConfig.Models plus every concrete model registered via
// RegisterGenerator, deduplicated and with wildcards excluded. Advertising a
// registered generator keeps the catalog and routing consistent with what the
// worker can actually serve.
func (w *Worker) advertisedModels() []ModelCapability {
	seen := make(map[string]bool)
	out := make([]ModelCapability, 0, len(w.config.Models)+len(w.registeredModels))
	add := func(m ModelCapability) {
		if m.Provider == "" || m.Model == "" || m.Model == "*" {
			return
		}
		key := generationKey(m.Provider, m.Model)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, m)
	}
	for _, m := range w.config.Models {
		add(m)
	}
	for _, m := range w.registeredModels {
		add(m)
	}
	return out
}

// Run keeps the worker connected until ctx is cancelled or credentials are
// revoked. WebSocket reconnects are routine; job liveness is controlled by
// per-job heartbeat frames, not by socket lifetime alone.
func (w *Worker) Run(ctx context.Context) error {
	// Maintain the environment keep-warm hold for the worker's lifetime; the
	// maintainer reacts to per-job acquire/release and is a no-op off-Sprite.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	holdCtx, cancelHold := context.WithCancel(runCtx)
	defer cancelHold()
	required := w.config.keepWarmRequired()
	holdErr := make(chan error, 1)
	go func() {
		err := w.keepWarm.run(holdCtx, holdRunOptions{Required: required})
		// Park the error before cancelling: the main loop's takeHoldError is a
		// non-blocking receive, so cancelling first would race it into
		// returning a generic context.Canceled and losing the actionable
		// hold-failure message.
		holdErr <- err
		if err != nil && required && !contextDoneError(err) {
			cancelRun()
		}
	}()
	if w.config.keepWarmLifetime() {
		// Pin a baseline hold so the environment stays warm across the idle gaps
		// between this run's jobs (e.g. agent-loop think-time between tool calls).
		// Never released explicitly: the maintainer deletes the hold when holdCtx
		// is cancelled on worker shutdown.
		w.keepWarm.acquire()
		// Establish it synchronously so the environment is warm before we start
		// claiming work, rather than racing the maintainer's first async refresh
		// (a sub-second boot window in which the Sprite could pause). This is
		// fail-closed in lifetime mode: Mobius Cloud runs the worker as a Sprite
		// Service, so a failed hold should restart the worker instead of letting it
		// claim jobs while the Sprite is free to hibernate. A couple of quick
		// retries keep one transient Tasks-API blip from bouncing the process.
		if !w.ensureHoldWithRetry(holdCtx) {
			if err := runCtx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("mobius: required keep-warm hold was not established")
		}
		// Advertise "established" only when there is a real environment hold;
		// off-Sprite there is nothing to establish and the capability would lie.
		if w.keepWarm.pins() {
			w.keepWarmEstablished.Store(true)
			w.config.Logger.Info("worker keep-warm: lifetime hold established",
				"environment_id", w.config.EnvironmentID)
		}
	}
	// Watch for the host resuming from a suspend (e.g. a Sprite pause): the
	// socket is guaranteed dead after one, so reconnect immediately instead of
	// waiting for reads or writes to time out.
	go w.watchWake(runCtx)
	// In-flight jobs run under ctx (not the socket), so cancelling ctx aborts
	// them; wait for those goroutines to unwind before Run returns. This defer
	// runs before cancelHold (LIFO), keeping the keep-warm hold active until
	// all draining jobs have unwound.
	defer w.wg.Wait()
	failures := 0
	for {
		if err := takeHoldError(holdErr); err != nil {
			return err
		}
		if w.authRevoked.Load() {
			return ErrAuthRevoked
		}
		w.socketRegistered.Store(false)
		started := time.Now()
		if err := w.runSocket(runCtx); err != nil {
			if runCtx.Err() != nil {
				if holdErr := takeHoldError(holdErr); holdErr != nil {
					return holdErr
				}
				return runCtx.Err()
			}
			// Terminal protocol failures must not reconnect: a revoked
			// credential needs a fresh process, a duplicate
			// worker_instance_id means another live process owns this
			// identity, and a missing project needs operator attention. All
			// bubble up as a non-zero exit so a supervisor can restart
			// (rotated credential) or an operator can fix the configuration,
			// rather than spinning in a reconnect loop.
			if errors.Is(err, ErrAuthRevoked) || errors.Is(err, ErrWorkerInstanceConflict) || errors.Is(err, ErrProjectNotFound) {
				return err
			}
			w.config.Logger.Warn("worker socket disconnected; reconnecting", "error", err)
		}
		// Back off exponentially (with jitter, so a fleet doesn't reconnect in
		// lockstep during a server incident); reset once a connection both
		// registered and survived for a while.
		if w.socketRegistered.Load() && time.Since(started) >= healthySocketAge {
			failures = 0
		} else {
			failures++
		}
		if err := sleepContext(runCtx, reconnectBackoff(w.config.ReconnectDelay, failures)); err != nil {
			if holdErr := takeHoldError(holdErr); holdErr != nil {
				return holdErr
			}
			return err
		}
	}
}

// ensureHoldWithRetry synchronously establishes the keep-warm hold, retrying a
// couple of times so a single transient Tasks-API failure doesn't take down a
// fail-closed worker at boot.
func (w *Worker) ensureHoldWithRetry(ctx context.Context) bool {
	const attempts = 3
	for i := 0; ; i++ {
		if w.keepWarm.ensure(ctx) {
			return true
		}
		if i >= attempts-1 || ctx.Err() != nil {
			return false
		}
		if err := sleepContext(ctx, w.holdRetryDelay); err != nil {
			return false
		}
	}
}

// watchWake detects the host being suspended and resumed (a Sprite microVM
// pause, a laptop lid-close) by watching for wall-clock gaps far larger than
// its tick. Every pause kills the worker socket — the Sprites lifecycle docs
// guarantee open TCP connections drop on any pause, warm or cold — but no FIN
// reaches the guest, so without this the worker only notices when a read or
// write deadline expires. On a detected resume it drops the current socket
// (forcing an immediate reconnect, re-register, and re-claim) and pokes the
// keep-warm hold, whose task may have expired during the suspend.
func (w *Worker) watchWake(ctx context.Context) {
	const tick = time.Second
	t := time.NewTicker(tick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			if gap := now.Sub(last); gap > wakeGapThreshold {
				w.config.Logger.Info("worker: wall-clock gap detected; assuming host resumed from suspend and reconnecting",
					"gap", gap.String())
				w.keepWarm.poke()
				w.mu.Lock()
				s := w.currentSocket
				w.mu.Unlock()
				// Closing the connection fails the blocked read loop, which
				// makes runSocket return and Run redial immediately.
				s.close()
			}
			last = now
		}
	}
}

// reconnectBackoff returns the delay before reconnect attempt `failures`
// (1-based): base, then doubling per consecutive failure, capped at
// maxWorkerReconnectDelay, with ±20% jitter to de-synchronize a fleet.
func reconnectBackoff(base time.Duration, failures int) time.Duration {
	if base <= 0 {
		base = defaultWorkerReconnectDelay
	}
	shift := failures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	d := base << uint(shift)
	if d > maxWorkerReconnectDelay || d <= 0 {
		d = maxWorkerReconnectDelay
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

func takeHoldError(ch <-chan error) error {
	select {
	case err := <-ch:
		if err != nil && !contextDoneError(err) {
			return err
		}
	default:
	}
	return nil
}

func contextDoneError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (w *Worker) runSocket(ctx context.Context) error {
	socket, resp, err := w.client.dialWorkerSocket(ctx)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case 401, 403:
				// 401: the credential was revoked. 403: the credential is
				// live but no longer authorized for this project. Both need
				// operator action, not a retry loop.
				return ErrAuthRevoked
			case 404:
				return fmt.Errorf("mobius: worker socket dial: %w", ErrProjectNotFound)
			}
		}
		return fmt.Errorf("mobius: worker socket dial: %w", err)
	}
	defer socket.close()

	if err := w.register(ctx, socket); err != nil {
		return err
	}

	// Bind this socket as the worker's current one. In-flight jobs that
	// survived a previous disconnect immediately resume heartbeating over it,
	// and any unacknowledged terminal reports are re-sent (the server dedupes
	// by lease, answering `duplicate` acks).
	w.setSocket(socket)
	defer w.setSocket(nil)
	w.flushPendingReports(socket)

	socketCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan socketEnvelope)
	readErr := make(chan error, 1)
	go readSocketFrame(socketCtx, socket, frames, readErr)

	claimOutstanding := false
	// draining is per-connection state: the server re-issues worker.drain on
	// re-register when the drain is still in force, so a reconnect clearing it
	// is the intended handshake rather than an escape hatch.
	draining := false
	var outstandingClaimID string // message id of the outstanding claim

	// claimDeadline fires when an outstanding claim has gone unanswered for
	// claimResponseTimeout. It is armed when a claim is sent and disarmed when one
	// is answered (jobs.claimed or a matching error). It starts stopped.
	claimDeadline := time.NewTimer(w.claimResponseTimeout)
	if !claimDeadline.Stop() {
		<-claimDeadline.C
	}
	defer claimDeadline.Stop()
	armClaimDeadline := func() {
		if !claimDeadline.Stop() {
			select {
			case <-claimDeadline.C:
			default:
			}
		}
		claimDeadline.Reset(w.claimResponseTimeout)
	}
	disarmClaimDeadline := func() {
		if !claimDeadline.Stop() {
			select {
			case <-claimDeadline.C:
			default:
			}
		}
	}

	claim := func() error {
		if claimOutstanding || draining {
			return nil
		}
		available := cap(w.slots) - len(w.slots)
		if available <= 0 {
			return nil
		}
		frame := w.claimFrame(available)
		claimOutstanding = true
		outstandingClaimID = workerSocketMessageIDValue(frame.MessageId)
		if err := socket.writeJSON(frame); err != nil {
			return err
		}
		// Bound how long this claim may go unanswered; a lost claim/response
		// would otherwise strand the worker until the connection's max-age.
		armClaimDeadline()
		return nil
	}
	if err := claim(); err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	pingTicker := time.NewTicker(socketPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Worker shutdown: cancel in-flight jobs so they unwind, then hold
			// the socket open briefly so their cancelled reports (and any other
			// unacknowledged reports) go out now instead of surfacing
			// server-side as slow lease-expiry failures. (A plain socket read
			// error does NOT reach here — those jobs survive to be resumed on
			// the next connection.)
			cancel()
			w.cancelAllInflight()
			w.drainReportsOnShutdown(socket)
			return ctx.Err()
		case err := <-readErr:
			// Disconnect. Leave in-flight jobs running; the deferred
			// setSocket(nil) unblocks them to wait for the next connection.
			cancel()
			return err
		case <-claimDeadline.C:
			// The outstanding claim has gone unanswered past claimResponseTimeout:
			// this socket is no longer a reliable work channel (lost claim frame,
			// lost response, or a wedged server-side handler). Drop it and
			// reconnect for a clean slate (re-register + fresh claim) — the same
			// recovery the connection's max-age eventually forces, but fast enough
			// to beat Mobius Cloud's dead-worker reaper. In-flight jobs run under
			// the worker context, not the socket, so they survive the reconnect
			// and resume on the next connection.
			cancel()
			return fmt.Errorf("mobius: jobs.claim went unanswered for %s; reconnecting", w.claimResponseTimeout)
		case <-pingTicker.C:
			// Client-driven liveness: the pong (or any inbound frame) extends
			// the read deadline; a dead peer fails the read loop within
			// socketPongWait instead of whenever the OS TCP stack gives up.
			if err := socket.ping(); err != nil {
				cancel()
				return fmt.Errorf("mobius: worker socket ping failed: %w", err)
			}
		case <-ticker.C:
			if err := claim(); err != nil {
				return err
			}
			// Re-send any reports still awaiting their ack. Cheap (the map is
			// almost always empty) and idempotent server-side.
			w.flushPendingReports(socket)
		case <-w.slotFreed:
			if err := claim(); err != nil {
				return err
			}
		case frame, ok := <-frames:
			if !ok {
				// The read loop closed the frame channel; surface the read
				// error it parked (the select may pick the closed channel
				// first) so the disconnect cause isn't silently dropped.
				cancel()
				select {
				case err := <-readErr:
					return err
				default:
					return nil
				}
			}
			switch frame.Type {
			case "jobs.claimed":
				claimOutstanding = false
				disarmClaimDeadline()
				var claimed api.WorkerSocketJobsClaimedFrame
				if err := json.Unmarshal(frame.Raw, &claimed); err != nil {
					return err
				}
				for _, j := range claimed.Jobs {
					select {
					case w.slots <- struct{}{}:
					default:
						// The server leased this job to us but every slot is
						// taken. Report it back immediately so it can be
						// requeued in seconds — silently dropping it would
						// strand the job in `claimed` for a full lease TTL.
						w.config.Logger.Warn("server returned more jobs than available worker slots; requeueing", "job_id", j.Id)
						over := claimedRuntimeJob(w.client.projectHandle, w.config.WorkerInstanceID, w.config.EnvironmentID, j)
						w.deliverReport(w.config.Logger, over.JobID,
							failureReportFrame(over, "WorkerOverCapacity", "worker had no free slot for claimed job"))
						continue
					}
					job := claimedRuntimeJob(w.client.projectHandle, w.config.WorkerInstanceID, w.config.EnvironmentID, j)
					jobCtx, cancelJob := context.WithCancel(ctx)
					w.mu.Lock()
					w.inflight[job.JobID] = cancelJob
					w.mu.Unlock()
					w.wg.Add(1)
					go func() {
						defer w.wg.Done()
						w.runJob(jobCtx, job)
					}()
				}
				// Re-claim immediately only when this response actually carried
				// jobs and spare capacity may remain — chaining claims to drain a
				// backlog. After an EMPTY response, do not re-claim here: that
				// would hot-poll if the server ever answers without long-polling.
				// The periodic ticker, work.available, and slotFreed drive the
				// next claim instead.
				if len(claimed.Jobs) > 0 {
					if err := claim(); err != nil {
						return err
					}
				}
			case "work.available":
				if err := claim(); err != nil {
					return err
				}
			case "job.cancel":
				var cancelFrame api.WorkerSocketJobCancelFrame
				if err := json.Unmarshal(frame.Raw, &cancelFrame); err != nil {
					return err
				}
				w.cancelInflight(cancelFrame.JobId)
			case "job.heartbeat.ack":
				var ack api.WorkerSocketJobHeartbeatAckFrame
				if err := json.Unmarshal(frame.Raw, &ack); err != nil {
					return err
				}
				if ack.Cancel != nil {
					w.cancelInflight(ack.JobId)
				}
			case "job.report.ack":
				var ack api.WorkerSocketJobReportAckFrame
				if err := json.Unmarshal(frame.Raw, &ack); err != nil {
					return err
				}
				w.resolveReportAck(ack)
			case "worker.drain":
				draining = true
				_ = socket.writeJSON(api.WorkerSocketWorkerDrainingFrame{
					Type:      api.WorkerSocketWorkerDrainingFrameTypeWorkerDraining,
					MessageId: messageIDPtr(),
				})
			case "keepalive":
				_ = socket.writeJSON(api.WorkerSocketKeepaliveFrame{
					Type:      api.WorkerSocketKeepaliveFrameTypeKeepalive,
					MessageId: messageIDPtr(),
				})
			case "error":
				var errFrame api.WorkerSocketErrorFrame
				if err := json.Unmarshal(frame.Raw, &errFrame); err != nil {
					return err
				}
				if terminal := w.terminalProtocolError(errFrame.Error); terminal != nil {
					cancel()
					return terminal
				}
				// If this nonterminal error answers our outstanding claim (matched
				// by message id), clear claimOutstanding so the next tick or
				// work.available re-claims. Otherwise a claim that the server
				// rejects with a recoverable error would stay "in flight" forever
				// and silently stop the worker from claiming.
				if claimOutstanding && outstandingClaimID != "" &&
					workerSocketMessageIDValue(errFrame.MessageId) == outstandingClaimID {
					claimOutstanding = false
					disarmClaimDeadline()
				}
				w.config.Logger.Error("worker socket protocol error",
					"code", errFrame.Error.Code,
					"message", errFrame.Error.Message,
					"message_id", workerSocketMessageIDValue(errFrame.MessageId),
				)
			}
		}
	}
}

// terminalProtocolError maps a worker-socket protocol error frame to a
// terminal SDK error when its code denotes an unrecoverable condition the
// worker must not reconnect through. It returns nil for protocol errors the
// worker can keep running past (which the caller logs instead).
//
//   - invalid_actor: the credential was revoked; the process must restart
//     under a fresh credential ([ErrAuthRevoked]).
//   - worker_instance_conflict: another live process already owns this
//     worker_instance_id; reconnecting would just lose the race again, so
//     this is a hard startup failure ([ErrWorkerInstanceConflict]).
func (w *Worker) terminalProtocolError(e api.WorkerSocketProtocolError) error {
	switch e.Code {
	case "invalid_actor":
		return ErrAuthRevoked
	case "worker_instance_conflict":
		return &InstanceConflictError{
			WorkerInstanceID: w.config.WorkerInstanceID,
			ProjectHandle:    w.client.projectHandle,
			Message:          e.Message,
		}
	default:
		return nil
	}
}

func (w *Worker) register(ctx context.Context, socket *workerSocket) error {
	msgID := messageIDPtr()
	concurrency := w.config.Concurrency
	// On a reconnect with jobs still in flight the worker is not empty:
	// advertise only the genuinely free slots, or the server may lease more
	// jobs than the worker can hold.
	available := w.config.Concurrency - len(w.slots)
	if available < 0 {
		available = 0
	}
	frame := api.WorkerSocketRegisterFrame{
		Type:               api.WorkerSocketRegisterFrameTypeWorkerRegister,
		MessageId:          msgID,
		WorkerInstanceId:   w.config.WorkerInstanceID,
		ConcurrencyLimit:   &concurrency,
		AvailableSlots:     &available,
		ActionNames:        stringSlicePtr(w.actionNames()),
		Queues:             stringSlicePtr(w.config.Queues),
		Capabilities:       stringSlicePtr(w.capabilities()),
		Models:             modelCapabilitiesPtr(w.advertisedModels()),
		EnvironmentId:      strPtr(w.config.EnvironmentID),
		Name:               strPtr(w.config.Name),
		Version:            strPtr(w.config.Version),
		WorkerSessionToken: strPtr(w.sessionToken),
	}
	if err := socket.writeJSON(frame); err != nil {
		return err
	}
	// Bound registration: a server that accepts the upgrade but never answers
	// must not hang the worker (the claim deadline only covers claims), and a
	// cancelled worker context must be able to unblock the read.
	_ = socket.conn.SetReadDeadline(time.Now().Add(registerReadTimeout))
	stopUnblock := context.AfterFunc(ctx, func() { socket.close() })
	defer stopUnblock()
	for {
		_, raw, err := socket.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var env socketEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		env.Raw = raw
		switch env.Type {
		case "worker.registered":
			var registered api.WorkerSocketRegisteredFrame
			if err := json.Unmarshal(env.Raw, &registered); err != nil {
				return err
			}
			w.sessionToken = registered.WorkerSessionToken
			w.socketRegistered.Store(true)
			// Hand liveness back to the rolling ping/pong deadline now that
			// registration is complete.
			socket.extendReadDeadline()
			w.applyLeaseConfig(registered.Lease)
			w.config.Logger.Info("worker registered",
				"worker_instance_id", w.config.WorkerInstanceID,
				"heartbeat_cadence_seconds", registered.Lease.HeartbeatCadenceSeconds,
			)
			return nil
		case "error":
			var errFrame api.WorkerSocketErrorFrame
			if err := json.Unmarshal(env.Raw, &errFrame); err != nil {
				return err
			}
			if terminal := w.terminalProtocolError(errFrame.Error); terminal != nil {
				return terminal
			}
			return fmt.Errorf("mobius: worker register failed: %s: %s", errFrame.Error.Code, errFrame.Error.Message)
		}
	}
}

// applyLeaseConfig derives socket timing from the server's registered lease
// config instead of trusting compile-time constants to straddle a
// cross-service invariant. The claim deadline must sit above the server's
// long-poll window (its heartbeat cadence) and below the dead-worker reaper's
// TTL; twice the cadence does both. An explicitly overridden
// claimResponseTimeout (tests) is left alone.
func (w *Worker) applyLeaseConfig(lease api.WorkerSocketLeaseConfig) {
	cadence := time.Duration(lease.HeartbeatCadenceSeconds) * time.Second
	if cadence <= 0 {
		return
	}
	if w.claimResponseTimeout == defaultClaimResponseTimeout {
		derived := 2 * cadence
		if derived < 30*time.Second {
			derived = 30 * time.Second
		}
		w.claimResponseTimeout = derived
	}
}

func (w *Worker) claimFrame(available int) api.WorkerSocketJobsClaimFrame {
	return api.WorkerSocketJobsClaimFrame{
		Type:           api.WorkerSocketJobsClaimFrameTypeJobsClaim,
		MessageId:      messageIDPtr(),
		AvailableSlots: &available,
		ActionNames:    stringSlicePtr(w.actionNames()),
		Queues:         stringSlicePtr(w.config.Queues),
		Models:         modelCapabilitiesPtr(w.advertisedModels()),
	}
}

func (w *Worker) actionNames() []string {
	if len(w.config.Actions) > 0 {
		return append([]string(nil), w.config.Actions...)
	}
	return w.registry.Names()
}

// Capability labels a worker advertises to Mobius Cloud. These are operational
// telemetry — they describe the worker's keep-warm posture so operators can see,
// from the worker session alone, whether a run-scoped worker actually pinned its
// environment warm. They are namespaced ("keep-warm:*") and never required by
// any job, so advertising them does not affect job routing (eligibility is a
// subset match: a worker with extra labels still matches).
const (
	capabilityKeepWarmLifetime    = "keep-warm:lifetime"
	capabilityKeepWarmEstablished = "keep-warm:established"
)

// capabilities returns the worker's advertised capability labels. Empty for a
// worker with no keep-warm posture (the common case), so the register frame
// omits the field entirely.
func (w *Worker) capabilities() []string {
	var caps []string
	if w.config.keepWarmLifetime() {
		caps = append(caps, capabilityKeepWarmLifetime)
		if w.keepWarmEstablished.Load() {
			caps = append(caps, capabilityKeepWarmEstablished)
		}
	}
	return caps
}

// setSocket binds (or clears, with nil) the worker's current socket and wakes
// anything waiting to send through it. Run calls runSocket sequentially, so
// there is only ever one socket at a time.
func (w *Worker) setSocket(s *workerSocket) {
	w.mu.Lock()
	w.currentSocket = s
	close(w.socketChanged)
	w.socketChanged = make(chan struct{})
	w.mu.Unlock()
}

// sendFrame writes a frame over the current socket if one is live. It reports
// whether the write happened; callers of best-effort frames (heartbeats,
// generation deltas) simply skip when disconnected.
func (w *Worker) sendFrame(v any) bool {
	w.mu.Lock()
	s := w.currentSocket
	w.mu.Unlock()
	if s == nil {
		return false
	}
	return s.writeJSON(v) == nil
}

// deliverReport records a job's terminal report as pending and attempts to
// send it over the current socket. Delivery is ack-driven: the report stays in
// pendingReports — re-sent after every re-register and on the periodic tick —
// until the server answers with job.report.ack (whose `duplicate` flag makes
// redelivery safe). A write success alone proves nothing: the frame may reach
// the kernel's socket buffer and die with the connection, which is exactly the
// window in which reconnects happen.
func (w *Worker) deliverReport(log *slog.Logger, jobID string, frame api.WorkerSocketJobReportFrame) {
	id := workerSocketMessageIDValue(frame.MessageId)
	w.mu.Lock()
	w.pendingReports[id] = frame
	s := w.currentSocket
	w.mu.Unlock()
	if s == nil {
		log.Info("no live socket; job report queued for redelivery", "job_id", jobID)
		return
	}
	if err := s.writeJSON(frame); err != nil {
		log.Warn("job report write failed; queued for redelivery", "job_id", jobID, "error", err)
	}
}

// flushPendingReports re-sends every unacknowledged terminal report over the
// socket. Called after each successful register and on the periodic tick;
// reports are deleted only by resolveReportAck.
func (w *Worker) flushPendingReports(socket *workerSocket) {
	w.mu.Lock()
	frames := make([]api.WorkerSocketJobReportFrame, 0, len(w.pendingReports))
	for _, f := range w.pendingReports {
		frames = append(frames, f)
	}
	w.mu.Unlock()
	for _, f := range frames {
		if err := socket.writeJSON(f); err != nil {
			return
		}
	}
}

// resolveReportAck marks a terminal report as delivered. The server echoes the
// report's message id; when it doesn't, fall back to matching by job id.
func (w *Worker) resolveReportAck(ack api.WorkerSocketJobReportAckFrame) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id := workerSocketMessageIDValue(ack.MessageId); id != "" {
		if _, ok := w.pendingReports[id]; ok {
			delete(w.pendingReports, id)
			return
		}
	}
	for id, f := range w.pendingReports {
		if f.JobId == ack.JobId {
			delete(w.pendingReports, id)
		}
	}
}

// drainReportsOnShutdown gives cancelled jobs a bounded window to unwind and
// write their terminal reports over the still-open socket, then re-sends any
// reports still unacknowledged. Best-effort by construction — the ack read
// loop is already stopping — but it converts the common shutdown case from
// "server rediscovers every in-flight job via lease expiry" into "server got
// clean cancelled reports".
func (w *Worker) drainReportsOnShutdown(socket *workerSocket) {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownReportDrain):
		w.config.Logger.Warn("shutdown drain window elapsed with jobs still unwinding")
	}
	w.flushPendingReports(socket)
	w.mu.Lock()
	remaining := len(w.pendingReports)
	w.mu.Unlock()
	if remaining > 0 {
		w.config.Logger.Warn("worker stopping with unacknowledged job reports", "count", remaining)
	}
}

func (w *Worker) cancelInflight(jobID string) {
	w.mu.Lock()
	cancel := w.inflight[jobID]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *Worker) cancelAllInflight() {
	w.mu.Lock()
	cancels := make([]func(), 0, len(w.inflight))
	for _, cancel := range w.inflight {
		cancels = append(cancels, cancel)
	}
	w.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// finishJob releases the job's slot and nudges the run loop to claim again.
func (w *Worker) finishJob(jobID string) {
	w.mu.Lock()
	delete(w.inflight, jobID)
	w.mu.Unlock()
	select {
	case <-w.slots:
	default:
	}
	select {
	case w.slotFreed <- struct{}{}:
	default:
	}
}

// runJob executes one claimed job and hands its terminal report to the
// ack-driven delivery pipeline. jobCtx is per-job and cancellable (server
// cancel directive or worker shutdown). The job is intentionally not tied to
// the socket it was claimed on.
func (w *Worker) runJob(jobCtx context.Context, job *runtimeJob) {
	defer w.finishJob(job.JobID)
	// Hold the environment warm for this job so a long outbound op (e.g. git
	// clone) can't be frozen by the host pausing mid-flight. No-op off-Sprite.
	w.keepWarm.acquire()
	defer w.keepWarm.release()
	log := w.config.Logger.With(
		"job_id", job.JobID,
		"run_id", job.RunID,
		"session_id", job.SessionID,
		"step_id", job.StepID,
		"action", job.Action,
		"provider", job.Provider,
		"model", job.Model,
		"attempt", job.Attempt,
	)
	log.Info("job claimed")

	hbDone := make(chan struct{})
	execCtx, cancel := context.WithCancel(jobCtx)
	go w.heartbeatLoop(execCtx, job, hbDone)

	ctxVal := newContext(execCtx, w.client, job, log, nil)
	var result any
	var err error
	switch job.Kind {
	case api.WorkerSocketClaimedJobKindActionExecution:
		result, err = w.executeAction(ctxVal, job)
	case api.WorkerSocketClaimedJobKindLlmGeneration:
		result, err = w.executeGeneration(ctxVal, job)
	default:
		err = fmt.Errorf("unsupported job kind %q", job.Kind)
	}

	cancel()
	<-hbDone

	if err != nil {
		// A job that ended because its context was cancelled (server cancel
		// directive or worker shutdown) is reported with the protocol's
		// distinct `cancelled` status, not as a failure.
		if jobCtx.Err() != nil && contextDoneError(err) {
			log.Info("job cancelled", "error", err)
			w.deliverReport(log, job.JobID, cancelledReportFrame(job, err.Error()))
			return
		}
		log.Error("job failed", "error", err)
		w.deliverReport(log, job.JobID, failureReportFrame(job, classifyError(err), err.Error()))
		return
	}
	w.deliverReport(log, job.JobID, successReportFrame(job, result))
	log.Info("job complete")
}

func (w *Worker) executeAction(ctx Context, job *runtimeJob) (any, error) {
	if job.Action == "" {
		return nil, fmt.Errorf("action_name is required")
	}
	action, ok := w.registry.Get(job.Action)
	if !ok {
		return nil, fmt.Errorf("action %q not registered on this worker", job.Action)
	}
	return safeExecute(action, ctx, job.Parameters)
}

func (w *Worker) executeGeneration(ctx Context, job *runtimeJob) (any, error) {
	fn := w.generator(job.Provider, job.Model)
	if fn == nil {
		return nil, fmt.Errorf("generation provider/model %s/%s not registered on this worker", job.Provider, job.Model)
	}
	var seq atomic.Int64
	emit := func(delta map[string]any) error {
		// Deltas are live-only and best-effort; if disconnected, drop this one
		// and let the terminal report reconcile the final response. The atomic
		// sequence makes the emitter safe for generators that stream from
		// multiple goroutines.
		w.sendFrame(generationDeltaFrame(job, seq.Add(1), delta))
		return nil
	}
	return fn(ctx, GenerationJob{
		JobID:       job.JobID,
		RunID:       job.RunID,
		SessionID:   job.SessionID,
		AgentTurnID: job.AgentTurnID,
		ToolCallID:  job.ToolCallID,
		Provider:    job.Provider,
		Model:       job.Model,
		Spec:        job.Spec,
	}, emit)
}

func (w *Worker) generator(provider, model string) GenerationFunc {
	if fn := w.generators[generationKey(provider, model)]; fn != nil {
		return fn
	}
	return w.generators[generationKey(provider, "*")]
}

func (w *Worker) heartbeatLoop(ctx context.Context, job *runtimeJob, done chan<- struct{}) {
	defer close(done)
	// The server's per-job cadence is authoritative — it is sized to the job
	// lease. A user-configured interval may only tighten it: letting it
	// lengthen past the cadence would look like a dead job to the reaper.
	interval := job.HeartbeatInterval
	if user := w.config.HeartbeatInterval; user > 0 && (interval <= 0 || user < interval) {
		interval = user
	}
	if interval <= 0 {
		interval = 20 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Best-effort: while disconnected there is no live socket, so the
			// tick is skipped and heartbeats resume on the next connection. The
			// loop keeps running until execution finishes (ctx cancelled).
			if !w.sendFrame(heartbeatFrame(job)) {
				w.config.Logger.Debug("heartbeat skipped; no live socket", "job_id", job.JobID)
			}
		}
	}
}

func safeExecute(a Action, ctx Context, params map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return a.Execute(ctx, params)
}

func classifyError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Cancelled"
	}
	return "Error"
}

func generationKey(provider, model string) string {
	return provider + "/" + model
}

func stringSlicePtr(in []string) *[]string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	return &out
}

func modelCapabilitiesPtr(in []ModelCapability) *[]api.WorkerSocketModelCapability {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.WorkerSocketModelCapability, 0, len(in))
	for _, m := range in {
		if m.Provider == "" || m.Model == "" {
			continue
		}
		out = append(out, api.WorkerSocketModelCapability{Provider: m.Provider, Model: m.Model})
	}
	if len(out) == 0 {
		return nil
	}
	return &out
}
