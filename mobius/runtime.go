package mobius

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ErrLeaseLost is returned when the server responds with 409 Conflict,
// indicating that the worker's lease on a job has been reclaimed.
var ErrLeaseLost = errors.New("mobius: lease lost")

// runtimeJob is the SDK's internal representation of a claimed job.
// Each job represents a single action invocation on behalf of a
// workflow run; workers no longer claim whole runs.
type runtimeJob struct {
	JobID             string
	RunID             string
	ProjectHandle     string
	WorkflowName      string
	StepName          string
	Action            string
	Parameters        map[string]any
	Attempt           int
	Queue             string
	WorkerInstanceID  string
	LeaseToken        string
	HeartbeatInterval time.Duration
}

func (c *Client) apiClient() (*api.ClientWithResponses, error) {
	return c.ac, nil
}

type jobEventEntry struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// runtimeClaim long-polls for the next available job matching the
// worker's queue and action filters. Returns nil when the poll
// window closes empty. Returns [ErrWorkerInstanceConflict] when the
// server rejects the registration because another live process is
// already holding this worker_instance_id.
func (c *Client) runtimeClaim(ctx context.Context, cfg WorkerConfig, sessionToken string) (*runtimeJob, error) {
	if c.projectHandle == "" {
		return nil, fmt.Errorf("mobius: claim: no project configured — set MOBIUS_PROJECT or pass --project")
	}
	wait := cfg.PollWaitSeconds
	instanceID := cfg.WorkerInstanceID
	data := api.JobClaimRequest{
		WorkerInstanceId:   instanceID,
		WorkerSessionToken: sessionToken,
		ConcurrencyLimit:   cfg.Concurrency,
		WaitSeconds:        &wait,
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

	resp, err := c.runtimeRequest(pollCtx, http.MethodPost, fmt.Sprintf("/v1/projects/%s/jobs/claim", url.PathEscape(c.projectHandle)), data)
	if err != nil {
		return nil, fmt.Errorf("mobius: claim: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrAuthRevoked
	}
	if resp.StatusCode == http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, parseInstanceConflict(body, instanceID, c.projectHandle)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("mobius: claim: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var claim api.JobClaim
	if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
		return nil, fmt.Errorf("mobius: claim decode: %w", err)
	}
	hb := time.Duration(0)
	if claim.HeartbeatIntervalSeconds != nil {
		hb = time.Duration(*claim.HeartbeatIntervalSeconds) * time.Second
	}
	action, err := claim.Spec.AsJobActionSpec()
	if err != nil {
		return nil, fmt.Errorf("mobius: claim: decode action spec: %w", err)
	}
	if action.Kind != api.JobActionSpecKindAction {
		return nil, fmt.Errorf("mobius: claim: unsupported job spec kind %q (this SDK only handles action jobs)", action.Kind)
	}
	var parameters map[string]any
	if action.Parameters != nil {
		parameters = *action.Parameters
	}
	attempt := 1
	if claim.AttemptNumber != nil {
		attempt = *claim.AttemptNumber
	}
	return &runtimeJob{
		JobID:             claim.JobId,
		RunID:             claim.RunId,
		ProjectHandle:     c.projectHandle,
		WorkflowName:      claim.WorkflowName,
		StepName:          claim.StepName,
		Action:            action.Name,
		Parameters:        parameters,
		Attempt:           attempt,
		Queue:             claim.Queue,
		WorkerInstanceID:  cfg.WorkerInstanceID,
		LeaseToken:        claim.LeaseToken,
		HeartbeatInterval: hb,
	}, nil
}

// parseInstanceConflict turns a 409 from /jobs/claim into an
// [InstanceConflictError]. The server wraps errors as
// {"error":{"code":"worker_instance_conflict","message":"…"}}.
// Any other 409 is bubbled up unchanged so the caller can
// retry-or-die normally.
func parseInstanceConflict(body []byte, instanceID, projectHandle string) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil &&
		envelope.Error.Code == "worker_instance_conflict" {
		return &InstanceConflictError{
			WorkerInstanceID: instanceID,
			ProjectHandle:    projectHandle,
			Message:          envelope.Error.Message,
		}
	}
	return fmt.Errorf("mobius: claim: unexpected status 409: %s", strings.TrimSpace(string(body)))
}

// runtimeHeartbeat refreshes the lease on a claimed job and returns
// any directives from the server. Returns ErrLeaseLost on 409, and
// ErrAuthRevoked on 401 so the caller can cancel in-flight work and
// exit the claim loop for the process supervisor to restart.
func (c *Client) runtimeHeartbeat(ctx context.Context, job *runtimeJob) (*api.JobHeartbeatDirectives, error) {
	instanceID := job.WorkerInstanceID
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/v1/projects/%s/jobs/%s/heartbeat", url.PathEscape(c.projectHandle), url.PathEscape(job.JobID)), api.JobFenceRequest{
		WorkerInstanceId: &instanceID,
		LeaseToken:       job.LeaseToken,
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: heartbeat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrAuthRevoked
	}
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

// runtimeCompleteSuccess reports a successful job completion along
// with its action result. The result is JSON-encoded and delivered
// as a base64-encoded blob inside the terminal `complete` outcome.
func (c *Client) runtimeCompleteSuccess(ctx context.Context, job *runtimeJob, result any) error {
	complete := api.OutcomeComplete{Kind: api.OutcomeCompleteKindComplete}
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("mobius: marshal action result: %w", err)
		}
		enc := base64.StdEncoding.EncodeToString(b)
		complete.ResultB64 = &enc
	}
	var outcome api.Outcome
	if err := outcome.FromOutcomeComplete(complete); err != nil {
		return fmt.Errorf("mobius: encode complete outcome: %w", err)
	}
	return c.runtimeReport(ctx, job, []api.Outcome{outcome})
}

