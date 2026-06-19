package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultWorkerReconnectDelay = 2 * time.Second

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
type GenerationEmitter func(delta map[string]any) error

// GenerationFunc executes one worker-backed LLM generation and returns the
// terminal generation result. Use the emitter for token deltas; the terminal
// result remains authoritative.
type GenerationFunc func(ctx Context, job GenerationJob, emit GenerationEmitter) (map[string]any, error)

// WorkerConfig configures a Worker.
type WorkerConfig struct {
	WorkerInstanceID  string
	Concurrency       int
	Name              string
	Version           string
	EnvironmentID     string
	Queues            []string
	Actions           []string
	Models            []ModelCapability
	ReconnectDelay    time.Duration
	HeartbeatInterval time.Duration
	Logger            *slog.Logger

	// KeepWarmForLifetime pins the environment keep-warm hold for the worker's
	// entire lifetime rather than only while a job is in flight. Set it for
	// single-run (run-scoped) environments: their agent steps dispatch a sequence
	// of tool-call jobs with idle LLM think-time between them, and a per-job hold
	// lets the environment (e.g. a Sprite microVM) pause in those gaps and strand
	// the worker so the next job is never claimed. Leave it false for reused
	// (lease/explicit) environments so they still hibernate between jobs.
	KeepWarmForLifetime bool
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

	sessionToken string
	authRevoked  atomic.Bool

	// keepWarm holds the worker's environment in an active state while jobs are
	// in flight (e.g. a Sprite microVM); a no-op on hosts that don't pause.
	keepWarm hold

	// Job lifecycle state that survives socket reconnects.
	slots     chan struct{} // capacity = Concurrency; one token per in-flight job
	slotFreed chan struct{} // nudges the run loop to claim after a job finishes
	wg        sync.WaitGroup

	mu            sync.Mutex
	currentSocket *workerSocket     // the live socket, or nil while disconnected
	socketChanged chan struct{}     // closed+replaced whenever currentSocket changes
	inflight      map[string]func() // jobID -> cancel for in-flight jobs
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
	return &Worker{
		client:        c,
		config:        cfg,
		registry:      NewActionRegistry(),
		generators:    map[string]GenerationFunc{},
		keepWarm:      detectHold(cfg.Logger),
		slots:         make(chan struct{}, cfg.Concurrency),
		slotFreed:     make(chan struct{}, cfg.Concurrency),
		socketChanged: make(chan struct{}),
		inflight:      map[string]func(){},
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
func (w *Worker) RegisterGenerator(provider, model string, fn GenerationFunc) {
	if provider == "" || model == "" || fn == nil {
		panic("mobius: RegisterGenerator requires provider, model, and function")
	}
	w.generators[generationKey(provider, model)] = fn
}

// Run keeps the worker connected until ctx is cancelled or credentials are
// revoked. WebSocket reconnects are routine; job liveness is controlled by
// per-job heartbeat frames, not by socket lifetime alone.
func (w *Worker) Run(ctx context.Context) error {
	// Maintain the Sprite keep-warm hold for the worker's lifetime; the
	// maintainer reacts to per-job acquire/release and is a no-op off-Sprite.
	holdCtx, cancelHold := context.WithCancel(ctx)
	defer cancelHold()
	go w.keepWarm.run(holdCtx)
	if w.config.KeepWarmForLifetime {
		// Pin a baseline hold so the environment stays warm across the idle gaps
		// between this run's jobs (e.g. agent-loop think-time between tool calls).
		// Never released explicitly: the maintainer deletes the hold when holdCtx
		// is cancelled on worker shutdown.
		w.keepWarm.acquire()
	}
	// In-flight jobs run under ctx (not the socket), so cancelling ctx aborts
	// them; wait for those goroutines to unwind before Run returns. This defer
	// runs before cancelHold (LIFO), keeping the keep-warm hold active until
	// all draining jobs have unwound.
	defer w.wg.Wait()
	for {
		if w.authRevoked.Load() {
			return ErrAuthRevoked
		}
		if err := w.runSocket(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Terminal protocol failures must not reconnect: a revoked
			// credential needs a fresh process, and a duplicate
			// worker_instance_id means another live process owns this
			// identity. Both bubble up as a non-zero exit so a supervisor
			// can restart (rotated credential) or an operator can fix the
			// duplicate instance ID, rather than spinning in a reconnect loop.
			if errors.Is(err, ErrAuthRevoked) || errors.Is(err, ErrWorkerInstanceConflict) {
				return err
			}
			w.config.Logger.Warn("worker socket disconnected; reconnecting", "error", err)
		}
		if err := sleepContext(ctx, w.config.ReconnectDelay); err != nil {
			return err
		}
	}
}

func (w *Worker) runSocket(ctx context.Context) error {
	socket, resp, err := w.client.dialWorkerSocket(ctx)
	if err != nil {
		if resp != nil && resp.StatusCode == 401 {
			return ErrAuthRevoked
		}
		return fmt.Errorf("mobius: worker socket dial: %w", err)
	}
	defer socket.close()

	if err := w.register(ctx, socket); err != nil {
		return err
	}

	// Bind this socket as the worker's current one. In-flight jobs that
	// survived a previous disconnect immediately resume heartbeating and flush
	// any pending terminal report over it (see deliverReport / heartbeatLoop).
	w.setSocket(socket)
	defer w.setSocket(nil)

	socketCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan socketEnvelope)
	readErr := make(chan error, 1)
	go readSocketFrame(socketCtx, socket, frames, readErr)

	claimOutstanding := false
	draining := false
	claim := func() error {
		if claimOutstanding || draining {
			return nil
		}
		available := cap(w.slots) - len(w.slots)
		if available <= 0 {
			return nil
		}
		claimOutstanding = true
		return socket.writeJSON(w.claimFrame(available))
	}
	if err := claim(); err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Worker shutdown: cancel in-flight jobs so they unwind. (A plain
			// socket read error does NOT reach here — those jobs survive to be
			// resumed on the next connection.)
			cancel()
			w.cancelAllInflight()
			return ctx.Err()
		case err := <-readErr:
			// Disconnect. Leave in-flight jobs running; the deferred
			// setSocket(nil) unblocks them to wait for the next connection.
			cancel()
			return err
		case <-ticker.C:
			if err := claim(); err != nil {
				return err
			}
		case <-w.slotFreed:
			if err := claim(); err != nil {
				return err
			}
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			switch frame.Type {
			case "jobs.claimed":
				claimOutstanding = false
				var claimed api.WorkerSocketJobsClaimedFrame
				if err := json.Unmarshal(frame.Raw, &claimed); err != nil {
					return err
				}
				for _, j := range claimed.Jobs {
					select {
					case w.slots <- struct{}{}:
					default:
						w.config.Logger.Warn("server returned more jobs than available worker slots", "job_id", j.Id)
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
						w.runJob(ctx, jobCtx, job)
					}()
				}
				if err := claim(); err != nil {
					return err
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
	available := w.config.Concurrency
	frame := api.WorkerSocketRegisterFrame{
		Type:               api.WorkerSocketRegisterFrameTypeWorkerRegister,
		MessageId:          msgID,
		WorkerInstanceId:   w.config.WorkerInstanceID,
		ConcurrencyLimit:   &concurrency,
		AvailableSlots:     &available,
		ActionNames:        stringSlicePtr(w.actionNames()),
		Queues:             stringSlicePtr(w.config.Queues),
		Models:             modelCapabilitiesPtr(w.config.Models),
		EnvironmentId:      strPtr(w.config.EnvironmentID),
		Name:               strPtr(w.config.Name),
		Version:            strPtr(w.config.Version),
		WorkerSessionToken: strPtr(w.sessionToken),
	}
	if err := socket.writeJSON(frame); err != nil {
		return err
	}
	for {
		_, raw, err := socket.conn.ReadMessage()
		if err != nil {
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

func (w *Worker) claimFrame(available int) api.WorkerSocketJobsClaimFrame {
	return api.WorkerSocketJobsClaimFrame{
		Type:           api.WorkerSocketJobsClaimFrameTypeJobsClaim,
		MessageId:      messageIDPtr(),
		AvailableSlots: &available,
		ActionNames:    stringSlicePtr(w.actionNames()),
		Queues:         stringSlicePtr(w.config.Queues),
		Models:         modelCapabilitiesPtr(w.config.Models),
	}
}

func (w *Worker) actionNames() []string {
	if len(w.config.Actions) > 0 {
		return append([]string(nil), w.config.Actions...)
	}
	return w.registry.Names()
}

// setSocket binds (or clears, with nil) the worker's current socket and wakes
// anything waiting to send through it (terminal reports parked during a
// disconnect). Run calls runSocket sequentially, so there is only ever one
// socket at a time.
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

// deliverReport sends a job's terminal report over the current socket, waiting
// across reconnects until it lands. It gives up only when ctx (the worker
// lifetime) is done. Terminal reports are idempotent server-side, so a report
// that races a socket teardown is safe to re-send on the next connection.
func (w *Worker) deliverReport(ctx context.Context, log *slog.Logger, jobID string, frame api.WorkerSocketJobReportFrame) {
	for {
		w.mu.Lock()
		s := w.currentSocket
		changed := w.socketChanged
		w.mu.Unlock()
		if s != nil {
			if err := s.writeJSON(frame); err == nil {
				return
			}
			log.Warn("job report write failed; waiting for reconnect", "job_id", jobID)
		}
		select {
		case <-ctx.Done():
			log.Warn("worker stopping; dropping job report", "job_id", jobID)
			return
		case <-changed:
		}
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

// runJob executes one claimed job and delivers its terminal report. lifeCtx is
// the worker lifetime (used to park the report across reconnects); jobCtx is
// per-job and cancellable (server cancel directive or worker shutdown). The
// job is intentionally not tied to the socket it was claimed on.
func (w *Worker) runJob(lifeCtx, jobCtx context.Context, job *runtimeJob) {
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
		log.Error("job failed", "error", err)
		w.deliverReport(lifeCtx, log, job.JobID, failureReportFrame(job, classifyError(err), err.Error()))
		return
	}
	w.deliverReport(lifeCtx, log, job.JobID, successReportFrame(job, result))
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
	var seq int64
	emit := func(delta map[string]any) error {
		seq++
		// Deltas are live-only and best-effort; if disconnected, drop this one
		// and let the terminal report reconcile the final response.
		w.sendFrame(generationDeltaFrame(job, seq, delta))
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
	interval := w.config.HeartbeatInterval
	if interval <= 0 {
		interval = job.HeartbeatInterval
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
