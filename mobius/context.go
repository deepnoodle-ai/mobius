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
	JobID() string
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
	environmentID string
	jobID         string
	leaseToken    string
	attempt       int
	queue         string
}

func (c *executionContext) Logger() *slog.Logger             { return c.logger }
func (c *executionContext) MobiusClient() *Client            { return c.client }
func (c *executionContext) EnvironmentID() string            { return c.environmentID }
func (c *executionContext) JobID() string                    { return c.jobID }
func (c *executionContext) LeaseToken() string               { return c.leaseToken }
func (c *executionContext) Attempt() int                     { return c.attempt }
func (c *executionContext) Queue() string                    { return c.queue }
func (c *executionContext) EmitEvent(string, map[string]any) {}

func newContext(ctx context.Context, client *Client, j *runtimeJob, logger *slog.Logger, emit func(string, map[string]any)) Context {
	return &executionContext{
		Context:       ctx,
		logger:        logger,
		client:        client,
		environmentID: j.EnvironmentID,
		jobID:         j.JobID,
		leaseToken:    j.LeaseToken,
		attempt:       j.Attempt,
		queue:         j.Queue,
	}
}
