package mobius

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Empty means "claim tasks from any queue in the namespace". Runs
	// default to the "default" queue when not explicitly assigned.
	Queues []string
	// Actions is an optional filter of action names this worker
	// will claim. When empty the worker advertises every action in
	// its registry — set this only if you want to claim a subset.
	Actions []string
	// Concurrency is the maximum number of tasks to execute in parallel.
	// Defaults to 10.
	Concurrency int
	// PollWaitSeconds is the long-poll window per claim request (0–30s).
	// The server holds the connection open until a task is available or
	// the window closes. Defaults to 20.
	PollWaitSeconds int
	// HeartbeatInterval overrides the heartbeat cadence while a task is
	// executing. When zero the SDK uses the interval advertised by the
	// server in the claim response, falling back to 10s.
	HeartbeatInterval time.Duration
	// Logger is the structured logger used by the worker and action
	// implementations. Defaults to slog.Default().
	Logger *slog.Logger
}

// Worker polls Mobius for queued tasks, executes the corresponding
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

		task, err := w.client.runtimeClaim(ctx, w.config)
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
		if task == nil {
			<-sem
			continue
		}

		go func() {
			defer func() { <-sem }()
			w.executeTask(ctx, task)
		}()
	}
}

// executeTask runs a single claimed task through the registered
// action, streaming heartbeats for the duration of the invocation.
func (w *Worker) executeTask(ctx context.Context, task *runtimeTask) {
	log := w.config.Logger.With(
		"task_id", task.TaskID,
		"run_id", task.RunID,
		"workflow", task.WorkflowName,
		"step", task.StepName,
		"action", task.Action,
		"attempt", task.Attempt,
	)
	log.Info("task claimed")

	action, ok := w.registry.Get(task.Action)
	if !ok {
		msg := fmt.Sprintf("action %q not registered on this worker", task.Action)
		log.Error(msg)
		w.failTask(task, "ActionNotRegistered", msg)
		return
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hbDone := make(chan struct{})
	go w.heartbeatLoop(execCtx, task, cancel, hbDone)

	ctxVal := newContext(execCtx, task, log)

	result, err := safeExecute(action, ctxVal, task.Parameters)

	cancel()
	<-hbDone

	if err != nil {
		log.Error("action failed", "error", err)
		w.failTask(task, classifyError(err), err.Error())
		return
	}

	if err := w.client.runtimeCompleteSuccess(context.Background(), task, result); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			log.Warn("complete: lease lost")
			return
		}
		log.Error("failed to complete task", "error", err)
		return
	}
	log.Info("task complete")
}

// heartbeatLoop periodically refreshes the task lease and, on a
// cancellation directive or lease loss, cancels the action via
// the provided cancel func. It exits when ctx is done.
func (w *Worker) heartbeatLoop(ctx context.Context, task *runtimeTask, cancelAction context.CancelFunc, done chan<- struct{}) {
	defer close(done)

	interval := w.config.HeartbeatInterval
	if interval <= 0 {
		interval = task.HeartbeatInterval
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
			directives, err := w.client.runtimeHeartbeat(context.Background(), task)
			if err != nil {
				if errors.Is(err, ErrLeaseLost) {
					w.config.Logger.Warn("heartbeat: lease lost; cancelling action", "task_id", task.TaskID)
					cancelAction()
					return
				}
				w.config.Logger.Error("heartbeat error", "task_id", task.TaskID, "error", err)
				continue
			}
			if directives != nil && directives.ShouldCancel != nil && *directives.ShouldCancel {
				w.config.Logger.Warn("heartbeat: cancel requested", "task_id", task.TaskID)
				cancelAction()
				return
			}
		}
	}
}

func (w *Worker) failTask(task *runtimeTask, errorType, message string) {
	// Use a background context so a cancelled exec ctx doesn't prevent
	// reporting terminal status.
	if err := w.client.runtimeCompleteFailure(context.Background(), task, errorType, message); err != nil {
		w.config.Logger.Error("failed to report task failure", "task_id", task.TaskID, "error", err)
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
