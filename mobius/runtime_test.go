package mobius

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

// --- runtime HTTP paths ----------------------------------------------------

// recordingHandler captures the last request body and responds with the
// stubbed status/body for a specific path prefix.
type recordingHandler struct {
	t          *testing.T
	routes     map[string]stubResponse
	lastBody   map[string][]byte
	lastHeader map[string]http.Header
}

type stubResponse struct {
	status int
	body   string
}

func newRecorder(t *testing.T, routes map[string]stubResponse) *recordingHandler {
	return &recordingHandler{
		t:          t,
		routes:     routes,
		lastBody:   map[string][]byte{},
		lastHeader: map[string]http.Header{},
	}
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for prefix, resp := range h.routes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			body, _ := io.ReadAll(r.Body)
			h.lastBody[prefix] = body
			h.lastHeader[prefix] = r.Header.Clone()
			if resp.body != "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(resp.status)
			_, _ = io.WriteString(w, resp.body)
			return
		}
	}
	http.NotFound(w, r)
}

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectSlug("test-project"))
	return c, srv
}

func TestNewClient_DefaultBaseURL(t *testing.T) {
	c := NewClient()
	assert.Equal(t, c.baseURL, DefaultBaseURL)
}

func TestNewClient_WithBaseURLOverride(t *testing.T) {
	c := NewClient(WithBaseURL("https://api.example.invalid"))
	assert.Equal(t, c.baseURL, "https://api.example.invalid")
}

func TestRuntimeClaim_Task(t *testing.T) {
	claimBody := `{"job_id":"task_1","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"print","parameters":{"msg":"hi"},"attempt":1,"queue":"default","heartbeat_interval_seconds":15}`
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/claim": {status: 200, body: claimBody},
	})
	c, _ := newTestClient(t, h)

	cfg := WorkerConfig{WorkerID: "w1", Name: "name", Version: "v1", PollWaitSeconds: 1, Actions: []string{"print"}}
	task, err := c.runtimeClaim(context.Background(), cfg)
	assert.NoError(t, err)
	assert.NotNil(t, task)
	assert.Equal(t, task.TaskID, "task_1")
	assert.Equal(t, task.RunID, "run_1")
	assert.Equal(t, task.Action, "print")
	assert.Equal(t, task.Attempt, 1)
	assert.Equal(t, task.Queue, "default")
	assert.Equal(t, task.StepName, "greet")
	assert.Equal(t, task.WorkerID, "w1")
	assert.Equal(t, task.HeartbeatInterval, 15*time.Second)
	assert.Equal(t, h.lastHeader["/projects/test-project/jobs/claim"].Get("Authorization"), "Bearer mbx_test")

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/projects/test-project/jobs/claim"], &sent)
	assert.Equal(t, sent["worker_id"], "w1")
	assert.Equal(t, sent["worker_name"], "name")
	acts, _ := sent["actions"].([]any)
	assert.Equal(t, len(acts), 1)
	assert.Equal(t, acts[0], "print")
}

func TestRuntimeClaim_Empty(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/claim": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	task, err := c.runtimeClaim(context.Background(), WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1})
	assert.NoError(t, err)
	assert.Nil(t, task)
}

func TestRuntimeHeartbeat_Directives(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/": {status: 200, body: `{"ok":true,"directives":{"should_cancel":true}}`},
	})
	c, _ := newTestClient(t, h)
	task := &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}
	dirs, err := c.runtimeHeartbeat(context.Background(), task)
	assert.NoError(t, err)
	assert.NotNil(t, dirs)
	assert.NotNil(t, dirs.ShouldCancel)
	assert.True(t, *dirs.ShouldCancel)
}

func TestRuntimeHeartbeat_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	_, err := c.runtimeHeartbeat(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"})
	assert.ErrorIs(t, err, ErrLeaseLost)
}

func TestRuntimeCompleteSuccess(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	task := &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}
	err := c.runtimeCompleteSuccess(context.Background(), task, map[string]any{"ok": true})
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/projects/test-project/jobs/"], &sent)
	assert.Equal(t, sent["status"], "completed")
	assert.NotNil(t, sent["result_b64"])
}

func TestRuntimeCompleteFailure_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}, "Error", "boom")
	assert.ErrorIs(t, err, ErrLeaseLost)
}

func TestRuntimeCompleteFailure_Body(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 2, WorkerID: "w1"}, "Timeout", "deadline exceeded")
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/projects/test-project/jobs/"], &sent)
	assert.Equal(t, sent["status"], "failed")
	assert.Equal(t, sent["error_type"], "Timeout")
	assert.Equal(t, sent["error_message"], "deadline exceeded")
}

func TestRuntimeEmitEvents(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/task_1/events": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	task := &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}
	err := c.runtimeEmitEvents(context.Background(), task, []jobEventEntry{
		{Type: "scrape.page_done", Payload: map[string]any{"url": "https://example.com"}},
	})
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/projects/test-project/jobs/task_1/events"], &sent)
	assert.Equal(t, sent["worker_id"], "w1")
	assert.Equal(t, sent["attempt"], float64(1))
	events, _ := sent["events"].([]any)
	assert.Len(t, events, 1)
	first, _ := events[0].(map[string]any)
	assert.Equal(t, first["type"], "scrape.page_done")
}

func TestWorkerExecuteTask_EmitsCustomEvents(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/projects/test-project/jobs/task_1/complete": {status: 204, body: ""},
		"/projects/test-project/jobs/task_1/events":   {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	w := c.NewWorker(WorkerConfig{WorkerID: "w1", EventBatchSize: 10})
	w.Register(ActionFunc("print", func(ctx Context, params map[string]any) (any, error) {
		ctx.EmitEvent("scrape.started", map[string]any{"url": params["url"]})
		return map[string]any{"ok": true}, nil
	}))

	w.executeTask(context.Background(), &runtimeTask{
		TaskID:            "task_1",
		RunID:             "run_1",
		ProjectID:         "prj_1",
		WorkflowName:      "hello",
		StepName:          "greet",
		Action:            "print",
		Parameters:        map[string]any{"url": "https://example.com"},
		Attempt:           1,
		Queue:             "default",
		WorkerID:          "w1",
		HeartbeatInterval: time.Hour,
	})

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/projects/test-project/jobs/task_1/events"], &sent)
	events, _ := sent["events"].([]any)
	assert.Len(t, events, 1)
	first, _ := events[0].(map[string]any)
	assert.Equal(t, first["type"], "scrape.started")
}
