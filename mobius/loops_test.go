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

func TestCreateLoop_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/loops")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, loopJSON("loop_1", "research"))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	loop, err := c.CreateLoop(context.Background(), LoopOptions{
		Name: "research",
		Tags: map[string]string{"env": "test"},
	})

	assert.NoError(t, err)
	assert.Equal(t, loop.Id, "loop_1")
	assert.Equal(t, body["name"], "research")
	assert.Equal(t, body["tags"].(map[string]any)["env"], "test")
}

func TestCreateLoopWithSpec_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/loops")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, loopJSON("loop_1", "research"))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	loop, err := c.CreateLoop(context.Background(), LoopOptions{
		Name:    "research",
		AgentID: "agent_1",
		Spec: map[string]any{
			"schema_version": "1",
			"steps":          []any{map[string]any{"key": "step_1", "kind": "agent"}},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, loop.Id, "loop_1")
	// Explicit options and the inline spec are both sent on the create body.
	assert.Equal(t, body["name"], "research")
	assert.Equal(t, body["agent_id"], "agent_1")
	assert.Equal(t, body["schema_version"], "1")
	steps := body["steps"].([]any)
	assert.Equal(t, len(steps), 1)
	assert.Equal(t, steps[0].(map[string]any)["key"], "step_1")
}

func TestUpdateLoop_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/test-project/loops/loop_1":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &body))
			_, _ = io.WriteString(w, loopJSON("loop_1", "research v2"))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	loop, err := c.UpdateLoop(context.Background(), "loop_1", UpdateLoopOptions{
		Name:        "research v2",
		Description: "new description",
		Status:      api.LoopStatusActive,
		Spec: map[string]any{
			"steps": []any{map[string]any{"key": "step_1", "kind": "agent"}},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, loop.Name, "research v2")
	assert.Equal(t, body["name"], "research v2")
	assert.Equal(t, body["description"], "new description")
	assert.Equal(t, body["status"], "active")
	assert.Equal(t, len(body["steps"].([]any)), 1)
}

func loopJSON(id, name string) string {
	return fmt.Sprintf(`{
		"id":%q,
		"name":%q,
		"status":"active",
		"triggers":[],
		"created_at":"2026-05-27T00:00:00Z",
		"updated_at":"2026-05-27T00:00:00Z"
	}`, id, name)
}
