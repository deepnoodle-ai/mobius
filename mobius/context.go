package mobius

import (
	"context"
	"log/slog"
)

// Context is the action-facing extension of context.Context. It exposes
// identity fields about the currently executing task and a structured
// logger scoped to the task. All methods are safe for concurrent use.
type Context interface {
	context.Context

	Logger() *slog.Logger
	RunID() string
	TaskID() string
	WorkflowName() string
	StepName() string
	Attempt() int
	Queue() string
}

type executionContext struct {
	context.Context
	logger       *slog.Logger
	runID        string
	taskID       string
	workflowName string
	stepName     string
	attempt      int
	queue        string
}

func (c *executionContext) Logger() *slog.Logger { return c.logger }
func (c *executionContext) RunID() string        { return c.runID }
func (c *executionContext) TaskID() string       { return c.taskID }
func (c *executionContext) WorkflowName() string { return c.workflowName }
func (c *executionContext) StepName() string     { return c.stepName }
func (c *executionContext) Attempt() int         { return c.attempt }
func (c *executionContext) Queue() string        { return c.queue }

func newContext(ctx context.Context, t *runtimeTask, logger *slog.Logger) Context {
	return &executionContext{
		Context:      ctx,
		logger:       logger,
		runID:        t.RunID,
		taskID:       t.TaskID,
		workflowName: t.WorkflowName,
		stepName:     t.StepName,
		attempt:      t.Attempt,
		queue:        t.Queue,
	}
}
