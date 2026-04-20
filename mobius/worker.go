package mobius

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// WorkerConfig configures a Worker.
type WorkerConfig struct {
	// WorkerID is a stable, unique identifier for this worker instance.
	// Required.
	WorkerID string
	// Name is the human-readable process name reported to Mobius
	// (e.g. "billing-worker"). Optional but recommended for observability.
	Name string
	// Version is the version string reported to Mobius (e.g. "1.2.0").
	Version string
	// Queues is the list of queue names this worker subscribes to.
	// Empty means "claim jobs from any queue in the project". Runs
	// default to the "default" queue when not explicitly assigned.
	Queues []string
	// Actions is an optional filter of action names this worker
	// will claim. When empty the worker advertises every action in
	// its registry — set this only if you want to claim a subset.
	Actions []string
	// Concurrency is the maximum number of jobs to execute in parallel.
	// Defaults to 10.
	Concurrency int
	// PollWaitSeconds is the long-poll window per claim request (0–30s).
	// The server holds the connection open until a job is available or
	// the window closes. Defaults to 20.
	PollWaitSeconds int
	// HeartbeatInterval overrides the heartbeat cadence while a job is
	// executing. When zero the SDK uses the interval advertised by the
	// server in the claim response, falling back to 10s.
	HeartbeatInterval time.Duration
	// Logger is the structured logger used by the worker and action
	// implementations. Defaults to slog.Default().
	Logger *slog.Logger
	// EventQueueSize bounds buffered custom events per executing job.
	// When full, the oldest event is dropped. Defaults to 256.
	EventQueueSize int
	// EventBatchSize controls how many custom events are sent per HTTP request.
	// Defaults to 20.
	EventBatchSize int
}

// Worker polls Mobius for queued jobs, executes the corresponding
// registered action, and reports the result back via the runtime API.
//
// Create a Worker with Client.NewWorker, register actions with
// RegisterAction or Worker.Register, then call Run to start the
// claim loop.
type Worker struct {
	client   *Client
	config   WorkerConfig
	registry *ActionRegistry
}

// NewWorker creates a Worker bound to the client.
func (c *Client) NewWorker(cfg WorkerConfig) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.PollWaitSeconds <= 0 {
		cfg.PollWaitSeconds = 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.EventQueueSize <= 0 {
		cfg.EventQueueSize = 256
	}
	if cfg.EventBatchSize <= 0 {
		cfg.EventBatchSize = 20
	}
	return &Worker{
		client:   c,
		config:   cfg,
		registry: NewActionRegistry(),
	}
}

// Run starts the claim–execute–heartbeat–complete loop and blocks
// until ctx is cancelled or a fatal error occurs.
func (w *Worker) Run(ctx context.Context) error {
	sem := make(chan struct{}, w.config.Concurrency)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sem <- struct{}{}:
		}

		job, err := w.client.runtimeClaim(ctx, w.config)
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.config.Logger.Error("claim error", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if job == nil {
			<-sem
			continue
		}

		go func() {
			defer func() { <-sem }()
			w.executeJob(ctx, job)
		}()
	}
}

// executeJob runs a single claimed job through the registered
// action, streaming heartbeats for the duration of the invocation.
func (w *Worker) executeJob(ctx context.Context, job *runtimeJob) {
	log := w.config.Logger.With(
		"job_id", job.JobID,
		"run_id", job.RunID,
		"workflow", job.WorkflowName,
		"step", job.StepName,
		"action", job.Action,
		"attempt", job.Attempt,
	)
	log.Info("job claimed")

	action, ok := w.registry.Get(job.Action)
	if !ok {
		msg := fmt.Sprintf("action %q not registered on this worker", job.Action)
		log.Error(msg)
		w.failJob(job, "ActionNotRegistered", msg)
		return
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hbDone := make(chan struct{})
	eventer := newJobEventer(w, job, log)
	eventDone := make(chan struct{})
	go w.heartbeatLoop(execCtx, job, cancel, hbDone)
	go eventer.run(execCtx, eventDone)

	ctxVal := newContext(execCtx, job, log, eventer.emit)

	result, err := safeExecute(action, ctxVal, job.Parameters)

	cancel()
	<-hbDone
	eventer.stop()
	<-eventDone

	if err != nil {
		log.Error("action failed", "error", err)
		w.failJob(job, classifyError(err), err.Error())
		return
	}

	if err := w.client.runtimeCompleteSuccess(context.Background(), job, result); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			log.Warn("complete: lease lost")
			return
		}
		log.Error("failed to complete job", "error", err)
		return
	}
	log.Info("job complete")
}

