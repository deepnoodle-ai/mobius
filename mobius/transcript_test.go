package mobius

import (
	"context"
	"encoding/json"
	"errors"
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

// applyJSON folds one frame given as JSON, with an optional SSE id.
func applyJSON(t *testing.T, view *SessionTranscript, sseID, jsonStr string) {
	t.Helper()
	view.Apply(TranscriptStreamEvent{ID: sseID, Frame: mustFrame(t, jsonStr)})
}

func blockMap(t *testing.T, block api.SessionContentBlock) map[string]interface{} {
	t.Helper()
	raw, err := block.MarshalJSON()
	assert.NoError(t, err)
	var m map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func mustMessage(t *testing.T, view *SessionTranscript, id string) *api.SessionTranscriptMessage {
	t.Helper()
	msg, ok := view.Message(id)
	assert.True(t, ok)
	return msg
}

const upsertAssistantStreaming = `{"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`

func TestSessionTranscript_UpsertBlockDeltaConverge(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", upsertAssistantStreaming)
	applyJSON(t, view, "", `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":0,"block":{"type":"text","text":""}}`)
	applyJSON(t, view, "", `{"event_type":"message.delta","session_id":"s1","message_id":"m_a","content_index":0,"text":"hel"}`)
	applyJSON(t, view, "", `{"event_type":"message.delta","session_id":"s1","message_id":"m_a","content_index":0,"text":"lo"}`)

	assert.Equal(t, blockMap(t, mustMessage(t, view, "m_a").Content[0])["text"], "hello")

	// Completing block replaces whatever deltas built.
	applyJSON(t, view, "", `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":0,"block":{"type":"text","text":"hello world"}}`)
	assert.Equal(t, blockMap(t, mustMessage(t, view, "m_a").Content[0])["text"], "hello world")
}

func TestSessionTranscript_BlockAtGapIndexPadsContent(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", upsertAssistantStreaming)
	// Opening index 2 on empty content pads indexes 0 and 1 with empty blocks.
	applyJSON(t, view, "", `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":2,"block":{"type":"text","text":"third"}}`)

	content := mustMessage(t, view, "m_a").Content
	assert.Equal(t, len(content), 3)
	assert.Equal(t, blockMap(t, content[2])["text"], "third")

	// Deltas target the padded blocks, not a hole.
	applyJSON(t, view, "", `{"event_type":"message.delta","session_id":"s1","message_id":"m_a","content_index":0,"text":"pad"}`)
	assert.Equal(t, blockMap(t, mustMessage(t, view, "m_a").Content[0])["text"], "pad")

	// A late message.block fills a padded index in place.
	applyJSON(t, view, "", `{"event_type":"message.block","session_id":"s1","message_id":"m_a","content_index":1,"block":{"type":"text","text":"second"}}`)
	assert.Equal(t, blockMap(t, mustMessage(t, view, "m_a").Content[1])["text"], "second")
}

func TestSessionTranscript_BlockPatchMergeAndNullClear(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[{"type":"tool_use","id":"toolu_1","name":"fetch","input":{},"status":"pending"}],"created_at":"2026-07-11T17:03:21Z"}`)

	applyJSON(t, view, "", `{"event_type":"message.block.patch","session_id":"s1","message_id":"m_a","content_index":0,"status":"running","progress":{"display":"scanned 1400 lines"}}`)
	block := blockMap(t, mustMessage(t, view, "m_a").Content[0])
	assert.Equal(t, block["status"], "running")
	assert.Equal(t, block["progress"].(map[string]interface{})["display"], "scanned 1400 lines")

	// progress:null clears; status still updates.
	applyJSON(t, view, "", `{"event_type":"message.block.patch","session_id":"s1","message_id":"m_a","content_index":0,"status":"ok","progress":null}`)
	block = blockMap(t, mustMessage(t, view, "m_a").Content[0])
	assert.Equal(t, block["status"], "ok")
	_, hasProgress := block["progress"]
	assert.False(t, hasProgress)
}

func TestSessionTranscript_NormalizesNullContentAtEveryIngress(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m_apply","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":null,"created_at":"2026-07-11T17:03:21Z"}`)
	assert.NotNil(t, mustMessage(t, view, "m_apply").Content)
	assert.Equal(t, len(mustMessage(t, view, "m_apply").Content), 0)

	var snap api.SessionTranscriptSnapshot
	assert.NoError(t, json.Unmarshal([]byte(`{"messages":[{"id":"m_snapshot","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":2,"sequence":null,"entry_type":"message","content":null,"created_at":"2026-07-11T17:03:21Z"}],"turns":[],"has_more":true,"resume_cursor":"1.1"}`), &snap))
	view.ApplySnapshot(&snap)
	assert.NotNil(t, mustMessage(t, view, "m_snapshot").Content)

	var ack api.TurnAck
	assert.NoError(t, json.Unmarshal([]byte(invokeAckWithCursor("s1", "t1", 42)), &ack))
	ack.UserMessage.Content = nil
	view.Seed(&ack)
	assert.NotNil(t, mustMessage(t, view, "m_user").Content)
}

