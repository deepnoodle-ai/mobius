package mobius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestStartRun_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/runs")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, workflowRunJSON("run_1", "active"))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	spec := api.WorkflowSpec{Name: "demo", Steps: []api.WorkflowStep{}}
	run, err := c.StartRun(context.Background(), spec, &StartRunOptions{
		Queue:      "research",
		ExternalID: "external-1",
		Metadata:   map[string]string{"org_id": "org_1"},
		Inputs:     map[string]interface{}{"topic": "sdk"},
	})

	assert.NoError(t, err)
	assert.Equal(t, run.Id, "run_1")
	assert.Equal(t, body["mode"], "inline")
	assert.Equal(t, body["queue"], "research")
	assert.Equal(t, body["external_id"], "external-1")
}

func TestStartWorkflowRun_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/workflows/wf_1/runs")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, workflowRunJSON("run_1", "active"))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.StartWorkflowRun(context.Background(), "wf_1", &StartRunOptions{
		ExternalID: "external-1",
		Inputs:     map[string]interface{}{"topic": "sdk"},
	})

	assert.NoError(t, err)
	assert.Equal(t, run.Id, "run_1")
	assert.Equal(t, body["external_id"], "external-1")
	assert.Equal(t, body["mode"], nil)
	assert.Equal(t, body["definition_id"], nil)
}

func TestRunControl_HighLevelClient(t *testing.T) {
	seenQuery := ""
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1":
			_, _ = io.WriteString(w, workflowRunDetailJSON("run_1", "completed"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs":
			seenQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, fmt.Sprintf(`{"items":[%s],"has_more":false}`, workflowRunJSON("run_1", "completed")))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/runs/run_1/cancellations":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/runs/run_1/resumptions":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/runs/run_1/signals":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"id":"sig_1","run_id":"run_1","name":"approval"}`)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.GetRun(context.Background(), "run_1")
	assert.NoError(t, err)
	assert.Equal(t, run.Status, api.WorkflowRunStatusCompleted)

	list, err := c.ListRuns(context.Background(), &ListRunsOptions{
		Status:     api.WorkflowRunStatusCompleted,
		ExternalID: "external-1",
		Limit:      10,
	})
	assert.NoError(t, err)
	assert.Equal(t, len(list.Items), 1)
	assert.True(t, strings.Contains(seenQuery, "status=completed"))
	assert.True(t, strings.Contains(seenQuery, "external_id=external-1"))
	assert.True(t, strings.Contains(seenQuery, "limit=10"))

	assert.NoError(t, c.CancelRun(context.Background(), "run_1"))
	assert.NoError(t, c.ResumeRun(context.Background(), "run_1"))
	signal, err := c.SendRunSignal(context.Background(), "run_1", "approval", map[string]interface{}{"ok": true})
	assert.NoError(t, err)
	assert.Equal(t, signal.Id, "sig_1")
}

func TestWaitRun_FetchesAfterStreamClosesBeforeTerminal(t *testing.T) {
	var getCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1":
			w.Header().Set("Content-Type", "application/json")
			if getCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, workflowRunDetailJSON("run_1", "active"))
				return
			}
			_, _ = io.WriteString(w, workflowRunDetailJSON("run_1", "completed"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `event: run_updated
id: 7
data: {"type":"run_updated","run_id":"run_1","seq":7,"timestamp":"2026-04-27T00:00:00Z","data":{"status":"active"}}

`)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.WaitRun(context.Background(), "run_1", &WaitRunOptions{ReconnectDelay: time.Millisecond})
	assert.NoError(t, err)
	assert.Equal(t, run.Status, api.WorkflowRunStatusCompleted)
	assert.Equal(t, int(getCalls.Load()), 2)
}

func workflowRunJSON(id, status string) string {
	return fmt.Sprintf(`{
		"id":%q,
		"ephemeral":true,
		"workflow_name":"demo",
		"status":%q,
		"path_counts":{"total":1,"active":0,"working":0,"waiting":0,"completed":1,"failed":0},
		"job_counts":{"ready":0,"scheduled":0,"claimed":0},
		"wait_summary":{"waiting_paths":0,"kind_counts":{},"next_wake_at":null,"waiting_on_signal_names":[],"interaction_ids":[]},
		"errors":[],
		"attempt":1,
		"created_at":"2026-04-27T00:00:00Z",
		"updated_at":"2026-04-27T00:00:00Z"
	}`, id, status)
}

func workflowRunDetailJSON(id, status string) string {
	return strings.TrimSuffix(workflowRunJSON(id, status), "}") + `,"paths":[]}`
}
