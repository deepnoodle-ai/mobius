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

// Socket liveness bounds. Every write carries a deadline so a stalled TCP peer
// (half-open connection, or the guest resuming after a Sprite pause dropped the
// remote end without a FIN) can never block a write indefinitely while holding
// the socket mutex — which would freeze claims and heartbeats until the OS TCP
// stack gave up. Reads are bounded by a rolling deadline extended by pongs and
// by any inbound frame: the worker pings on socketPingInterval, so a peer that
// stays silent past socketPongWait fails the read and triggers a reconnect.
const (
	socketWriteTimeout = 10 * time.Second
	socketPingInterval = 20 * time.Second
	socketPongWait     = 65 * time.Second
	// registerReadTimeout bounds how long the worker waits for the
	// worker.registered response; a server that accepts the upgrade but never
	// answers must not hang the worker forever.
	registerReadTimeout = 30 * time.Second
)

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
	_ = s.conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout))
	return s.conn.WriteJSON(v)
}

// ping sends a WebSocket ping control frame. The peer's pong (or any inbound
// frame) extends the read deadline; a peer silent past socketPongWait fails
// the read loop and the worker reconnects.
func (s *workerSocket) ping() error {
	return s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(socketWriteTimeout))
}

// extendReadDeadline rolls the read deadline forward; called for pongs and for
// every successfully read frame (inbound data proves the peer is alive even if
// pongs are dropped by an intermediary).
func (s *workerSocket) extendReadDeadline() {
	_ = s.conn.SetReadDeadline(time.Now().Add(socketPongWait))
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
	s := &workerSocket{conn: conn}
	s.extendReadDeadline()
	conn.SetPongHandler(func(string) error {
		s.extendReadDeadline()
		return nil
	})
	return s, resp, nil
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
		s.extendReadDeadline()
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

// The terminal report, heartbeat, and generation-delta frames are built here
// but sent by the Worker through whatever socket is currently live, not a
// captured connection. A job survives socket reconnects (see worker.go), so
// its lease-fenced frames must target the worker's current socket rather than
// the one it was claimed on. Terminal reports are idempotent server-side, so
// re-delivering after a reconnect is safe.

func successReportFrame(job *runtimeJob, result any) api.WorkerSocketJobReportFrame {
	status := api.WorkerSocketJobReportFrameStatusCompleted
	body := map[string]any{"output": result}
	if m, ok := result.(map[string]any); ok {
		body = m
	}
	return api.WorkerSocketJobReportFrame{
		Type:       api.WorkerSocketJobReportFrameTypeJobReport,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
		Status:     &status,
		Result:     &body,
	}
}

func failureReportFrame(job *runtimeJob, errorType, message string) api.WorkerSocketJobReportFrame {
	status := api.WorkerSocketJobReportFrameStatusFailed
	return api.WorkerSocketJobReportFrame{
		Type:         api.WorkerSocketJobReportFrameTypeJobReport,
		MessageId:    messageIDPtr(),
		JobId:        job.JobID,
		LeaseToken:   job.LeaseToken,
		Status:       &status,
		ErrorType:    strPtr(errorType),
		ErrorMessage: strPtr(message),
	}
}

// cancelledReportFrame reports a job that ended because its context was
// cancelled (server cancel directive or worker shutdown). The protocol's
// distinct `cancelled` status keeps cancellations out of failure metrics;
// error_type "Cancelled" is retained alongside it for consumers that key off
// the error taxonomy.
func cancelledReportFrame(job *runtimeJob, message string) api.WorkerSocketJobReportFrame {
	status := api.WorkerSocketJobReportFrameStatusCancelled
	return api.WorkerSocketJobReportFrame{
		Type:         api.WorkerSocketJobReportFrameTypeJobReport,
		MessageId:    messageIDPtr(),
		JobId:        job.JobID,
		LeaseToken:   job.LeaseToken,
		Status:       &status,
		ErrorType:    strPtr("Cancelled"),
		ErrorMessage: strPtr(message),
	}
}

func heartbeatFrame(job *runtimeJob) api.WorkerSocketJobHeartbeatFrame {
	return api.WorkerSocketJobHeartbeatFrame{
		Type:       api.WorkerSocketJobHeartbeatFrameTypeJobHeartbeat,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
	}
}

func generationDeltaFrame(job *runtimeJob, sequence int64, delta map[string]any) api.WorkerSocketGenerationDeltaFrame {
	if delta == nil {
		delta = map[string]any{}
	}
	return api.WorkerSocketGenerationDeltaFrame{
		Type:       api.WorkerSocketGenerationDeltaFrameTypeGenerationDelta,
		MessageId:  messageIDPtr(),
		JobId:      job.JobID,
		LeaseToken: job.LeaseToken,
		Sequence:   sequence,
		Delta:      delta,
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stringPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
