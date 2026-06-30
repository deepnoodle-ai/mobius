package mobius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestInvokeAgent_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &body))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, turnAckJSON("sess_1", "turn_1", 7))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	ack, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentID:        "agent_1",
		Content:        []map[string]interface{}{{"type": "text", "text": "hi"}},
		IdempotencyKey: "evt_1",
		Session: &api.InvokeSessionSpec{
			SessionKey: ptr("app:acct_1:user_2"),
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, ack.AfterSequence, int64(7))
	assert.Equal(t, ack.Session.Id, "sess_1")
	assert.Equal(t, ack.Turn.Id, "turn_1")
	assert.Equal(t, body["agent_ref"].(map[string]any)["id"], "agent_1")
	assert.Equal(t, body["input"].(map[string]any)["idempotency_key"], "evt_1")
	assert.Equal(t, body["session"].(map[string]any)["session_key"], "app:acct_1:user_2")
}

func TestInvokeAgent_RequiresAgentRefAndContent(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		Content: []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.Error(t, err)

	_, err = c.InvokeAgent(context.Background(), InvokeAgentOptions{AgentID: "agent_1"})
	assert.Error(t, err)
}

func TestInvokeAgentStream_HighLevelClient(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			assert.Equal(t, r.Header.Get("Accept"), "text/event-stream")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: turn.completed\ndata: {\"usage\":{\"input_tokens\":42}}\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	events, err := c.InvokeAgentStream(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)

	ev, ok := <-events
	assert.True(t, ok)
	assert.Equal(t, ev.EventType, "turn.completed")
	payload, err := ev.Frame.AsTurnCompletedPayload()
	assert.NoError(t, err)
	assert.Equal(t, (*payload.Usage)["input_tokens"], float64(42))

	_, ok = <-events
	assert.False(t, ok)
}

func turnAckJSON(sessionID, turnID string, afterSequence int) string {
	return fmt.Sprintf(`{
		"after_sequence": %d,
		"session": {
			"id": %q,
			"agent_id": "agent_1",
			"origin": "api",
			"scope": "agent",
			"scope_name": "app:acct_1:user_2",
			"scope_ref_id": "agent_1",
			"session_key": "app:acct_1:user_2",
			"status": "active",
			"title": "",
			"visibility": "private",
			"version": 1,
			"message_count": 1,
			"token_input_total": 0,
			"token_output_total": 0,
			"created_at": "2026-05-27T00:00:00Z",
			"updated_at": "2026-05-27T00:00:00Z"
		},
		"turn": {
			"id": %q,
			"agent_id": "agent_1",
			"session_id": %q,
			"attempt": 1,
			"status": "running",
			"created_at": "2026-05-27T00:00:00Z",
			"updated_at": "2026-05-27T00:00:00Z"
		}
	}`, afterSequence, sessionID, turnID, sessionID)
}

func ptr[T any](v T) *T { return &v }
