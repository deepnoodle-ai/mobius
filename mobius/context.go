package mobius

import (
	"context"
	"log/slog"
)

// Context is passed to action and generation handlers executed by a worker.
// The agent loop engine runs in Mobius Cloud; handlers receive only the
// current job identity plus the cancellation context.
type Context interface {
	context.Context

	Logger() *slog.Logger
	ProjectHandle() string
	// ProjectID is a deprecated alias for ProjectHandle.
	//
	// Deprecated: use ProjectHandle.
	ProjectID() string
	RunID() string
	JobID() string
	// WorkflowName is retained for source compatibility with pre-automation
	// workers. Worker jobs no longer carry a workflow name, so this returns "".
	//
	// Deprecated: use RunID, JobID, and StepName for job identity.
	WorkflowName() string
	StepName() string
	Attempt() int
	Queue() string

	// EmitEvent is retained for source compatibility. The WebSocket worker
	// protocol currently has dedicated generation.delta streaming but no
	// general custom-event frame, so this is a no-op.
	EmitEvent(eventType string, payload map[string]any)
}

type executionContext struct {
	context.Context
	logger        *slog.Logger
	client        *Client
	projectHndl   string
	environmentID string
	runID         string
	jobID         string
	stepID        string
	leaseToken    string
	attempt       int
	queue         string
}

func (c *executionContext) Logger() *slog.Logger             { return c.logger }
func (c *executionContext) MobiusClient() *Client            { return c.client }
func (c *executionContext) ProjectHandle() string            { return c.projectHndl }
func (c *executionContext) ProjectID() string                { return c.projectHndl }
func (c *executionContext) EnvironmentID() string            { return c.environmentID }
func (c *executionContext) RunID() string                    { return c.runID }
func (c *executionContext) JobID() string                    { return c.jobID }
func (c *executionContext) LeaseToken() string               { return c.leaseToken }
func (c *executionContext) WorkflowName() string             { return "" }
func (c *executionContext) StepName() string                 { return c.stepID }
func (c *executionContext) Attempt() int                     { return c.attempt }
func (c *executionContext) Queue() string                    { return c.queue }
func (c *executionContext) EmitEvent(string, map[string]any) {}

func newContext(ctx context.Context, client *Client, j *runtimeJob, logger *slog.Logger, emit func(string, map[string]any)) Context {
	return &executionContext{
		Context:       ctx,
		logger:        logger,
		client:        client,
		projectHndl:   j.ProjectHandle,
		environmentID: j.EnvironmentID,
		runID:         j.RunID,
		jobID:         j.JobID,
		stepID:        j.StepID,
		leaseToken:    j.LeaseToken,
		attempt:       j.Attempt,
		queue:         j.Queue,
	}
}