func TestSessionTranscript_RenderableMessagesAndContentHelpers(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:20Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"empty_1","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"empty_2","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":2,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"preview","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":3,"sequence":null,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"catalog","input":{"command":"check","args":{"domain":"x.test"}}}],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"final","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":3,"sequence":4,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"catalog","input":{"command":"check","args":{"domain":"x.test"}},"resolved_action":{"name":"naming.domain.check","input":{"domain":"x.test"}}},{"type":"text","text":"done"}],"created_at":"2026-07-11T17:03:21Z"}`)

	visible := view.RenderableMessages()
	assert.Equal(t, len(visible), 2)
	assert.Equal(t, visible[0].Id, "final")
	assert.Equal(t, visible[1].Id, "empty_2")
	tool, err := visible[0].Content[0].AsSessionToolUseBlock()
	assert.NoError(t, err)
	normalized := NormalizeToolUse(tool)
	assert.Equal(t, normalized.WireName, "catalog")
	assert.Equal(t, normalized.ResolvedAction.Name, "naming.domain.check")
	assert.Equal(t, normalized.WireInput["command"], "check")
	assert.Equal(t, TextOf(visible[0]), "done")

	var stringResult api.SessionToolResultBlock
	assert.NoError(t, json.Unmarshal([]byte(`{"type":"tool_result","tool_use_id":"call_1","content":"plain"}`), &stringResult))
	assert.Equal(t, ToolResultText(stringResult), "plain")
	var blockResult api.SessionToolResultBlock
	assert.NoError(t, json.Unmarshal([]byte(`{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"blocks"}]}`), &blockResult))
	assert.Equal(t, ToolResultText(blockResult), "blocks")
}

func TestSessionTranscript_ResolvedActionPatchEnrichesLiveBlock(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m_tool","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":1,"sequence":null,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"wire","input":{}}],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.block.patch","session_id":"s1","message_id":"m_tool","content_index":0,"resolved_action":{"name":"naming.domain.check","input":{"domain":"x.test"}}}`)
	block := blockMap(t, mustMessage(t, view, "m_tool").Content[0])
	resolved := block["resolved_action"].(map[string]interface{})
	assert.Equal(t, resolved["name"], "naming.domain.check")
}

func TestSessionTranscript_TerminalTurnPrunesStreamingRows(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", upsertAssistantStreaming)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m_final","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"cancelled","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:30Z"}`)

	_, hasLive := view.Message("m_a")
	assert.False(t, hasLive) // pruned
	_, hasFinal := view.Message("m_final")
	assert.True(t, hasFinal) // durable row survives
}

func TestSessionTranscript_CursorAndReady(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "42.7", `{"event_type":"turn.upsert","id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"running","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:20Z"}`)
	assert.Equal(t, view.Cursor(), "42.7")
	assert.False(t, view.Ready())

	applyJSON(t, view, "", `{"event_type":"stream.ready","session_id":"s1","resume_cursor":"99.9"}`)
	assert.Equal(t, view.Cursor(), "99.9")
	assert.True(t, view.Ready())
}

