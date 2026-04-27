package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectHandle("test-project"))
	assert.NoError(t, err)
	return c, srv
}

func TestNewClient_DefaultBaseURL(t *testing.T) {
	c, err := NewClient()
	assert.NoError(t, err)
	assert.Equal(t, c.baseURL, DefaultBaseURL)
}

func TestNewClient_WithBaseURLOverride(t *testing.T) {
	c, err := NewClient(WithBaseURL("https://api.example.invalid"))
	assert.NoError(t, err)
	assert.Equal(t, c.baseURL, "https://api.example.invalid")
}

// TestNewClient_ExtractsHandleFromAPIKey covers the self-contained
// credential path: a project-pinned token "mbx_<secret>.<handle>"
// should yield a client with projectHandle already set, and the full
// token stays on the Authorization header so the server can validate.
func TestNewClient_ExtractsHandleFromAPIKey(t *testing.T) {
	c, err := NewClient(WithAPIKey("mbx_secret.prod"))
	assert.NoError(t, err)
	assert.Equal(t, c.projectHandle, "prod")
	assert.Equal(t, c.apiKey, "mbx_secret.prod")
}

func TestNewClient_HandleConflictBetweenFlagAndKey(t *testing.T) {
	_, err := NewClient(WithAPIKey("mbx_secret.prod"), WithProjectHandle("staging"))
	assert.True(t, err != nil)
}

func TestNewClient_InvalidHandleSuffix(t *testing.T) {
	_, err := NewClient(WithAPIKey("mbx_secret.Not_A_Handle"))
	assert.True(t, err != nil)
}

func TestNewClient_RejectsTrailingDotSuffix(t *testing.T) {
	_, err := NewClient(WithAPIKey("mbx_secret."))
	assert.True(t, err != nil)
}

func TestRuntimeClaim_Job(t *testing.T) {
	claimBody := `{"job_id":"job_1","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"print","parameters":{"msg":"hi"},"attempt":1,"queue":"default","heartbeat_interval_seconds":15}`
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/claim": {status: 200, body: claimBody},
	})
	c, _ := newTestClient(t, h)

	cfg := WorkerConfig{WorkerID: "w1", Name: "name", Version: "v1", PollWaitSeconds: 1, Actions: []string{"print"}}
	job, err := c.runtimeClaim(context.Background(), cfg)
	assert.NoError(t, err)
	assert.NotNil(t, job)
	assert.Equal(t, job.JobID, "job_1")
	assert.Equal(t, job.RunID, "run_1")
	assert.Equal(t, job.Action, "print")
	assert.Equal(t, job.Attempt, 1)
	assert.Equal(t, job.Queue, "default")
	assert.Equal(t, job.StepName, "greet")
	assert.Equal(t, job.WorkerID, "w1")
	assert.Equal(t, job.HeartbeatInterval, 15*time.Second)
	assert.Equal(t, h.lastHeader["/v1/projects/test-project/jobs/claim"].Get("Authorization"), "Bearer mbx_test")

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/v1/projects/test-project/jobs/claim"], &sent)
	assert.Equal(t, sent["worker_id"], "w1")
	assert.Equal(t, sent["worker_name"], "name")
	acts, _ := sent["actions"].([]any)
	assert.Equal(t, len(acts), 1)
	assert.Equal(t, acts[0], "print")
}

// Regression: URL-unsafe characters in the project handle / job ID must be
// PathEscape'd before being interpolated into the runtime routes, otherwise
// the request either fails client-side or hits the wrong endpoint.
func TestRuntimeClaim_EscapesProjectHandle(t *testing.T) {
	const rawProject = "team a/b"
	wantEscaped := "/v1/projects/" + url.PathEscape(rawProject) + "/jobs/claim"
	claimBody := `{"job_id":"job_1","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"print","parameters":{},"attempt":1,"queue":"default","heartbeat_interval_seconds":15}`

	var gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, claimBody)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectHandle(rawProject))
	assert.NoError(t, err)

	cfg := WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1, Actions: []string{"print"}}
	job, err := c.runtimeClaim(context.Background(), cfg)
	assert.NoError(t, err)
	assert.NotNil(t, job)
	assert.Equal(t, job.JobID, "job_1")
	assert.Equal(t, gotEscaped, wantEscaped)
}

func TestRuntimeHeartbeat_EscapesJobID(t *testing.T) {
	const rawJob = "job/with spaces"
	wantEscaped := "/v1/projects/test-project/jobs/" + url.PathEscape(rawJob) + "/heartbeat"

	var gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"lease_expires_at":"2026-01-01T00:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectHandle("test-project"))
	assert.NoError(t, err)

	job := &runtimeJob{JobID: rawJob, WorkerID: "w1", Attempt: 1}
	_, err = c.runtimeHeartbeat(context.Background(), job)
	assert.NoError(t, err)
	assert.Equal(t, gotEscaped, wantEscaped)
}

