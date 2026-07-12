package mobius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func mustFrame(t *testing.T, jsonStr string) api.SessionTranscriptFrame {
	t.Helper()
	var frame api.SessionTranscriptFrame
	assert.NoError(t, frame.UnmarshalJSON([]byte(jsonStr)))
	return frame
}

func blockMap(t *testing.T, block api.SessionContentBlock) map[string]interface{} {
	t.Helper()
	raw, err := block.MarshalJSON()
	assert.NoError(t, err)
	var m map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw, &m))
	return m
}

const upsertAssistantStreaming = `{"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`

func TestReducer_UpsertBlockDeltaConverge(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, upsertAssistantStreaming), "")
	r.Apply(mustFrame(t, `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":0,"block":{"type":"text","text":""}}`), "")
	r.Apply(mustFrame(t, `{"event_type":"message.delta","session_id":"s1","message_id":"m_a","content_index":0,"text":"hel"}`), "")
	r.Apply(mustFrame(t, `{"event_type":"message.delta","session_id":"s1","message_id":"m_a","content_index":0,"text":"lo"}`), "")

	assert.Equal(t, blockMap(t, r.Rows["m_a"].Content[0])["text"], "hello")

	// Completing block replaces whatever deltas built.
	r.Apply(mustFrame(t, `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":0,"block":{"type":"text","text":"hello world"}}`), "")
	assert.Equal(t, blockMap(t, r.Rows["m_a"].Content[0])["text"], "hello world")
}

func TestReducer_BlockPatchMergeAndNullClear(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[{"type":"tool_use","id":"toolu_1","name":"fetch","input":{},"status":"pending"}],"created_at":"2026-07-11T17:03:21Z"}`), "")

	r.Apply(mustFrame(t, `{"event_type":"message.block.patch","session_id":"s1","message_id":"m_a","content_index":0,"status":"running","progress":{"display":"scanned 1400 lines"}}`), "")
	block := blockMap(t, r.Rows["m_a"].Content[0])
	assert.Equal(t, block["status"], "running")
	assert.Equal(t, block["progress"].(map[string]interface{})["display"], "scanned 1400 lines")

	// progress:null clears; status still updates.
	r.Apply(mustFrame(t, `{"event_type":"message.block.patch","session_id":"s1","message_id":"m_a","content_index":0,"status":"ok","progress":null}`), "")
	block = blockMap(t, r.Rows["m_a"].Content[0])
	assert.Equal(t, block["status"], "ok")
	_, hasProgress := block["progress"]
	assert.False(t, hasProgress)
}

func TestReducer_TerminalTurnPrunesStreamingRows(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, upsertAssistantStreaming), "")
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m_final","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`), "")
	r.Apply(mustFrame(t, `{"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"cancelled","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:30Z"}`), "")

	_, hasLive := r.Rows["m_a"]
	assert.False(t, hasLive) // pruned
	_, hasFinal := r.Rows["m_final"]
	assert.True(t, hasFinal) // durable row survives
}

func TestReducer_CursorAndReady(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, `{"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:20Z"}`), "42.7")
	assert.Equal(t, r.Cursor, "42.7")
	assert.False(t, r.Ready)

	r.Apply(mustFrame(t, `{"event_type":"stream.ready","session_id":"s1","resume_cursor":"99.9"}`), "")
	assert.Equal(t, r.Cursor, "99.9")
	assert.True(t, r.Ready)
}

func TestReducer_OrderingAndUnknownFrame(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, `{"event_type":"future.frame","whatever":true}`), "") // ignored
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m2","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`), "")
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m1","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`), "")
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m3","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":2,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`), "")

	var ids []string
	for _, m := range r.Messages() {
		ids = append(ids, m.Id)
	}
	assert.Equal(t, ids, []string{"m1", "m2", "m3"})
}

func TestReducer_ApplySnapshotPrunesStreaming(t *testing.T) {
	r := NewSessionTranscriptReducer()
	r.Apply(mustFrame(t, `{"event_type":"message.upsert","id":"m_stale","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t0","turn_index":9,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`), "")

	var snap api.SessionTranscriptSnapshot
	assert.NoError(t, json.Unmarshal([]byte(`{"messages":[{"id":"m1","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}],"turns":[],"has_more":false,"resume_cursor":"42.6"}`), &snap))
	r.ApplySnapshot(&snap)

	_, hasStale := r.Rows["m_stale"]
	assert.False(t, hasStale) // dropped: absent from final page
	_, hasM1 := r.Rows["m1"]
	assert.True(t, hasM1)
	assert.Equal(t, r.Cursor, "42.6")
}