func TestSessionTranscript_OrderingAndUnknownFrame(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"future.frame","whatever":true}`) // ignored
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m2","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m1","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m3","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t1","turn_index":2,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)

	var ids []string
	for _, m := range view.Messages() {
		ids = append(ids, m.Id)
	}
	assert.Equal(t, ids, []string{"m1", "m2", "m3"})
}

func TestSessionTranscript_ApplySnapshotPrunesStreaming(t *testing.T) {
	view := NewSessionTranscript()
	applyJSON(t, view, "", `{"event_type":"message.upsert","id":"m_stale","session_id":"s1","agent_id":"a1","role":"assistant","status":"streaming","turn_id":"t0","turn_index":9,"sequence":null,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}`)

	var snap api.SessionTranscriptSnapshot
	assert.NoError(t, json.Unmarshal([]byte(`{"messages":[{"id":"m1","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}],"turns":[],"has_more":false,"resume_cursor":"42.6"}`), &snap))
	view.ApplySnapshot(&snap)

	_, hasStale := view.Message("m_stale")
	assert.False(t, hasStale) // dropped: absent from final page
	_, hasM1 := view.Message("m1")
	assert.True(t, hasM1)
	assert.Equal(t, view.Cursor(), "42.6")
}

func TestSessionTranscript_SeedFoldsAckState(t *testing.T) {
	var ack api.TurnAck
	assert.NoError(t, json.Unmarshal([]byte(invokeAckWithCursor("s1", "t1", 42)), &ack))

	view := NewSessionTranscript()
	view.Seed(&ack)

	msg, ok := view.Message("m_user")
	assert.True(t, ok)
	assert.Equal(t, string(msg.Role), "user")
	turn, ok := view.Turn("t1")
	assert.True(t, ok)
	assert.Equal(t, turn.Status, "queued")
	assert.Equal(t, view.Cursor(), "41.6")
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

	watch := c.WatchSessionTranscript(context.Background(), "sess_1", nil)
	steps := 0
	for watch.Next() {
		steps++
	}
	assert.NoError(t, watch.Err())

	assert.Equal(t, steps, 2)                           // one turn.upsert per connection
	assert.Equal(t, atomic.LoadInt32(&calls), int32(2)) // reconnected once on rotate, stopped on idle
	assert.Equal(t, cursors[0], "")                     // first connect: no cursor
	assert.Equal(t, cursors[1], "42.7")                 // reconnect carries the advanced cursor
	turn, ok := watch.Turn("t1")
	assert.True(t, ok)
	assert.Equal(t, turn.Status, "completed")
	assert.Equal(t, watch.Cursor(), "43.9")
}

func TestWatchSessionTranscript_SurfacesPermanentError(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	watch := c.WatchSessionTranscript(context.Background(), "sess_1", nil)
	steps := 0
	for watch.Next() {
		steps++
	}

	assert.Error(t, watch.Err())                // permanent status surfaced to the caller
	assert.ErrorContains(t, watch.Err(), "404") // carries the status
	var apiErr *APIError
	assert.True(t, errors.As(watch.Err(), &apiErr))
	assert.Equal(t, apiErr.Code, "not_found")
	assert.Equal(t, steps, 0)                           // no state changes on a failed open
	assert.Equal(t, atomic.LoadInt32(&calls), int32(1)) // no reconnect on a permanent status
}

func TestWatchSessionTranscript_ContinuesProvidedTranscript(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Query().Get("cursor"), "42.6") // resumes from the view's cursor
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: stream.end\ndata: {\"event_type\":\"stream.end\",\"session_id\":\"s1\",\"reason\":\"idle\"}\n\n")
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	view := NewSessionTranscript()
	var snap api.SessionTranscriptSnapshot
	assert.NoError(t, json.Unmarshal([]byte(`{"messages":[],"turns":[],"has_more":false,"resume_cursor":"42.6"}`), &snap))
	view.ApplySnapshot(&snap)

	watch := c.WatchSessionTranscript(context.Background(), "sess_1", &WatchSessionTranscriptOptions{Transcript: view})
	for watch.Next() {
	}
	assert.NoError(t, watch.Err())
}

func TestInvokeAgent_StreamsTurnToTerminal(t *testing.T) {
	var streamCalls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckWithCursor("s1", "t1", 42))
		case r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript/stream":
			atomic.AddInt32(&streamCalls, 1)
			assert.Equal(t, r.URL.Query().Get("cursor"), "41.6") // opened from the seeded cursor
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message.upsert\ndata: {\"event_type\":\"message.upsert\",\"id\":\"m_a\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"role\":\"assistant\",\"status\":\"final\",\"turn_id\":\"t1\",\"turn_index\":1,\"sequence\":43,\"entry_type\":\"message\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}],\"created_at\":\"2026-07-11T17:03:21Z\"}\n\n")
			_, _ = io.WriteString(w, "id: 43.9\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"completed\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:40Z\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript":
			assert.Equal(t, r.URL.Query().Get("cursor"), "41.6")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"messages":[],"turns":[],"has_more":false,"resume_cursor":"43.9"}`)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)
	assert.Equal(t, turn.ID(), "t1")
	assert.Equal(t, turn.SessionID(), "s1")
	assert.Equal(t, turn.Status(), "queued") // seeded from the invoke response
	assert.Equal(t, atomic.LoadInt32(&streamCalls), int32(0))

	// The user message is seeded before any streaming.
	var ids []string
	for _, m := range turn.Messages() {
		ids = append(ids, m.Id)
	}
	assert.Equal(t, ids, []string{"m_user"})

	steps := 0
	for turn.Next() {
		steps++
	}
	assert.NoError(t, turn.Err())

	assert.Equal(t, steps, 2) // message.upsert + terminal turn.upsert
	assert.Equal(t, turn.Status(), "completed")
	ids = nil
	for _, m := range turn.Messages() {
		ids = append(ids, m.Id)
	}
	assert.Equal(t, ids, []string{"m_user", "m_a"})
	assert.Equal(t, turn.Transcript().Cursor(), "43.9")
}

func TestInvokeAgent_RedrainsFromInvocationCursorBeforeFinalUpdate(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckWithCursor("s1", "t1", 42))
		case r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message.upsert\ndata: {\"event_type\":\"message.upsert\",\"id\":\"m_preview\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"role\":\"assistant\",\"status\":\"streaming\",\"turn_id\":\"t1\",\"turn_index\":1,\"sequence\":null,\"entry_type\":\"message\",\"content\":[{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"naming_words_coin\",\"input\":{\"count\":2}}],\"created_at\":\"2026-07-11T17:03:21Z\"}\n\n")
			_, _ = io.WriteString(w, "id: 44.3\nevent: message.upsert\ndata: {\"event_type\":\"message.upsert\",\"id\":\"m_result\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"role\":\"user\",\"status\":\"final\",\"turn_id\":\"t1\",\"turn_index\":2,\"sequence\":44,\"entry_type\":\"message\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"call_1\",\"content\":\"ok\"}],\"created_at\":\"2026-07-11T17:03:21Z\"}\n\n")
			_, _ = io.WriteString(w, "id: 44.9\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"completed\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:40Z\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript":
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Query().Get("cursor") == "44.9":
				// Real lower-bound filtering cannot redeliver the call at sequence 43.
				_, _ = io.WriteString(w, `{"messages":[{"id":"m_done","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":3,"sequence":45,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"2026-07-11T17:03:21Z"}],"turns":[{"id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:40Z"}],"has_more":false,"resume_cursor":"45.9"}`)
			case r.URL.Query().Get("page_token") == "terminal-page-2":
				_, _ = io.WriteString(w, `{"messages":[{"id":"m_result","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":2,"sequence":44,"entry_type":"message","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}],"created_at":"2026-07-11T17:03:21Z"},{"id":"m_done","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":3,"sequence":45,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"2026-07-11T17:03:21Z"}],"turns":[{"id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:40Z"}],"has_more":false,"resume_cursor":"45.9"}`)
			default:
				assert.Equal(t, r.URL.Query().Get("cursor"), "41.6")
				_, _ = io.WriteString(w, `{"messages":[{"id":"m_user","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[{"type":"text","text":"hi"}],"created_at":"2026-07-11T17:03:21Z"},{"id":"m_call","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"naming_words_coin","input":{"count":2},"resolved_action":{"name":"naming.words.coin","input":{"count":2}}}],"created_at":"2026-07-11T17:03:21Z"}],"turns":[],"has_more":true,"next_page_token":"terminal-page-2","resume_cursor":"43.3"}`)
			}
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)

	steps := 0
	for turn.Next() {
		steps++
		if turn.Status() == "completed" {
			assert.Equal(t, turn.Update().Cursor, "45.9")
			assert.Equal(t, turn.Update().Connection, TranscriptConnectionEnded)
		}
	}
	assert.NoError(t, turn.Err())
	assert.Equal(t, steps, 3)

	var durable api.SessionTranscriptSnapshot
	assert.NoError(t, json.Unmarshal([]byte(`{"messages":[{"id":"m_user","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[{"type":"text","text":"hi"}],"created_at":"2026-07-11T17:03:21Z"},{"id":"m_call","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[{"type":"tool_use","id":"call_1","name":"naming_words_coin","input":{"count":2},"resolved_action":{"name":"naming.words.coin","input":{"count":2}}}],"created_at":"2026-07-11T17:03:21Z"},{"id":"m_result","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":2,"sequence":44,"entry_type":"message","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}],"created_at":"2026-07-11T17:03:21Z"},{"id":"m_done","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":3,"sequence":45,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"2026-07-11T17:03:21Z"}],"turns":[{"id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:40Z"}],"has_more":false,"resume_cursor":"45.9"}`), &durable))
	full := NewSessionTranscript()
	full.ApplySnapshot(&durable)
	assert.Equal(t, turn.RenderableMessages(), full.RenderableMessagesForTurn("t1"))
	resolved := blockMap(t, mustMessage(t, turn.Transcript(), "m_call").Content[0])["resolved_action"].(map[string]interface{})
	assert.Equal(t, resolved["name"], "naming.words.coin")
}

func TestInvokeAgent_ReturnsTerminalReconciliationError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckWithCursor("s1", "t1", 42))
		case r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id: 43.9\nevent: turn.upsert\ndata: {\"event_type\":\"turn.upsert\",\"id\":\"t1\",\"session_id\":\"s1\",\"agent_id\":\"a1\",\"attempt\":1,\"status\":\"completed\",\"created_at\":\"2026-07-11T17:03:20Z\",\"updated_at\":\"2026-07-11T17:03:40Z\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript":
			http.Error(w, `{"error":{"code":"snapshot_unavailable"}}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)
	assert.False(t, turn.Next())
	assert.Equal(t, turn.Status(), "completed")
	assert.ErrorContains(t, turn.Err(), "reconcile terminal transcript")
	assert.ErrorContains(t, turn.Err(), "500")
}

func TestInvokeAgent_LazyStreamNeverOpensWithoutNext(t *testing.T) {
	var streamCalls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckWithCursor("s1", "t1", 42))
		default:
			atomic.AddInt32(&streamCalls, 1)
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName: "support",
		Content:   []map[string]interface{}{{"type": "text", "text": "hi"}},
	})
	assert.NoError(t, err)
	assert.Equal(t, turn.ID(), "t1")
	assert.Equal(t, atomic.LoadInt32(&streamCalls), int32(0)) // fire-and-forget: no stream request
}

func TestInvokeAgent_TerminalAckHydratesFromSnapshot(t *testing.T) {
	var streamCalls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/agents/invoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, invokeAckTerminal("s1", "t1"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/sessions/s1/transcript":
			w.Header().Set("Content-Type", "application/json")
			// Two pages: hydration must follow next_page_token until has_more
			// is false so Messages includes the older page.
			if r.URL.Query().Get("page_token") == "pt_2" {
				_, _ = io.WriteString(w, `{"messages":[{"id":"m_a","session_id":"s1","agent_id":"a1","role":"assistant","status":"final","turn_id":"t1","turn_index":1,"sequence":43,"entry_type":"message","content":[{"type":"text","text":"done"}],"created_at":"2026-07-11T17:03:21Z"}],"turns":[{"id":"t1","session_id":"s1","agent_id":"a1","attempt":1,"status":"completed","created_at":"2026-07-11T17:03:20Z","updated_at":"2026-07-11T17:03:40Z"}],"has_more":false,"resume_cursor":"43.9"}`)
				return
			}
			_, _ = io.WriteString(w, `{"messages":[{"id":"m_user","session_id":"s1","agent_id":"a1","role":"user","status":"final","turn_id":"t1","turn_index":0,"sequence":42,"entry_type":"message","content":[],"created_at":"2026-07-11T17:03:21Z"}],"turns":[],"has_more":true,"next_page_token":"pt_2","resume_cursor":"42.1"}`)
		default:
			atomic.AddInt32(&streamCalls, 1)
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	turn, err := c.InvokeAgent(context.Background(), InvokeAgentOptions{
		AgentName:      "support",
		Content:        []map[string]interface{}{{"type": "text", "text": "hi"}},
		IdempotencyKey: "evt_1",
	})
	assert.NoError(t, err)
	assert.True(t, turn.Deduped())
	assert.Equal(t, turn.Status(), "completed")

	steps := 0
	for turn.Next() {
		steps++
	}
	assert.NoError(t, turn.Err())

	assert.Equal(t, steps, 1)                                 // one snapshot hydration
	assert.Equal(t, atomic.LoadInt32(&streamCalls), int32(0)) // no SSE connection for a finished turn
	var ids []string
	for _, m := range turn.Messages() {
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

// invokeAckTerminal is a deduped invoke response whose turn already completed.
func invokeAckTerminal(sessionID, turnID string) string {
	return fmt.Sprintf(`{
		"after_sequence": 43,
		"deduped": true,
		"resume_cursor": "41.6",
		"session": {
			"id": %q, "agent_id": "a1", "origin": "api", "scope": "agent",
			"scope_name": "app", "scope_ref_id": "a1", "session_key": "app",
			"status": "active", "title": "", "visibility": "private", "version": 1,
			"message_count": 2, "token_input_total": 0, "token_output_total": 0,
			"created_at": "2026-05-27T00:00:00Z", "updated_at": "2026-05-27T00:00:00Z"
		},
		"turn": {
			"id": %q, "agent_id": "a1", "session_id": %q, "attempt": 1,
			"status": "completed", "created_at": "2026-05-27T00:00:00Z",
			"updated_at": "2026-05-27T00:00:00Z"
		}
	}`, sessionID, turnID, sessionID)
}