// runtimeCompleteFailure reports a failed job with an error message
// and optional error type. The server uses the error type to decide
// whether the job is retryable.
func (c *Client) runtimeCompleteFailure(ctx context.Context, job *runtimeJob, errorType, message string) error {
	fail := api.OutcomeFail{
		Kind:         api.OutcomeFailKindFail,
		ErrorMessage: message,
		ErrorType:    errorType,
	}
	var outcome api.Outcome
	if err := outcome.FromOutcomeFail(fail); err != nil {
		return fmt.Errorf("mobius: encode fail outcome: %w", err)
	}
	return c.runtimeReport(ctx, job, []api.Outcome{outcome})
}

func (c *Client) runtimeReport(ctx context.Context, job *runtimeJob, outcomes []api.Outcome) error {
	instanceID := job.WorkerInstanceID
	req := api.JobReportRequest{
		LeaseToken:       job.LeaseToken,
		WorkerInstanceId: &instanceID,
		Outcomes:         outcomes,
	}
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/v1/projects/%s/jobs/%s/report", url.PathEscape(c.projectHandle), url.PathEscape(job.JobID)), req)
	if err != nil {
		return fmt.Errorf("mobius: report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrAuthRevoked
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrLeaseLost
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mobius: report: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) runtimeEmitEvents(ctx context.Context, job *runtimeJob, events []jobEventEntry) error {
	if len(events) == 0 {
		return nil
	}
	apiEvents := make([]api.JobEventEntry, len(events))
	for i, ev := range events {
		apiEvents[i] = api.JobEventEntry{Type: ev.Type, Payload: ev.Payload}
	}
	instanceID := job.WorkerInstanceID
	resp, err := c.runtimeRequest(ctx, http.MethodPost, fmt.Sprintf("/v1/projects/%s/jobs/%s/events", url.PathEscape(c.projectHandle), url.PathEscape(job.JobID)), api.JobEventsRequest{
		LeaseToken:       job.LeaseToken,
		WorkerInstanceId: &instanceID,
		Events:           apiEvents,
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