func TestGetSessionTranscript_BuildsQuery(t *testing.T) {
	var gotPath, gotCursor, gotLimit string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCursor = r.URL.Query().Get("cursor")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[],"turns":[],"has_more":false,"resume_cursor":"1.1"}`)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	snap, err := c.GetSessionTranscript(context.Background(), "sess_1", &GetSessionTranscriptOptions{Cursor: "10.2", Limit: 50})
	assert.NoError(t, err)
	assert.Equal(t, snap.ResumeCursor, "1.1")
	assert.Equal(t, gotPath, "/v1/projects/test-project/sessions/sess_1/transcript")
	assert.Equal(t, gotCursor, "10.2")
	assert.Equal(t, gotLimit, "50")
}

func TestStreamSessionTranscript_DecodesFramesWithID(t *testing.T) {
	var accept string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 42.7\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"running\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:20Z\"}\n\n")
		_, _ = io.WriteString(w, "event: stream.ready\ndata: {\"event_type\":\"stream.ready\",\"session_id\":\"s1\",\"resume_cursor\":\"42.7\"}\n\n")
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	ch, err := c.StreamSessionTranscript(context.Background(), "sess_1", nil)
	assert.NoError(t, err)

	var got []TranscriptStreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	assert.Equal(t, accept, "text/event-stream")
	assert.Equal(t, len(got), 2)
	assert.Equal(t, got[0].ID, "42.7")
	assert.Equal(t, got[0].EventType, "turn.upsert")
	// The ready frame has no id: line; last-event-id persists as the cursor.
	assert.Equal(t, got[1].ID, "42.7")
}

func TestWatchSessionTranscript_ReconnectsOnRotateStopsOnIdle(t *testing.T) {
	var calls int32
	var cursors []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, "id: 42.7\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"running\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:20Z\"}\n\n")
			_, _ = io.WriteString(w, "event: stream.end\ndata: {\"event_type\":\"stream.end\",\"session_id\":\"s1\",\"reason\":\"rotate\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "id: 43.9\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"completed\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:40Z\"}\n\n")
		_, _ = io.WriteString(w, "event: stream.end\ndata: {\"event_type\":\"stream.end\",\"session_id\":\"s1\",\"reason\":\"idle\"}\n\n")
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	r := NewSessionTranscriptReducer()
	for ev := range c.WatchSessionTranscript(context.Background(), "sess_1", nil) {
		r.Apply(ev.Frame, ev.ID)
	}

	assert.Equal(t, atomic.LoadInt32(&calls), int32(2)) // reconnected once on rotate, stopped on idle
	assert.Equal(t, cursors[0], "")                     // first connect: no cursor
	assert.Equal(t, cursors[1], "42.7")                 // reconnect carries the advanced cursor
	assert.Equal(t, r.Turns["t1"].Status, "completed")
}

func TestWatchSessionTranscript_SurfacesPermanentError(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	var streamErr error
	var frames int
	for ev := range c.WatchSessionTranscript(context.Background(), "sess_1", nil) {
		if ev.Err != nil {
			streamErr = ev.Err
			continue
		}
		frames++
	}

	assert.Error(t, streamErr)                          // permanent status surfaced to the caller
	assert.ErrorContains(t, streamErr, "404")           // carries the status
	assert.Equal(t, frames, 0)                          // no frames on a failed open
	assert.Equal(t, atomic.LoadInt32(&calls), int32(1)) // no reconnect on a permanent status
}

func TestInvokeAgentTranscript_StreamsTurnToTerminal(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckWithCursor("s1", "t1", 42))
		case r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript/stream":
			assert.Equal(t, r.URL.Query().Get("cursor"), "41.6")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message.upsert\ndata: {\"event_type\":\"message.upsert\",\"id\":\"m_a\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"role\":\"assistant\",\"status\":\"final\",\"turn_id\":\"t1\",\"turn_index\":1,\"sequence\":43,\"entry_type\":\"message\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}],\"created_at\":\"2026-07-11T17:03:21Z\"}\n\n")
			_, _ = io.WriteString(w, "id: 43.9\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"completed\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:40Z\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	ack, ch, err := c.InvokeAgentTranscript(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)
	assert.Equal(t, ack.Turn.Id, "t1")
	assert.NotNil(t, ack.UserMessage)

	r := NewSessionTranscriptReducer()
	// Seed from the ack as documented, then apply streamed frames.
	r.Rows[ack.UserMessage.Id] = ack.UserMessage
	for ev := range ch {
		r.Apply(ev.Frame, ev.ID)
	}

	assert.Equal(t, r.Turns["t1"].Status, "completed")
	var ids []string
	for _, m := range r.MessagesForTurn("t1") {
		ids = append(ids, m.Id)
	}
	assert.Equal(t, ids, []string{"m_user", "m_a"})
}

func TestIsTerminalTurnStatus(t *testing.T) {
	assert.True(t, IsTerminalTurnStatus("completed"))
	assert.True(t, IsTerminalTurnStatus("failed"))
	assert.True(t, IsTerminalTurnStatus("cancelled"))
	assert.False(t, IsTerminalTurnStatus("running"))
	assert.False(t, IsTerminalTurnStatus("queued"))
}

func invokeAckWithCursor(sessionID, turnID string, afterSequence int) string {
	return fmt.Sprintf(`{
		"after_sequence": %d,
		"resume_cursor": "41.6",
		"user_message": {
			"id": "m_user", "session_id": %q, "agent_id": "a1", "role": "user",
			"status": "final", "turn_id": %q, "turn_index": 0, "sequence": 42,
			"entry_type": "message", "content": [{"type":"text","text":"hi"}],
			"created_at": "2026-07-11T17:03:21Z"
		},
		"session": {
			"id": %q, "agent_id": "a1", "origin": "api", "scope": "agent",
			"scope_name": "app", "scope_ref_id": "a1", "session_key": "app",
			"status": "active", "title": "", "visibility": "private", "version": 1,
			"message_count": 1, "token_input_total": 0, "token_output_total": 0,
			"created_at": "2026-05-27T00:00:00Z", "updated_at": "2026-05-27T00:00:00Z"
		},
		"turn": {
			"id": %q, "agent_id": "a1", "session_id": %q, "attempt": 1,
			"status": "queued", "created_at": "2026-05-27T00:00:00Z",
			"updated_at": "2026-05-27T00:00:00Z"
		}
	}`, afterSequence, sessionID, turnID, sessionID, turnID, sessionID)
}
