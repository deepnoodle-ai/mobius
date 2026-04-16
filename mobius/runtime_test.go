package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	c := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithNamespaceSlug("test-ns"))
	return c, srv
}

func TestNewClient_DefaultBaseURL(t *testing.T) {
	c := NewClient()
	if c.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
}

func TestNewClient_WithBaseURLOverride(t *testing.T) {
	c := NewClient(WithBaseURL("https://api.example.invalid"))
	if c.baseURL != "https://api.example.invalid" {
		t.Fatalf("baseURL = %q, want override", c.baseURL)
	}
}

func TestRuntimeClaim_Task(t *testing.T) {
	claimBody := `{"data":{"job_id":"task_1","run_id":"run_1","namespace_id":"ns_1","workflow_name":"hello","step_name":"greet","action":"print","parameters":{"msg":"hi"},"attempt":1,"queue":"default","heartbeat_interval_seconds":15}}`
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/claim": {status: 200, body: claimBody},
	})
	c, _ := newTestClient(t, h)

	cfg := WorkerConfig{WorkerID: "w1", Name: "name", Version: "v1", PollWaitSeconds: 1, Actions: []string{"print"}}
	task, err := c.runtimeClaim(context.Background(), cfg)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil {
		t.Fatal("expected task, got nil")
	}
	if task.TaskID != "task_1" || task.RunID != "run_1" || task.Action != "print" {
		t.Errorf("task = %+v", task)
	}
	if task.Attempt != 1 || task.Queue != "default" || task.StepName != "greet" {
		t.Errorf("task fields: %+v", task)
	}
	if task.WorkerID != "w1" {
		t.Errorf("worker id = %q, want w1", task.WorkerID)
	}
	if task.HeartbeatInterval.Seconds() != 15 {
		t.Errorf("heartbeat interval = %v, want 15s", task.HeartbeatInterval)
	}
	if got := h.lastHeader["/namespaces/test-ns/jobs/claim"].Get("Authorization"); got != "Bearer mbx_test" {
		t.Errorf("auth header = %q", got)
	}
	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/namespaces/test-ns/jobs/claim"], &sent)
	data, _ := sent["data"].(map[string]any)
	if data == nil {
		t.Fatalf("claim body missing data envelope: %v", sent)
	}
	if data["worker_id"] != "w1" || data["worker_name"] != "name" {
		t.Errorf("claim body = %v", data)
	}
	if acts, _ := data["actions"].([]any); len(acts) != 1 || acts[0] != "print" {
		t.Errorf("actions filter not forwarded: %v", data["actions"])
	}
}

func TestRuntimeClaim_Empty(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/claim": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	task, err := c.runtimeClaim(context.Background(), WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil task, got %+v", task)
	}
}

func TestRuntimeHeartbeat_Directives(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/": {status: 200, body: `{"data":{"ok":true,"directives":{"should_cancel":true}}}`},
	})
	c, _ := newTestClient(t, h)
	task := &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}
	dirs, err := c.runtimeHeartbeat(context.Background(), task)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if dirs == nil || dirs.ShouldCancel == nil || !*dirs.ShouldCancel {
		t.Errorf("expected should_cancel=true, got %+v", dirs)
	}
}

func TestRuntimeHeartbeat_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	_, err := c.runtimeHeartbeat(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"})
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
}

func TestRuntimeCompleteSuccess(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	task := &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}
	err := c.runtimeCompleteSuccess(context.Background(), task, map[string]any{"ok": true})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/namespaces/test-ns/jobs/"], &sent)
	data, _ := sent["data"].(map[string]any)
	if data == nil {
		t.Fatalf("complete body missing data envelope: %v", sent)
	}
	if data["status"] != "completed" {
		t.Errorf("status = %v", data["status"])
	}
	if data["result_b64"] == nil {
		t.Errorf("expected result_b64 in body, got %v", data)
	}
}

func TestRuntimeCompleteFailure_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 1, WorkerID: "w1"}, "Error", "boom")
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
}

func TestRuntimeCompleteFailure_Body(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/namespaces/test-ns/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeTask{TaskID: "task_1", Attempt: 2, WorkerID: "w1"}, "Timeout", "deadline exceeded")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/namespaces/test-ns/jobs/"], &sent)
	data, _ := sent["data"].(map[string]any)
	if data == nil {
		t.Fatalf("complete body missing data envelope: %v", sent)
	}
	if data["status"] != "failed" {
		t.Errorf("status = %v", data["status"])
	}
	if data["error_type"] != "Timeout" || data["error_message"] != "deadline exceeded" {
		t.Errorf("error fields = %v", data)
	}
}
