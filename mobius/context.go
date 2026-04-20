package mobius

import (
	"context"
	"log/slog"
)

// Context is the action-facing extension of context.Context. It exposes
// identity fields about the currently executing job and a structured
// logger scoped to the job. All methods are safe for concurrent use.
type Context interface {
	context.Context

	Logger() *slog.Logger
	ProjectID() string
	RunID() string
	JobID() string
	WorkflowName() string
	StepName() string
	Attempt() int
	Queue() string
	EmitEvent(eventType string, payload map[string]any)
}

type executionContext struct {
	context.Context
	emit         func(string, map[string]any)
	logger       *slog.Logger
	projectID    string
	runID        string
	jobID        string
	workflowName string
	stepName     string
	attempt      int
	queue        string
}

func (c *executionContext) Logger() *slog.Logger { return c.logger }
func (c *executionContext) ProjectID() string    { return c.projectID }
func (c *executionContext) RunID() string        { return c.runID }
func (c *executionContext) JobID() string        { return c.jobID }
func (c *executionContext) WorkflowName() string { return c.workflowName }
func (c *executionContext) StepName() string     { return c.stepName }
func (c *executionContext) Attempt() int         { return c.attempt }
func (c *executionContext) Queue() string        { return c.queue }
func (c *executionContext) EmitEvent(eventType string, payload map[string]any) {
	if c.emit != nil {
		c.emit(eventType, payload)
	}
}

func newContext(ctx context.Context, j *runtimeJob, logger *slog.Logger, emit func(string, map[string]any)) Context {
	return &executionContext{
		Context:      ctx,
		emit:         emit,
		logger:       logger,
		projectID:    j.ProjectID,
		runID:        j.RunID,
		jobID:        j.JobID,
		workflowName: j.WorkflowName,
		stepName:     j.StepName,
		attempt:      j.Attempt,
		queue:        j.Queue,
	}
}
