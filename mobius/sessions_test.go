package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestListSessionsFiltersByAgentNameAndSessionKey(t *testing.T) {
	var query string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"has_more":false}`))
	}))
	defer srv.Close()

	_, err := c.ListSessions(context.Background(), &ListSessionsOptions{
		AgentName:  "Scout",
		SessionKey: "conversation-1",
	})
	assert.NoError(t, err)
	assert.Contains(t, query, "agent_name=Scout")
	assert.Contains(t, query, "session_key=conversation-1")
}

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

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentID:        "agent_1",
		Content:        []map[string]interface{}{{"type": "text", "text": "hi"}},
		Context:        []RuntimeContextItem{{Name: "naming-board", Content: "Chosen: none"}},
		IdempotencyKey: "evt_1",
		Session: &api.InvokeSessionSpec{
			SessionKey: ptr("app:acct_1:user_2"),
		},
		Config: &api.InlineAgentConfig{
			Instructions: ptr("Be concise."),
			Model:        ptr("claude-sonnet-4-6"),
			Effort:       ptr(api.ThinkingEffortMedium),
			Toolkits: &[]api.InlineToolkit{
				{Name: "tickets", Actions: &[]string{"tickets.search"}},
			},
		},
		Operation: &api.AgentTurnOperationPolicy{TimeoutSeconds: ptr(int64(90))},
	})

	assert.NoError(t, err)
	assert.Equal(t, turn.AfterSequence(), int64(7))
	assert.Equal(t, turn.SessionID(), "sess_1")
	assert.Equal(t, turn.ID(), "turn_1")
	assert.Equal(t, turn.Status(), "running")
	assert.False(t, turn.Deduped())
	assert.Equal(t, body["agent_ref"].(map[string]any)["id"], "agent_1")
	input := body["input"].(map[string]any)
	assert.Equal(t, input["idempotency_key"], "evt_1")
	contextItem := input["context"].([]any)[0].(map[string]any)
	assert.Equal(t, contextItem["name"], "naming-board")
	assert.Equal(t, contextItem["content"], "Chosen: none")
	assert.Equal(t, body["session"].(map[string]any)["session_key"], "app:acct_1:user_2")
	config := body["config"].(map[string]any)
	assert.Equal(t, config["instructions"], "Be concise.")
	assert.Equal(t, config["model"], "claude-sonnet-4-6")
	assert.Equal(t, config["effort"], "medium")
	assert.Equal(t, config["toolkits"].([]any)[0].(map[string]any)["name"], "tickets")
	assert.Equal(t, body["operation"].(map[string]any)["timeout_seconds"], float64(90))

	turn.Transcript().Apply(TranscriptStreamEvent{Frame: mustFrame(t,
		`{"event_type":"turn.upsert","id":"turn_1","session_id":"sess_1","agent_id":"agent_1","attempt":1,"status":"failed","error_type":"invalid_conversation_state","error_message":"history ended with assistant content","created_at":"2026-05-27T00:00:00Z","updated_at":"2026-05-27T00:00:01Z"}`)})
	assert.Equal(t, turn.ErrorType(), "invalid_conversation_state")
	assert.Equal(t, turn.ErrorMessage(), "history ended with assistant content")
	assert.Error(t, turn.TurnError())
}

func TestStartTurn_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/sessions/sess_1/turns")
		raw, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, turnAckJSON("sess_1", "turn_1", 7))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.StartTurn(context.Background(), "sess_1", StartTurnOptions{
		Content:        []map[string]interface{}{{"type": "text", "text": "hi"}},
		Context:        []RuntimeContextItem{{Name: "naming-board", Content: "Chosen: none"}},
		IdempotencyKey: "evt_1",
		Operation:      &api.AgentTurnOperationPolicy{TimeoutSeconds: ptr(int64(45))},
		Metadata:       map[string]interface{}{"source": "app"},
	})

	assert.NoError(t, err)
	assert.Equal(t, turn.ID(), "turn_1")
	assert.Equal(t, body["idempotency_key"], "evt_1")
	assert.Equal(t, body["operation"].(map[string]any)["timeout_seconds"], float64(45))
	assert.Equal(t, body["metadata"].(map[string]any)["source"], "app")
	contextItem := body["context"].([]any)[0].(map[string]any)
	assert.Equal(t, contextItem["name"], "naming-board")
	assert.Equal(t, contextItem["content"], "Chosen: none")
}

func TestListSessionMessages_IncludesContext(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/sessions/sess_1/messages")
		assert.Equal(t, r.URL.Query().Get("include"), "context")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	messages, err := c.ListSessionMessages(context.Background(), "sess_1", &ListSessionMessagesOptions{Include: "context"})
	assert.NoError(t, err)
	assert.Equal(t, len(messages.Items), 0)
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

func TestInvokeAgent_StructuredAPIError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req_1")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"session_turn_active","message":"another direct turn is active","details":{"turn_id":"turn_blocking","status":"running"}}}`)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	_, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "next"}},
	})
	var apiErr *APIError
	assert.True(t, errors.As(err, &apiErr))
	assert.Equal(t, apiErr.Status, http.StatusConflict)
	assert.Equal(t, apiErr.Code, "session_turn_active")
	assert.Equal(t, apiErr.Details["turn_id"], "turn_blocking")
	assert.Equal(t, apiErr.RequestID, "req_1")
	assert.Equal(t, apiErr.RetryAfter.String(), "2s")
}

func TestNudgeSession_HighLevelClient(t *testing.T) {
	var body map[string]interface{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/sessions/s1/nudges")
		raw, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"nudge_id":"nudge_1","delivery":"current_turn","session":{"id":"s1","agent_id":"a1","origin":"api","scope":"agent","scope_name":"app","scope_ref_id":"a1","session_key":"app","status":"active","title":"","visibility":"private","version":1,"message_count":1,"token_input_total":0,"cache_read_input_total":0,"cache_creation_input_total":0,"token_output_total":0,"created_at":"2026-05-27T00:00:00Z","updated_at":"2026-05-27T00:00:00Z"},"turn":{"id":"t1","agent_id":"a1","session_id":"s1","attempt":1,"status":"running","created_at":"2026-05-27T00:00:00Z","updated_at":"2026-05-27T00:00:00Z"},"after_sequence":2,"deduped":false,"woke_turn":true}`)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	ack, err := c.NudgeSession(context.Background(), "s1", NudgeSessionOptions{
		Content:        "Use the shorter name",
		IdempotencyKey: "event_2",
		Wake:           true,
	})
	assert.NoError(t, err)
	assert.Equal(t, ack.NudgeId, "nudge_1")
	assert.Equal(t, body["content"], "Use the shorter name")
	assert.Equal(t, body["idempotency_key"], "event_2")
	assert.Equal(t, body["wake"], true)
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
		"resume_cursor": "41.6",
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
