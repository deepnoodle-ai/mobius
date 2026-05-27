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

func TestStartAutomationRun_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/automations/research/runs")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, automationRunJSON("run_1", "running"))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.StartAutomationRun(context.Background(), "research", &StartRunOptions{
		ExternalID: "external-1",
		Inputs:     map[string]interface{}{"topic": "sdk"},
	})

	assert.NoError(t, err)
	assert.Equal(t, run.Id, "run_1")
	assert.Equal(t, body["external_id"], "external-1")
	assert.Equal(t, body["inputs"].(map[string]any)["topic"], "sdk")
}

func TestRunControl_HighLevelClient(t *testing.T) {
	seenQuery := ""
	var cancelBody map[string]any
	var signalBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1":
			_, _ = io.WriteString(w, automationRunJSON("run_1", "completed"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs":
			seenQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, fmt.Sprintf(`{"items":[%s],"has_more":false}`, automationRunJSON("run_1", "completed")))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/runs/run_1/cancellations":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &cancelBody))
			_, _ = io.WriteString(w, automationRunJSON("run_1", "cancelled"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/runs/run_1/signals":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &signalBody))
			_, _ = io.WriteString(w, automationRunJSON("run_1", "running"))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.GetRun(context.Background(), "run_1")
	assert.NoError(t, err)
	assert.Equal(t, run.Status, api.AutomationRunStatusCompleted)

	list, err := c.ListRuns(context.Background(), &ListRunsOptions{
		Status:       api.AutomationRunStatusCompleted,
		AutomationID: "aut_1",
		Limit:        10,
	})
	assert.NoError(t, err)
	assert.Equal(t, len(list.Items), 1)
	assert.True(t, strings.Contains(seenQuery, "status=completed"))
	assert.True(t, strings.Contains(seenQuery, "automation_id=aut_1"))
	assert.True(t, strings.Contains(seenQuery, "limit=10"))

	cancelled, err := c.CancelRun(context.Background(), "run_1", "user requested")
	assert.NoError(t, err)
	assert.Equal(t, cancelled.Status, api.AutomationRunStatusCancelled)
	assert.Equal(t, cancelBody["reason"], "user requested")

	signalled, err := c.SignalRun(context.Background(), "run_1", "approval", map[string]interface{}{"ok": true})
	assert.NoError(t, err)
	assert.Equal(t, signalled.Id, "run_1")
	assert.Equal(t, signalBody["step_key"], "approval")
	assert.Equal(t, signalBody["result"].(map[string]any)["ok"], true)
}

func TestWaitRun_FetchesAfterStreamClosesBeforeTerminal(t *testing.T) {
	var getCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1":
			w.Header().Set("Content-Type", "application/json")
			if getCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, automationRunJSON("run_1", "running"))
				return
			}
			_, _ = io.WriteString(w, automationRunJSON("run_1", "completed"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/runs/run_1/events.stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `event: run.updated
id: 7
data: {"id":"evt_1","org_id":"org_1","project_id":"proj_1","run_id":"run_1","event_type":"run.updated","sequence":7,"payload":{"status":"running"},"created_at":"2026-05-27T00:00:00Z"}

`)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	run, err := c.WaitRun(context.Background(), "run_1", &WaitRunOptions{ReconnectDelay: time.Millisecond})
	assert.NoError(t, err)
	assert.Equal(t, run.Status, api.AutomationRunStatusCompleted)
	assert.Equal(t, int(getCalls.Load()), 2)
}

func automationRunJSON(id, status string) string {
	return fmt.Sprintf(`{
		"id":%q,
		"org_id":"org_1",
		"project_id":"proj_1",
		"automation_id":"aut_1",
		"automation_version_id":"autv_1",
		"automation_version":1,
		"status":%q,
		"created_at":"2026-05-27T00:00:00Z",
		"updated_at":"2026-05-27T00:00:00Z"
	}`, id, status)
}
