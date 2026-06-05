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
}

// Worker connects to Mobius Cloud over the worker WebSocket, claims jobs,
// executes registered local actions or model generations, and reports results.
type Worker struct {
	client     *Client
	config     WorkerConfig
	registry   *ActionRegistry
	generators map[string]GenerationFunc

	sessionToken string
	authRevoked  atomic.Bool
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
		client:     c,
		config:     cfg,
		registry:   NewActionRegistry(),
		generators: map[string]GenerationFunc{},
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

	socketCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan socketEnvelope)
	readErr := make(chan error, 1)
	go readSocketFrame(socketCtx, socket, frames, readErr)

	slots := make(chan struct{}, w.config.Concurrency)
	done := make(chan string, w.config.Concurrency)
	cancels := map[string]context.CancelFunc{}
	var cancelMu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()

	cancelJob := func(jobID string) {
		cancelMu.Lock()
		cancelJob := cancels[jobID]
		cancelMu.Unlock()
		if cancelJob != nil {
			cancelJob()
		}
	}

	claimOutstanding := false
	draining := false
	claim := func() error {
		if claimOutstanding || draining {
			return nil
		}
		available := cap(slots) - len(slots)
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
			cancel()
			cancelMu.Lock()
			for _, cancelJob := range cancels {
				cancelJob()
			}
			cancelMu.Unlock()
			return ctx.Err()
		case err := <-readErr:
			cancel()
			return err
		case <-ticker.C:
			if err := claim(); err != nil {
				return err
			}
		case jobID := <-done:
			<-slots
			cancelMu.Lock()
			delete(cancels, jobID)
			cancelMu.Unlock()
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
					if len(slots) >= cap(slots) {
						w.config.Logger.Warn("server returned more jobs than available worker slots", "job_id", j.Id)
						continue
					}
					slots <- struct{}{}
					job := claimedRuntimeJob(w.client.projectHandle, w.config.WorkerInstanceID, w.config.EnvironmentID, j)
					jobCtx, cancelJob := context.WithCancel(socketCtx)
					cancelMu.Lock()
					cancels[j.Id] = cancelJob
					cancelMu.Unlock()
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer func() { done <- job.JobID }()
						w.executeJob(jobCtx, socket, job)
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
				cancelJob(cancelFrame.JobId)
			case "job.heartbeat.ack":
				var ack api.WorkerSocketJobHeartbeatAckFrame
				if err := json.Unmarshal(frame.Raw, &ack); err != nil {
					return err
				}
				if ack.Cancel != nil {
					cancelJob(ack.JobId)
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

func (w *Worker) executeJob(ctx context.Context, socket *workerSocket, job *runtimeJob) {
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
	execCtx, cancel := context.WithCancel(ctx)
	go w.heartbeatLoop(execCtx, socket, job, hbDone)

	ctxVal := newContext(execCtx, w.client, job, log, nil)
	var result any
	var err error
	switch job.Kind {
	case api.WorkerSocketClaimedJobKindActionExecution:
		result, err = w.executeAction(ctxVal, job)
	case api.WorkerSocketClaimedJobKindLlmGeneration:
		result, err = w.executeGeneration(ctxVal, socket, job)
	default:
		err = fmt.Errorf("unsupported job kind %q", job.Kind)
	}

	cancel()
	<-hbDone
	if err != nil {
		log.Error("job failed", "error", err)
		if reportErr := socketReportFailure(socket, job, classifyError(err), err.Error()); reportErr != nil {
			log.Error("failed to report job failure", "error", reportErr)
		}
		return
	}
	if reportErr := socketReportSuccess(socket, job, result); reportErr != nil {
		log.Error("failed to report job success", "error", reportErr)
		return
	}
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

func (w *Worker) executeGeneration(ctx Context, socket *workerSocket, job *runtimeJob) (any, error) {
	fn := w.generator(job.Provider, job.Model)
	if fn == nil {
		return nil, fmt.Errorf("generation provider/model %s/%s not registered on this worker", job.Provider, job.Model)
	}
	var seq int64
	emit := func(delta map[string]any) error {
		seq++
		return socketGenerationDelta(socket, job, seq, delta)
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

func (w *Worker) heartbeatLoop(ctx context.Context, socket *workerSocket, job *runtimeJob, done chan<- struct{}) {
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
			if err := socketHeartbeat(socket, job); err != nil {
				w.config.Logger.Warn("heartbeat write failed", "job_id", job.JobID, "error", err)
				return
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