func TestRuntimeClaim_Empty(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/claim": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	job, err := c.runtimeClaim(context.Background(), WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1})
	assert.NoError(t, err)
	assert.Nil(t, job)
}

func TestRuntimeHeartbeat_Directives(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 200, body: `{"ok":true,"directives":{"should_cancel":true}}`},
	})
	c, _ := newTestClient(t, h)
	job := &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"}
	dirs, err := c.runtimeHeartbeat(context.Background(), job)
	assert.NoError(t, err)
	assert.NotNil(t, dirs)
	assert.NotNil(t, dirs.ShouldCancel)
	assert.True(t, *dirs.ShouldCancel)
}

func TestRuntimeHeartbeat_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	_, err := c.runtimeHeartbeat(context.Background(), &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"})
	assert.ErrorIs(t, err, ErrLeaseLost)
}

// TestRuntimeHeartbeat_AuthRevoked covers the 401 → ErrAuthRevoked
// surface that the worker loop uses to detect mid-execution credential
// revocation. The worker.heartbeatLoop latches on this and cancels
// the action; Run returns non-zero so a process supervisor can restart
// under a rotated credential.
func TestRuntimeHeartbeat_AuthRevoked(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 401, body: ""},
	})
	c, _ := newTestClient(t, h)
	_, err := c.runtimeHeartbeat(context.Background(), &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"})
	assert.ErrorIs(t, err, ErrAuthRevoked)
}

func TestRuntimeComplete_AuthRevoked(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 401, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteSuccess(context.Background(), &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"}, nil)
	assert.ErrorIs(t, err, ErrAuthRevoked)
}

func TestRuntimeClaim_AuthRevoked(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/claim": {status: 401, body: ""},
	})
	c, _ := newTestClient(t, h)
	cfg := WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1, Actions: []string{"print"}}
	_, err := c.runtimeClaim(context.Background(), cfg)
	assert.ErrorIs(t, err, ErrAuthRevoked)
}

func TestRuntimeCompleteSuccess(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	job := &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"}
	err := c.runtimeCompleteSuccess(context.Background(), job, map[string]any{"ok": true})
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/v1/projects/test-project/jobs/"], &sent)
	assert.Equal(t, sent["status"], "completed")
	assert.NotNil(t, sent["result_b64"])
}

func TestRuntimeCompleteFailure_LeaseLost(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 409, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"}, "Error", "boom")
	assert.ErrorIs(t, err, ErrLeaseLost)
}

func TestRuntimeCompleteFailure_Body(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	err := c.runtimeCompleteFailure(context.Background(), &runtimeJob{JobID: "job_1", Attempt: 2, WorkerID: "w1"}, "Timeout", "deadline exceeded")
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/v1/projects/test-project/jobs/"], &sent)
	assert.Equal(t, sent["status"], "failed")
	assert.Equal(t, sent["error_type"], "Timeout")
	assert.Equal(t, sent["error_message"], "deadline exceeded")
}

func TestRuntimeEmitEvents(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/job_1/events": {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	job := &runtimeJob{JobID: "job_1", Attempt: 1, WorkerID: "w1"}
	err := c.runtimeEmitEvents(context.Background(), job, []jobEventEntry{
		{Type: "scrape.page_done", Payload: map[string]any{"url": "https://example.com"}},
	})
	assert.NoError(t, err)

	var sent map[string]any
	_ = json.Unmarshal(h.lastBody["/v1/projects/test-project/jobs/job_1/events"], &sent)
	assert.Equal(t, sent["worker_id"], "w1")
	assert.Equal(t, sent["attempt"], float64(1))
	events, _ := sent["events"].([]any)
	assert.Len(t, events, 1)
	first, _ := events[0].(map[string]any)
	assert.Equal(t, first["type"], "scrape.page_done")
}

