package mobius

import (
	"context"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultWaitRunReconnectDelay = time.Second

// StartRunOptions contains common fields for starting inline or saved workflow
// runs. ExternalID is a caller-supplied correlation or idempotency key.
type StartRunOptions struct {
	Queue      string
	Inputs     map[string]interface{}
	Metadata   map[string]string
	Tags       map[string]string
	ExternalID string
	Config     *api.ConfigEntries
}

// ListRunsOptions filters and paginates project workflow runs.
type ListRunsOptions struct {
	Status       api.WorkflowRunStatus
	WorkflowType string
	Queue        string
	ParentRunID  string
	InitiatedBy  string
	ExternalID   string
	Cursor       string
	Limit        int
}

// WaitRunOptions configures WaitRun.
type WaitRunOptions struct {
	// Since is the durable SSE cursor to resume from. Zero starts live.
	Since int64
	// ReconnectDelay is used when the SSE stream closes before the run
	// terminalizes. Defaults to one second.
	ReconnectDelay time.Duration
}

// StartRun starts an ephemeral workflow run from a workflow spec supplied by
// the caller. No workflow definition is persisted.
func (c *Client) StartRun(ctx context.Context, spec api.WorkflowSpec, opts *StartRunOptions) (*api.WorkflowRun, error) {
	req := api.StartInlineRunRequest{
		Mode: api.StartInlineRunRequestModeInline,
		Spec: spec,
	}
	applyStartRunOptions(&req.Queue, &req.Inputs, &req.Metadata, &req.Tags, &req.ExternalId, &req.Config, opts)

	var body api.StartRunRequest
	if err := body.FromStartInlineRunRequest(req); err != nil {
		return nil, fmt.Errorf("mobius: start run request: %w", err)
	}
	resp, err := c.ac.StartRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), body)
	if err != nil {
		return nil, fmt.Errorf("mobius: start run: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedRunStatus("start run", resp.Status(), resp.Body)
	}
	return resp.JSON202, nil
}

// StartWorkflowRun starts a run from an existing workflow definition.
func (c *Client) StartWorkflowRun(ctx context.Context, workflowID string, opts *StartRunOptions) (*api.WorkflowRun, error) {
	req := api.StartBoundRunRequest{}
	applyStartRunOptions(&req.Queue, &req.Inputs, &req.Metadata, &req.Tags, &req.ExternalId, &req.Config, opts)
	resp, err := c.ac.StartWorkflowRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(workflowID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: start workflow run: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedRunStatus("start workflow run", resp.Status(), resp.Body)
	}
	return resp.JSON202, nil
}

// ListRuns returns project workflow runs matching opts.
func (c *Client) ListRuns(ctx context.Context, opts *ListRunsOptions) (*api.WorkflowRunListResponse, error) {
	params := listRunsParams(opts)
	resp, err := c.ac.ListRunsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list runs: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("list runs", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetRun returns the current run detail, including terminal result/error fields
// and spawned jobs when available.
func (c *Client) GetRun(ctx context.Context, runID string) (*api.WorkflowRunDetail, error) {
	resp, err := c.ac.GetRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get run: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedRunStatus("get run", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CancelRun requests cancellation of an in-flight run.
func (c *Client) CancelRun(ctx context.Context, runID string) error {
	resp, err := c.ac.CancelRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID))
	if err != nil {
		return fmt.Errorf("mobius: cancel run: %w", err)
	}
	if resp.StatusCode() != 204 {
		return unexpectedRunStatus("cancel run", resp.Status(), resp.Body)
	}
	return nil
}

// ResumeRun re-enters any resumable waiting paths for a run.
func (c *Client) ResumeRun(ctx context.Context, runID string) error {
	resp, err := c.ac.ResumeRunWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID))
	if err != nil {
		return fmt.Errorf("mobius: resume run: %w", err)
	}
	if resp.StatusCode() != 204 {
		return unexpectedRunStatus("resume run", resp.Status(), resp.Body)
	}
	return nil
}

// SendRunSignal durably delivers a signal to a workflow run.
func (c *Client) SendRunSignal(ctx context.Context, runID, name string, payload map[string]interface{}) (*api.RunSignal, error) {
	req := api.SendRunSignalRequest{Name: name}
	if payload != nil {
		req.Payload = &payload
	}
	resp, err := c.ac.SendRunSignalWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: send run signal: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedRunStatus("send run signal", resp.Status(), resp.Body)
	}
	return resp.JSON202, nil
}

// WaitRun waits until runID reaches a terminal state and returns a fresh run
// detail. It combines the run SSE stream with GetRun fallback so callers can
// recover when a stream closes before the terminal event is observed.
func (c *Client) WaitRun(ctx context.Context, runID string, opts *WaitRunOptions) (*api.WorkflowRunDetail, error) {
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
			if ev.Seq > since {
				since = ev.Seq
			}
			if ev.Type != RunEventTypeRunUpdated {
				continue
			}
			status, _ := ev.Data["status"].(string)
			if isTerminalStatusString(status) {
				return c.GetRun(ctx, runID)
			}
		}

		if err := sleepContext(ctx, reconnectDelay); err != nil {
			return nil, err
		}
	}
}

// IsTerminalRunStatus reports whether status is completed or failed.
func IsTerminalRunStatus(status api.WorkflowRunStatus) bool {
	return status == api.WorkflowRunStatusCompleted || status == api.WorkflowRunStatusFailed
}

func applyStartRunOptions(queue **string, inputs **map[string]interface{}, metadata **map[string]string, tags **api.TagMap, externalID **string, config **api.ConfigEntries, opts *StartRunOptions) {
	if opts == nil {
		return
	}
	if opts.Queue != "" {
		*queue = &opts.Queue
	}
	if opts.Inputs != nil {
		*inputs = &opts.Inputs
	}
	if opts.Metadata != nil {
		*metadata = &opts.Metadata
	}
	if opts.Tags != nil {
		tagMap := api.TagMap(opts.Tags)
		*tags = &tagMap
	}
	if opts.ExternalID != "" {
		*externalID = &opts.ExternalID
	}
	if opts.Config != nil {
		*config = opts.Config
	}
}

func listRunsParams(opts *ListRunsOptions) *api.ListRunsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListRunsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.WorkflowType != "" {
		params.WorkflowType = &opts.WorkflowType
	}
	if opts.Queue != "" {
		params.Queue = &opts.Queue
	}
	if opts.ParentRunID != "" {
		params.ParentRunId = &opts.ParentRunID
	}
	if opts.InitiatedBy != "" {
		params.InitiatedBy = &opts.InitiatedBy
	}
	if opts.ExternalID != "" {
		params.ExternalId = &opts.ExternalID
	}
	if opts.Cursor != "" {
		cursor := api.CursorParam(opts.Cursor)
		params.Cursor = &cursor
	}
	if opts.Limit > 0 {
		limit := api.LimitParam(opts.Limit)
		params.Limit = &limit
	}
	return params
}

func unexpectedRunStatus(op, status string, body []byte) error {
	return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
}

func isTerminalStatusString(status string) bool {
	return status == string(api.WorkflowRunStatusCompleted) || status == string(api.WorkflowRunStatusFailed)
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
