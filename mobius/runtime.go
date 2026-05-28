package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ErrLeaseLost is returned when the server rejects a job heartbeat or report
// because this worker no longer owns the active lease.
var ErrLeaseLost = errors.New("mobius: lease lost")

type runtimeJob struct {
	JobID             string
	RunID             string
	ProjectHandle     string
	EnvironmentID     string
	StepID            string
	AgentTurnID       string
	SessionID         string
	ToolCallID        string
	Kind              api.WorkerSocketClaimedJobKind
	Action            string
	Provider          string
	Model             string
	Spec              map[string]any
	Parameters        map[string]any
	Attempt           int
	Queue             string
	WorkerInstanceID  string
	LeaseToken        string
	HeartbeatInterval time.Duration
}

type socketEnvelope struct {
	Type      string          `json:"type"`
	MessageID string          `json:"message_id,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type workerSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *workerSocket) close() {
	if s == nil || s.conn == nil {
		return
	}
	_ = s.conn.Close()
}

func (s *workerSocket) writeJSON(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(v)
}

func (c *Client) dialWorkerSocket(ctx context.Context) (*workerSocket, *http.Response, error) {
	u, err := c.workerSocketURL()
	if err != nil {
		return nil, nil, err
	}
	header := http.Header{}
	if c.apiKey != "" {
		header.Set("Authorization", "Bearer "+c.apiKey)
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u, header)
	if err != nil {
		return nil, resp, err
	}
	return &workerSocket{conn: conn}, resp, nil
}

func (c *Client) workerSocketURL() (string, error) {
	if c.projectHandle == "" {
		return "", fmt.Errorf("mobius: worker socket: no project configured - set MOBIUS_PROJECT or pass --project")
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("mobius: worker socket: invalid base URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https", "":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("mobius: worker socket: unsupported URL scheme %q", u.Scheme)
	}
	base := strings.TrimRight(u.Path, "/")
	escapedBase := strings.TrimRight(u.EscapedPath(), "/")
	u.Path = base + "/v1/projects/" + c.projectHandle + "/workers/socket"
	u.RawPath = escapedBase + "/v1/projects/" + url.PathEscape(c.projectHandle) + "/workers/socket"
	u.RawQuery = ""
	return u.String(), nil
}

func readSocketFrame(ctx context.Context, s *workerSocket, out chan<- socketEnvelope, errCh chan<- error) {
	defer close(out)
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
			default:
				errCh <- err
			}
			return
		}
		var env socketEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			errCh <- fmt.Errorf("mobius: worker socket: decode frame: %w", err)
			return
		}
		env.Raw = append(json.RawMessage(nil), raw...)
		select {
		case out <- env:
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		}
	}
}

func claimedRuntimeJob(projectHandle, workerID, environmentID string, j api.WorkerSocketClaimedJob) *runtimeJob {
	params := map[string]any{}
	if raw, ok := j.Spec["parameters"].(map[string]any); ok {
		params = raw
	} else if raw, ok := j.Spec["parameters"].(map[string]interface{}); ok {
		params = map[string]any(raw)
	}
	actionName := stringPtrValue(j.ActionName)
	if actionName == "" {
		actionName, _ = j.Spec["action_name"].(string)
	}
	if claimedEnvironmentID := stringPtrValue(j.EnvironmentId); claimedEnvironmentID != "" {
		environmentID = claimedEnvironmentID
	}
	return &runtimeJob{
		JobID:             j.Id,
		RunID:             stringPtrValue(j.RunId),
		ProjectHandle:     projectHandle,
		EnvironmentID:     environmentID,
		StepID:            stringPtrValue(j.StepId),
		AgentTurnID:       stringPtrValue(j.AgentTurnId),
		SessionID:         stringPtrValue(j.SessionId),
		ToolCallID:        stringPtrValue(j.ToolCallId),
		Kind:              j.Kind,
		Action:            actionName,
		Provider:          stringPtrValue(j.Provider),
		Model:             stringPtrValue(j.Model),
		Spec:              j.Spec,
		Parameters:        params,
		Attempt:           j.ClaimAttempt,
		Queue:             j.Queue,
		WorkerInstanceID:  workerID,
		LeaseToken:        j.LeaseToken,
		HeartbeatInterval: time.Duration(j.HeartbeatCadenceSeconds) * time.Second,
	}
}

func socketReportSuccess(s *workerSocket, job *runtimeJob, result any) error {
	status := api.WorkerSocketJobReportFrameStatusCompleted
	body := map[string]any{"output": result}
	if m, ok := result.(map[string]any); ok {
		body = m
	}
	return s.writeJSON(api.WorkerSocketJobReportFrame{
		Type:       api.WorkerSocketJobReportFrameTypeJobReport,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
		Status:     &status,
		Result:     &body,
	})
}

func socketReportFailure(s *workerSocket, job *runtimeJob, errorType, message string) error {
	status := api.WorkerSocketJobReportFrameStatusFailed
	return s.writeJSON(api.WorkerSocketJobReportFrame{
		Type:         api.WorkerSocketJobReportFrameTypeJobReport,
		MessageId:    messageIDPtr(),
		JobId:        job.JobID,
		LeaseToken:   job.LeaseToken,
		Status:       &status,
		ErrorType:    strPtr(errorType),
		ErrorMessage: strPtr(message),
	})
}

func socketHeartbeat(s *workerSocket, job *runtimeJob) error {
	return s.writeJSON(api.WorkerSocketJobHeartbeatFrame{
		Type:       api.WorkerSocketJobHeartbeatFrameTypeJobHeartbeat,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
	})
}

func socketGenerationDelta(s *workerSocket, job *runtimeJob, sequence int64, delta map[string]any) error {
	if delta == nil {
		delta = map[string]any{}
	}
	return s.writeJSON(api.WorkerSocketGenerationDeltaFrame{
		Type:       api.WorkerSocketGenerationDeltaFrameTypeGenerationDelta,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
		Sequence:   sequence,
		Delta:      delta,
	})
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func messageIDPtr() *api.WorkerSocketMessageID {
	id := api.WorkerSocketMessageID("msg_" + uuid.NewString())
	return &id
}

func workerSocketMessageIDValue(id *api.WorkerSocketMessageID) string {
	if id == nil {
		return ""
	}
	return string(*id)
}
