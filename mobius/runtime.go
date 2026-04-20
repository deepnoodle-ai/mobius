package mobius

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ErrLeaseLost is returned when the server responds with 409 Conflict,
// indicating that the worker's lease on a task has been reclaimed.
var ErrLeaseLost = errors.New("mobius: lease lost")

// runtimeTask is the SDK's internal representation of a claimed task.
// Each task represents a single action invocation on behalf of a
// workflow run; workers no longer claim whole runs.
type runtimeTask struct {
	TaskID            string
	RunID             string
	ProjectID         string
	WorkflowName      string
	StepName          string
	Action            string
	Parameters        map[string]any
	Attempt           int
	Queue             string
	WorkerID          string
	HeartbeatInterval time.Duration
}

func (c *Client) apiClient() (*api.ClientWithResponses, error) {
	return c.ac, nil
}

type jobEventEntry struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

type jobEventsRequest struct {
	WorkerID string          `json:"worker_id"`
	Attempt  int             `json:"attempt"`
	Events   []jobEventEntry `json:"events"`
}

// runtimeClaim long-polls for the next available task matching the
// worker's queue and action filters. Returns nil when the poll
// window closes empty.
func (c *Client) runtimeClaim(ctx context.Context, cfg WorkerConfig) (*runtimeTask, error) {
	wait := cfg.PollWaitSeconds
	data := api.JobClaimRequest{
		WorkerId:    cfg.WorkerID,
		WaitSeconds: &wait,
	}
	if cfg.Name != "" {
		data.WorkerName = &cfg.Name
	}
	if cfg.Version != "" {
		data.WorkerVersion = &cfg.Version
	}
	if len(cfg.Queues) > 0 {
		queues := append([]string(nil), cfg.Queues...)
		data.Queues = &queues
	}
	if len(cfg.Actions) > 0 {
		actions := append([]string(nil), cfg.Actions...)
		data.Actions = &actions
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.PollWaitSeconds+5)*time.Second)
	defer cancel()

	resp, err := c.runtimeRequest(pollCtx, http.MethodPost, fmt.Sprintf("/projects/%s/jobs/claim", c.projectSlug), data)
	if err != nil {
		return nil, fmt.Errorf("mobius: claim: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mobius: claim: unexpected status %d", resp.StatusCode)
	}
	var claim api.JobClaim
	if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
		return nil, fmt.Errorf("mobius: claim decode: %w", err)
	}
	hb := time.Duration(0)
	if claim.HeartbeatIntervalSeconds != nil {
		hb = time.Duration(*claim.HeartbeatIntervalSeconds) * time.Second
	}
	return &runtimeTask{
		TaskID:            claim.JobId,
		RunID:             claim.RunId,
		ProjectID:         c.projectSlug,
		WorkflowName:      claim.WorkflowName,
		StepName:          claim.StepName,
		Action:            claim.Action,
		Parameters:        claim.Parameters,
		Attempt:           claim.Attempt,
		Queue:             claim.Queue,
		WorkerID:          cfg.WorkerID,
		HeartbeatInterval: hb,
	}, nil
}

// runtimeHeartbeat refreshes the lease on a claimed task and returns
// any directives from the server. Returns ErrLeaseLost on 409.
func (c *Client) runtimeHeartbeat(ctx context.Context, task *runtimeTask) (*api.JobHeartbeatDirectives, error) {
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/jobs/%s/heartbeat", c.projectSlug, task.TaskID), api.JobFenceRequest{
		WorkerId: task.WorkerID,
		Attempt:  task.Attempt,
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: heartbeat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil, ErrLeaseLost
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mobius: heartbeat: unexpected status %d", resp.StatusCode)
	}
	var body api.JobHeartbeat
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("mobius: heartbeat decode: %w", err)
	}
	return &body.Directives, nil
}

// runtimeCompleteSuccess reports a successful task completion along
// with its action result. The result is JSON-encoded and delivered
// as a base64-encoded blob.
func (c *Client) runtimeCompleteSuccess(ctx context.Context, task *runtimeTask, result any) error {
	data := api.JobCompleteRequest{
		WorkerId: task.WorkerID,
		Attempt:  task.Attempt,
		Status:   api.JobCompleteRequestStatusCompleted,
	}
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("mobius: marshal action result: %w", err)
		}
		enc := base64.StdEncoding.EncodeToString(b)
		data.ResultB64 = &enc
	}
	return c.runtimeCompleteRaw(ctx, task.TaskID, data)
}

// runtimeCompleteFailure reports a failed task with an error message
// and optional error type. The server uses the error type to decide
// whether the task is retryable.
func (c *Client) runtimeCompleteFailure(ctx context.Context, task *runtimeTask, errorType, message string) error {
	data := api.JobCompleteRequest{
		WorkerId:     task.WorkerID,
		Attempt:      task.Attempt,
		Status:       api.JobCompleteRequestStatusFailed,
		ErrorMessage: strPtr(message),
	}
	if errorType != "" {
		data.ErrorType = &errorType
	}
	return c.runtimeCompleteRaw(ctx, task.TaskID, data)
}

func (c *Client) runtimeCompleteRaw(ctx context.Context, taskID string, req api.JobCompleteRequest) error {
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/jobs/%s/complete", c.projectSlug, taskID), req)
	if err != nil {
		return fmt.Errorf("mobius: complete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return ErrLeaseLost
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mobius: complete: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) runtimeEmitEvents(ctx context.Context, task *runtimeTask, events []jobEventEntry) error {
	if len(events) == 0 {
		return nil
	}
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/jobs/%s/events", c.projectSlug, task.TaskID), jobEventsRequest{
		WorkerID: task.WorkerID,
		Attempt:  task.Attempt,
		Events:   events,
	})
	if err != nil {
		return fmt.Errorf("mobius: emit events: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusConflict:
		return ErrLeaseLost
	case http.StatusRequestEntityTooLarge:
		return ErrPayloadTooLarge
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mobius: emit events: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) runtimeRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return c.httpClient.Do(req)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
