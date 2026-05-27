package mobius

import (
	"context"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultWaitRunReconnectDelay = time.Second

// StartRunOptions contains common fields for starting automation runs.
// ExternalID is a caller-supplied correlation and idempotency key.
type StartRunOptions struct {
	Inputs     map[string]interface{}
	Source     *api.AutomationRunSource
	ExternalID string
}

// ListRunsOptions filters and paginates project automation runs.
type ListRunsOptions struct {
	Status       api.AutomationRunStatus
	AutomationID string
	Cursor       string
	Limit        int
}

// WaitRunOptions configures WaitRun.
type WaitRunOptions struct {
	// Since is the durable SSE sequence cursor to resume from. Zero starts live.
	Since int64
	// ReconnectDelay is used when the SSE stream closes before the run
	// terminalizes. Defaults to one second.
	ReconnectDelay time.Duration
}

// StartRun starts a published automation by handle.
func (c *Client) StartRun(ctx context.Context, automationHandle string, opts *StartRunOptions) (*api.AutomationRun, error) {
	return c.StartAutomationRun(ctx, automationHandle, opts)
}

// StartAutomationRun starts a published automation by handle.
func (c *Client) StartAutomationRun(ctx context.Context, automationHandle string, opts *StartRunOptions) (*api.AutomationRun, error) {
	req := startAutomationRunRequest(opts)
	resp, err := c.ac.StartAutomationRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), automationHandle, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: start automation run: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedRunStatus("start automation run", resp.Status(), resp.Body)
	}
	return resp.JSON202, nil
}

// ListRuns returns project automation runs matching opts.
func (c *Client) ListRuns(ctx context.Context, opts *ListRunsOptions) (*api.AutomationRunListResponse, error) {
	resp, err := c.ac.ListAutomationRunsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listRunsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list runs: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("list runs", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetRun returns the current automation run detail.
func (c *Client) GetRun(ctx context.Context, runID string) (*api.AutomationRun, error) {
	resp, err := c.ac.GetAutomationRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("get run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CancelRun requests cancellation of an in-flight automation run.
func (c *Client) CancelRun(ctx context.Context, runID string, reason string) (*api.AutomationRun, error) {
	req := api.CancelAutomationRunRequest{}
	if reason != "" {
		req.Reason = &reason
	}
	resp, err := c.ac.CancelAutomationRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("cancel run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// SignalRun durably resumes a suspended automation step.
func (c *Client) SignalRun(ctx context.Context, runID, stepKey string, result map[string]interface{}) (*api.AutomationRun, error) {
	req := api.SignalAutomationRunRequest{StepKey: stepKey}
	if result != nil {
		req.Result = &result
	}
	resp, err := c.ac.SignalAutomationRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), req)
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
func (c *Client) SendRunSignal(ctx context.Context, runID, name string, payload map[string]interface{}) (*api.AutomationRun, error) {
	return c.SignalRun(ctx, runID, name, payload)
}

// WaitRun waits until runID reaches a terminal state and returns a fresh run
// detail. It combines the run SSE stream with GetRun fallback so callers can
// recover when a stream closes before the terminal event is observed.
func (c *Client) WaitRun(ctx context.Context, runID string, opts *WaitRunOptions) (*api.AutomationRun, error) {
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
			if ev.Payload == nil {
				continue
			}
			status, _ := (*ev.Payload)["status"].(string)
			if isTerminalStatusString(status) {
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
func IsTerminalRunStatus(status api.AutomationRunStatus) bool {
	return status == api.AutomationRunStatusCompleted ||
		status == api.AutomationRunStatusFailed ||
		status == api.AutomationRunStatusCancelled
}

func startAutomationRunRequest(opts *StartRunOptions) api.StartAutomationRunRequest {
	req := api.StartAutomationRunRequest{}
	if opts == nil {
		return req
	}
	if opts.Inputs != nil {
		req.Inputs = &opts.Inputs
	}
	if opts.Source != nil {
		req.Source = opts.Source
	}
	if opts.ExternalID != "" {
		req.ExternalId = &opts.ExternalID
	}
	return req
}

func listRunsParams(opts *ListRunsOptions) *api.ListAutomationRunsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListAutomationRunsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.AutomationID != "" {
		params.AutomationId = &opts.AutomationID
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

func isTerminalStatusString(status string) bool {
	return status == string(api.AutomationRunStatusCompleted) ||
		status == string(api.AutomationRunStatusFailed) ||
		status == string(api.AutomationRunStatusCancelled)
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
