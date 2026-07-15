package mobius

import (
	"context"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultWaitRunReconnectDelay = time.Second

// StartRunOptions contains common fields for starting loop runs.
//
// Event is the exact event object that starts the run, reachable in templates
// at ${{ event.<key> }}. Config is optional static or caller-provided
// configuration reachable at config.*. Meta is optional caller-supplied event
// metadata; Mobius adds its own provenance (run, loop, source, trigger ids).
type StartRunOptions struct {
	Event  map[string]interface{}
	Config map[string]interface{}
	Meta   map[string]interface{}
	Source *api.LoopRunSource
	// IdempotencyKey deduplicates run creation within the project.
	IdempotencyKey string
	// ExternalID is the deprecated name for IdempotencyKey.
	// Deprecated: use IdempotencyKey.
	ExternalID string
}

// ListRunsOptions filters and paginates project loop runs.
type ListRunsOptions struct {
	Status api.LoopRunStatus
	LoopID string
	Cursor string
	Limit  int
}

// WaitRunOptions configures WaitRun.
type WaitRunOptions struct {
	// Since is the durable SSE sequence cursor to resume from. Zero starts live.
	Since int64
	// ReconnectDelay is used when the SSE stream closes before the run
	// terminalizes. Defaults to one second.
	ReconnectDelay time.Duration
}

// StartRun starts a published loop run by loop ID.
func (c *Client) StartRun(ctx context.Context, loopID string, opts *StartRunOptions) (*api.LoopRun, error) {
	if opts != nil {
		key := normalizeIdempotencyKey(opts.IdempotencyKey)
		legacyKey := normalizeIdempotencyKey(opts.ExternalID)
		if key != "" && legacyKey != "" && key != legacyKey {
			return nil, fmt.Errorf("mobius: start run: IdempotencyKey and deprecated ExternalID must match when both are set")
		}
	}
	req := startRunRequest(opts)
	key := ""
	if req.IdempotencyKey != nil {
		key = *req.IdempotencyKey
	}
	resp, err := c.ac.StartRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(loopID), req, idempotencyRequestEditors(key)...)
	if err != nil {
		return nil, fmt.Errorf("mobius: start run: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedRunStatus("start run", resp.Status(), resp.Body)
	}
	return resp.JSON202, nil
}

// ListRuns returns project loop runs matching opts.
func (c *Client) ListRuns(ctx context.Context, opts *ListRunsOptions) (*api.LoopRunListResponse, error) {
	resp, err := c.ac.ListRunsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listRunsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list runs: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("list runs", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetRun returns the current loop run detail.
func (c *Client) GetRun(ctx context.Context, runID string) (*api.LoopRun, error) {
	resp, err := c.ac.GetRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("get run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CancelRun requests cancellation of an in-flight loop run.
func (c *Client) CancelRun(ctx context.Context, runID string, reason string) (*api.LoopRun, error) {
	req := api.CancelLoopRunRequest{}
	if reason != "" {
		req.Reason = &reason
	}
	resp, err := c.ac.CancelRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("cancel run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// SignalRun durably resumes a suspended loop step.
func (c *Client) SignalRun(ctx context.Context, runID, stepKey string, result map[string]interface{}) (*api.LoopRun, error) {
	req := api.SignalLoopRunRequest{StepKey: stepKey}
	if result != nil {
		req.Result = &result
	}
	resp, err := c.ac.SignalRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: signal run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("signal run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// SendRunSignal is retained as a source-compatible alias for SignalRun. The
// name argument is interpreted as the suspended step key.
func (c *Client) SendRunSignal(ctx context.Context, runID, name string, payload map[string]interface{}) (*api.LoopRun, error) {
	return c.SignalRun(ctx, runID, name, payload)
}

// WaitRun waits until runID reaches a terminal state and returns a fresh run
// detail. It combines the run SSE stream with GetRun fallback so callers can
// recover when a stream closes before the terminal event is observed.
func (c *Client) WaitRun(ctx context.Context, runID string, opts *WaitRunOptions) (*api.LoopRun, error) {
	since := int64(0)
	reconnectDelay := defaultWaitRunReconnectDelay
	if opts != nil {
		since = opts.Since
		if opts.ReconnectDelay > 0 {
			reconnectDelay = opts.ReconnectDelay
		}
	}

	for {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		if IsTerminalRunStatus(run.Status) {
			return run, nil
		}

		events, err := c.WatchRun(ctx, runID, since)
		if err != nil {
			if err := sleepContext(ctx, reconnectDelay); err != nil {
				return nil, err
			}
			continue
		}

		for ev := range events {
			if ev.Sequence > since {
				since = ev.Sequence
			}
			if isTerminalRunEventType(ev.EventType) {
				return c.GetRun(ctx, runID)
			}
		}

		if err := sleepContext(ctx, reconnectDelay); err != nil {
			return nil, err
		}
	}
}

// IsTerminalRunStatus reports whether status is completed, failed, or
// cancelled.
func IsTerminalRunStatus(status api.LoopRunStatus) bool {
	return status == api.LoopRunStatusCompleted ||
		status == api.LoopRunStatusFailed ||
		status == api.LoopRunStatusCancelled
}

func startRunRequest(opts *StartRunOptions) api.StartLoopRunRequest {
	req := api.StartLoopRunRequest{}
	if opts == nil {
		return req
	}
	if opts.Event != nil {
		req.Event = &opts.Event
	}
	if opts.Config != nil {
		req.Config = &opts.Config
	}
	if opts.Meta != nil {
		req.Meta = &opts.Meta
	}
	if opts.Source != nil {
		req.Source = opts.Source
	}
	key := normalizeIdempotencyKey(opts.IdempotencyKey)
	if key == "" {
		key = normalizeIdempotencyKey(opts.ExternalID)
	}
	if key != "" {
		req.IdempotencyKey = &key
	}
	return req
}

func listRunsParams(opts *ListRunsOptions) *api.ListRunsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListRunsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.LoopID != "" {
		params.LoopId = &opts.LoopID
	}
	if opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	return params
}

func unexpectedRunStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}

// isTerminalRunEventType reports whether a run-stream event marks the run as
// finished. The durable payload no longer carries a status field, so terminal
// state is detected from the event type instead.
func isTerminalRunEventType(eventType string) bool {
	switch eventType {
	case "run.completed", "run.failed", "run.cancelled":
		return true
	default:
		return false
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