// heartbeatLoop periodically refreshes the job lease and, on a
// cancellation directive or lease loss, cancels the action via
// the provided cancel func. It exits when ctx is done.
func (w *Worker) heartbeatLoop(ctx context.Context, job *runtimeJob, cancelAction context.CancelFunc, done chan<- struct{}) {
	defer close(done)

	interval := w.config.HeartbeatInterval
	if interval <= 0 {
		interval = job.HeartbeatInterval
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			directives, err := w.client.runtimeHeartbeat(context.Background(), job)
			if err != nil {
				if errors.Is(err, ErrLeaseLost) {
					w.config.Logger.Warn("heartbeat: lease lost; cancelling action", "job_id", job.JobID)
					cancelAction()
					return
				}
				w.config.Logger.Error("heartbeat error", "job_id", job.JobID, "error", err)
				continue
			}
			if directives != nil && directives.ShouldCancel != nil && *directives.ShouldCancel {
				w.config.Logger.Warn("heartbeat: cancel requested", "job_id", job.JobID)
				cancelAction()
				return
			}
		}
	}
}

func (w *Worker) failJob(job *runtimeJob, errorType, message string) {
	// Use a background context so a cancelled exec ctx doesn't prevent
	// reporting terminal status.
	if err := w.client.runtimeCompleteFailure(context.Background(), job, errorType, message); err != nil {
		w.config.Logger.Error("failed to report job failure", "job_id", job.JobID, "error", err)
	}
}

// safeExecute runs the action and converts any panic into an error
// so a misbehaving action cannot take down the worker process.
func safeExecute(a Action, ctx Context, params map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return a.Execute(ctx, params)
}

// classifyError chooses the error_type reported to the server. It
// recognises context cancellation as a timeout and leaves everything
// else as a generic error.
func classifyError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Timeout"
	}
	return "Error"
}

type jobEventer struct {
	worker   *Worker
	job      *runtimeJob
	log      *slog.Logger
	mu       sync.Mutex
	notifyCh chan struct{}
	events   []jobEventEntry
	closed   bool
}

func newJobEventer(w *Worker, job *runtimeJob, log *slog.Logger) *jobEventer {
	return &jobEventer{
		worker:   w,
		job:      job,
		log:      log,
		notifyCh: make(chan struct{}, 1),
	}
}

func (e *jobEventer) emit(eventType string, payload map[string]any) {
	if eventType == "" {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if len(e.events) >= e.worker.config.EventQueueSize {
		e.events = append(e.events[1:], jobEventEntry{Type: eventType, Payload: payload})
		e.log.Warn("custom event queue full; dropping oldest event")
	} else {
		e.events = append(e.events, jobEventEntry{Type: eventType, Payload: payload})
	}
	select {
	case e.notifyCh <- struct{}{}:
	default:
	}
}

func (e *jobEventer) stop() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	select {
	case e.notifyCh <- struct{}{}:
	default:
	}
}

func (e *jobEventer) popBatch() []jobEventEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		return nil
	}
	n := e.worker.config.EventBatchSize
	if n > len(e.events) {
		n = len(e.events)
	}
	batch := append([]jobEventEntry(nil), e.events[:n]...)
	e.events = append([]jobEventEntry(nil), e.events[n:]...)
	return batch
}

func (e *jobEventer) isDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed && len(e.events) == 0
}

func (e *jobEventer) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		if batch := e.popBatch(); len(batch) > 0 {
			if err := e.worker.client.runtimeEmitEvents(context.Background(), e.job, batch); err != nil {
				switch {
				case errors.Is(err, ErrLeaseLost):
					e.log.Warn("custom event emit: lease lost")
					return
				case errors.Is(err, ErrPayloadTooLarge):
					e.log.Warn("custom event emit rejected: payload too large")
				case errors.Is(err, ErrRateLimited):
					e.log.Warn("custom event emit rejected: rate limited")
				default:
					e.log.Error("custom event emit failed", "error", err)
				}
			}
			continue
		}
		if e.isDone() {
			return
		}
		select {
		case <-ctx.Done():
		case <-e.notifyCh:
		}
	}
}