func TestWorkerExecuteJob_EmitsCustomEvents(t *testing.T) {
	h := newRecorder(t, map[string]stubResponse{
		"/v1/projects/test-project/jobs/job_1/complete": {status: 204, body: ""},
		"/v1/projects/test-project/jobs/job_1/events":   {status: 204, body: ""},
	})
	c, _ := newTestClient(t, h)
	w := c.NewWorker(WorkerConfig{WorkerID: "w1", EventBatchSize: 10})
	w.Register(ActionFunc("print", func(ctx Context, params map[string]any) (any, error) {
		ctx.EmitEvent("scrape.started", map[string]any{"url": params["url"]})
		return map[string]any{"ok": true}, nil
	}))

	w.executeJob(context.Background(), &runtimeJob{
		JobID:             "job_1",
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
	_ = json.Unmarshal(h.lastBody["/v1/projects/test-project/jobs/job_1/events"], &sent)
	events, _ := sent["events"].([]any)
	assert.Len(t, events, 1)
	first, _ := events[0].(map[string]any)
	assert.Equal(t, first["type"], "scrape.started")
}

func TestWorkerRun_ClaimsNextJobOnlyAfterCurrentCompletes(t *testing.T) {
	var claims atomic.Int32
	extraClaim := make(chan struct{}, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/projects/test-project/jobs/claim":
			n := claims.Add(1)
			if n == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"job_id":"job_1","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"block","parameters":{},"attempt":1,"queue":"default","heartbeat_interval_seconds":3600}`)
				return
			}
			select {
			case extraClaim <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/projects/test-project/jobs/job_1/complete":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := newTestClient(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	worker := c.NewWorker(WorkerConfig{WorkerID: "w1", PollWaitSeconds: 1})
	worker.Register(ActionFunc("block", func(ctx Context, params map[string]any) (any, error) {
		close(started)
		<-release
		cancel()
		return map[string]any{"ok": true}, nil
	}))

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start executing first job")
	}
	select {
	case <-extraClaim:
		t.Fatal("worker claimed another job before the current job completed")
	case <-time.After(50 * time.Millisecond):
	}
	assert.Equal(t, int(claims.Load()), 1)
	close(release)
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after release")
	}
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestWorkerPool_RunUsesDistinctSingleJobWorkers(t *testing.T) {
	var mu sync.Mutex
	var claims int
	workerIDs := map[string]bool{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/projects/test-project/jobs/claim":
			var sent map[string]any
			_ = json.NewDecoder(r.Body).Decode(&sent)
			mu.Lock()
			claims++
			n := claims
			if id, ok := sent["worker_id"].(string); ok {
				workerIDs[id] = true
			}
			mu.Unlock()
			if n <= 3 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"job_id":"job_%d","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"block","parameters":{},"attempt":1,"queue":"default","heartbeat_interval_seconds":3600}`, n))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/v1/projects/test-project/jobs/job_") && strings.HasSuffix(r.URL.Path, "/complete"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := newTestClient(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	pool := c.NewWorkerPool(WorkerPoolConfig{
		WorkerConfig:   WorkerConfig{PollWaitSeconds: 1},
		Count:          3,
		WorkerIDPrefix: "pool-worker",
	})
	pool.Register(ActionFunc("block", func(ctx Context, params map[string]any) (any, error) {
		started <- struct{}{}
		<-release
		return map[string]any{"ok": true}, nil
	}))

	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("pool worker did not start executing job")
		}
	}
	mu.Lock()
	assert.Equal(t, claims, 3)
	assert.Equal(t, len(workerIDs), 3)
	assert.True(t, workerIDs["pool-worker-1"])
	assert.True(t, workerIDs["pool-worker-2"])
	assert.True(t, workerIDs["pool-worker-3"])
	mu.Unlock()

	close(release)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("worker pool did not stop after release")
	}
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestWorkerPool_RunReturnsAuthRevokedAndCancelsSiblings(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var startOnce sync.Once
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/projects/test-project/jobs/claim":
			var sent map[string]any
			_ = json.NewDecoder(r.Body).Decode(&sent)
			switch sent["worker_id"] {
			case "pool-worker-1":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"job_id":"job_1","run_id":"run_1","workflow_name":"hello","step_name":"greet","action":"block","parameters":{},"attempt":1,"queue":"default","heartbeat_interval_seconds":3600}`)
			case "pool-worker-2":
				<-started
				w.WriteHeader(http.StatusUnauthorized)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case r.URL.Path == "/v1/projects/test-project/jobs/job_1/complete":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := newTestClient(t, h)

	pool := c.NewWorkerPool(WorkerPoolConfig{
		WorkerConfig:   WorkerConfig{PollWaitSeconds: 1},
		Count:          2,
		WorkerIDPrefix: "pool-worker",
	})
	pool.Register(ActionFunc("block", func(ctx Context, params map[string]any) (any, error) {
		startOnce.Do(func() { close(started) })
		<-ctx.Done()
		close(cancelled)
		return map[string]any{"ok": true}, nil
	}))

	done := make(chan error, 1)
	go func() { done <- pool.Run(context.Background()) }()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("sibling worker was not cancelled after credential revocation")
	}
	err := <-done
	assert.True(t, errors.Is(err, ErrAuthRevoked))
}
