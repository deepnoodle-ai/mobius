package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/gorilla/websocket"
)

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

func TestWorkerSocketURL_EscapesProjectHandleAndSwitchesScheme(t *testing.T) {
	c, err := NewClient(WithBaseURL("https://api.example.test/base/"), WithAPIKey("mbx_test"), WithProjectHandle("team a/b"))
	assert.NoError(t, err)

	got, err := c.workerSocketURL()
	assert.NoError(t, err)

	u, err := url.Parse(got)
	assert.NoError(t, err)
	assert.Equal(t, u.Scheme, "wss")
	assert.Equal(t, u.EscapedPath(), "/base/v1/projects/team%20a%2Fb/workers/socket")
}

func TestWorkerRun_ExecutesActionJobOverWebSocket(t *testing.T) {
	reported := make(chan api.WorkerSocketJobReportFrame, 1)
	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		expectRegister(t, conn, "echo", nil)
		claim := expectClaim(t, conn)
		assert.Equal(t, *claim.AvailableSlots, 2)
		sendClaimed(t, conn, api.WorkerSocketClaimedJob{
			Id:                      "job_1",
			Kind:                    api.WorkerSocketClaimedJobKindActionExecution,
			Origin:                  api.WorkerSocketClaimedJobOriginAutomationActionStep,
			ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
			ActionName:              strPtr("echo"),
			Queue:                   "default",
			RunId:                   strPtr("run_1"),
			StepId:                  strPtr("step_1"),
			ClaimAttempt:            1,
			LeaseToken:              "lease-1",
			HeartbeatCadenceSeconds: 60,
			LeaseDurationSeconds:    180,
			Spec: map[string]any{
				"parameters": map[string]any{"msg": "hi"},
			},
		})
		for {
			env := readTestFrame(t, conn)
			if env.Type != "job.report" {
				continue
			}
			var report api.WorkerSocketJobReportFrame
			assert.NoError(t, json.Unmarshal(env.Raw, &report))
			reported <- report
			return
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := c.NewWorker(WorkerConfig{WorkerInstanceID: "w1", Concurrency: 2, HeartbeatInterval: time.Hour})
	worker.Register(ActionFunc("echo", func(ctx Context, params map[string]any) (any, error) {
		assert.Equal(t, ctx.JobID(), "job_1")
		return map[string]any{"echo": params["msg"]}, nil
	}))

	done := make(chan error, 1)
	go func() { done <- worker.runSocket(ctx) }()

	select {
	case report := <-reported:
		cancel()
		assert.NotNil(t, report.Status)
		assert.Equal(t, *report.Status, api.WorkerSocketJobReportFrameStatusCompleted)
		assert.NotNil(t, report.Result)
		assert.Equal(t, (*report.Result)["echo"], "hi")
	case <-time.After(time.Second):
		t.Fatal("worker did not report action result")
	}
	select {
	case err := <-done:
		assert.True(t, errors.Is(err, context.Canceled) || err == nil || errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerRun_StreamsGenerationDeltaAndReportsTerminalResult(t *testing.T) {
	deltaSeen := make(chan api.WorkerSocketGenerationDeltaFrame, 1)
	reported := make(chan api.WorkerSocketJobReportFrame, 1)
	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		expectRegister(t, conn, "", []api.WorkerSocketModelCapability{{Provider: "ollama", Model: "llama3"}})
		expectClaim(t, conn)
		sendClaimed(t, conn, api.WorkerSocketClaimedJob{
			Id:                      "job_gen_1",
			Kind:                    api.WorkerSocketClaimedJobKindLlmGeneration,
			Origin:                  api.WorkerSocketClaimedJobOriginAgentLlmCall,
			ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
			Provider:                strPtr("ollama"),
			Model:                   strPtr("llama3"),
			Queue:                   "default",
			RunId:                   strPtr("run_1"),
			AgentTurnId:             strPtr("turn_1"),
			ClaimAttempt:            1,
			LeaseToken:              "lease-gen-1",
			HeartbeatCadenceSeconds: 60,
			LeaseDurationSeconds:    180,
			Spec:                    map[string]any{"prompt": "hello"},
		})
		for {
			env := readTestFrame(t, conn)
			switch env.Type {
			case "generation.delta":
				var delta api.WorkerSocketGenerationDeltaFrame
				assert.NoError(t, json.Unmarshal(env.Raw, &delta))
				deltaSeen <- delta
			case "job.report":
				var report api.WorkerSocketJobReportFrame
				assert.NoError(t, json.Unmarshal(env.Raw, &report))
				reported <- report
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := c.NewWorker(WorkerConfig{
		WorkerInstanceID:  "w-gen",
		Models:            []ModelCapability{{Provider: "ollama", Model: "llama3"}},
		HeartbeatInterval: time.Hour,
	})
	worker.RegisterGenerator("ollama", "llama3", func(ctx Context, job GenerationJob, emit GenerationEmitter) (map[string]any, error) {
		assert.Equal(t, job.Provider, "ollama")
		assert.NoError(t, emit(map[string]any{"text": "hel"}))
		return map[string]any{"text": "hello"}, nil
	})

	done := make(chan error, 1)
	go func() { done <- worker.runSocket(ctx) }()

	select {
	case delta := <-deltaSeen:
		assert.Equal(t, delta.Sequence, int64(1))
		assert.Equal(t, delta.Delta["text"], "hel")
	case <-time.After(time.Second):
		t.Fatal("worker did not stream generation delta")
	}
	select {
	case report := <-reported:
		cancel()
		assert.NotNil(t, report.Status)
		assert.Equal(t, *report.Status, api.WorkerSocketJobReportFrameStatusCompleted)
		assert.Equal(t, (*report.Result)["text"], "hello")
	case <-time.After(time.Second):
		t.Fatal("worker did not report generation result")
	}
	<-done
}

func TestWorkerRun_HeartbeatCancelDirectiveCancelsJob(t *testing.T) {
	reported := make(chan api.WorkerSocketJobReportFrame, 1)
	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		expectRegister(t, conn, "block", nil)
		expectClaim(t, conn)
		sendClaimed(t, conn, api.WorkerSocketClaimedJob{
			Id:                      "job_cancel_1",
			Kind:                    api.WorkerSocketClaimedJobKindActionExecution,
			Origin:                  api.WorkerSocketClaimedJobOriginAutomationActionStep,
			ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
			ActionName:              strPtr("block"),
			Queue:                   "default",
			RunId:                   strPtr("run_1"),
			ClaimAttempt:            1,
			LeaseToken:              "lease-cancel-1",
			HeartbeatCadenceSeconds: 1,
			LeaseDurationSeconds:    30,
			Spec:                    map[string]any{"parameters": map[string]any{}},
		})
		for {
			env := readTestFrame(t, conn)
			switch env.Type {
			case "job.heartbeat":
				_ = conn.WriteJSON(api.WorkerSocketJobHeartbeatAckFrame{
					Type:  api.WorkerSocketJobHeartbeatAckFrameTypeJobHeartbeatAck,
					JobId: "job_cancel_1",
					Cancel: &api.WorkerSocketCancelDirective{
						JobId:  "job_cancel_1",
						Reason: strPtr("test requested cancel"),
					},
				})
			case "job.report":
				var report api.WorkerSocketJobReportFrame
				assert.NoError(t, json.Unmarshal(env.Raw, &report))
				reported <- report
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := c.NewWorker(WorkerConfig{WorkerInstanceID: "w-cancel", HeartbeatInterval: 10 * time.Millisecond})
	worker.Register(ActionFunc("block", func(ctx Context, params map[string]any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))

	done := make(chan error, 1)
	go func() { done <- worker.runSocket(ctx) }()

	select {
	case report := <-reported:
		cancel()
		assert.NotNil(t, report.Status)
		assert.Equal(t, *report.Status, api.WorkerSocketJobReportFrameStatusFailed)
		assert.Equal(t, *report.ErrorType, "Cancelled")
	case <-time.After(time.Second):
		t.Fatal("worker did not report cancellation")
	}
	<-done
}

func newWorkerSocketTestClient(t *testing.T, fn func(t *testing.T, conn *websocket.Conn)) (*Client, *httptest.Server) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/workers/socket")
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer mbx_test")
		conn, err := upgrader.Upgrade(w, r, nil)
		assert.NoError(t, err)
		defer conn.Close()
		fn(t, conn)
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectHandle("test-project"))
	assert.NoError(t, err)
	return c, srv
}

func expectRegister(t *testing.T, conn *websocket.Conn, action string, models []api.WorkerSocketModelCapability) {
	t.Helper()
	env := readTestFrame(t, conn)
	assert.Equal(t, env.Type, "worker.register")
	var register api.WorkerSocketRegisterFrame
	assert.NoError(t, json.Unmarshal(env.Raw, &register))
	if action != "" {
		assert.NotNil(t, register.ActionNames)
		assert.Equal(t, (*register.ActionNames)[0], action)
	}
	if len(models) > 0 {
		assert.NotNil(t, register.Models)
		assert.Equal(t, (*register.Models)[0], models[0])
	}
	_ = conn.WriteJSON(api.WorkerSocketRegisteredFrame{
		Type: api.WorkerSocketRegisteredFrameTypeWorkerRegistered,
		Lease: api.WorkerSocketLeaseConfig{
			HeartbeatCadenceSeconds: 60,
			LeaseDurationSeconds:    180,
		},
		WorkerSessionToken: "session-1",
		MessageId:          register.MessageId,
	})
}

func expectClaim(t *testing.T, conn *websocket.Conn) api.WorkerSocketJobsClaimFrame {
	t.Helper()
	for {
		env := readTestFrame(t, conn)
		if env.Type != "jobs.claim" {
			continue
		}
		var claim api.WorkerSocketJobsClaimFrame
		assert.NoError(t, json.Unmarshal(env.Raw, &claim))
		return claim
	}
}

func sendClaimed(t *testing.T, conn *websocket.Conn, jobs ...api.WorkerSocketClaimedJob) {
	t.Helper()
	assert.NoError(t, conn.WriteJSON(api.WorkerSocketJobsClaimedFrame{
		Type: api.WorkerSocketJobsClaimedFrameTypeJobsClaimed,
		Jobs: jobs,
	}))
}

func readTestFrame(t *testing.T, conn *websocket.Conn) socketEnvelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := conn.ReadMessage()
	assert.NoError(t, err)
	var env socketEnvelope
	assert.NoError(t, json.Unmarshal(raw, &env))
	env.Raw = append(json.RawMessage(nil), raw...)
	return env
}
