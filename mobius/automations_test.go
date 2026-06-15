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

func TestCreateAutomation_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/loops")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, automationJSON("loop_1", "research", 0))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	automation, err := c.CreateAutomation(context.Background(), AutomationOptions{
		Name: "research",
		Tags: map[string]string{"env": "test"},
	})

	assert.NoError(t, err)
	assert.Equal(t, automation.Id, "loop_1")
	assert.Equal(t, body["name"], "research")
	assert.Equal(t, body["tags"].(map[string]any)["env"], "test")
}

func TestUpdateAutomation_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/test-project/loops/loop_1":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &body))
			_, _ = io.WriteString(w, automationJSON("loop_1", "research v2", 1))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	automation, err := c.UpdateAutomation(context.Background(), "loop_1", UpdateAutomationOptions{
		Name:        "research v2",
		Description: "new description",
		Status:      api.LoopStatusActive,
	})

	assert.NoError(t, err)
	assert.Equal(t, automation.Name, "research v2")
	assert.Equal(t, body["name"], "research v2")
	assert.Equal(t, body["description"], "new description")
	assert.Equal(t, body["status"], "active")
}

func TestCreateAndPublishAutomationVersion_HighLevelClient(t *testing.T) {
	var versionBody map[string]any
	published := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/loops/loop_1/versions":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &versionBody))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, automationVersionJSON("lver_1", "loop_1", 1, "draft", automationSpecJSON("research")))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/loops/loop_1/versions/1/publication":
			published = true
			_, _ = io.WriteString(w, automationJSON("loop_1", "research", 1))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	version, err := c.CreateAutomationVersion(context.Background(), "loop_1", map[string]any{"name": "research", "steps": []any{}}, &AutomationVersionOptions{Publish: true})

	assert.NoError(t, err)
	assert.Equal(t, version.Id, "lver_1")
	assert.Equal(t, published, true)
	assert.Equal(t, versionBody["spec"].(map[string]any)["name"], "research")
}

func automationJSON(id, name string, publishedVersion int) string {
	published := "null"
	if publishedVersion > 0 {
		published = fmt.Sprintf("%d", publishedVersion)
	}
	return fmt.Sprintf(`{
		"id":%q,
		"org_id":"org_1",
		"project_id":"proj_1",
		"name":%q,
		"latest_version":1,
		"published_version":%s,
		"status":"active",
		"triggers":[],
		"created_at":"2026-05-27T00:00:00Z",
		"updated_at":"2026-05-27T00:00:00Z"
	}`, id, name, published)
}

func automationVersionJSON(id, automationID string, version int, status string, spec string) string {
	return fmt.Sprintf(`{
		"id":%q,
		"org_id":"org_1",
		"project_id":"proj_1",
			"loop_id":%q,
		"version":%d,
		"status":%q,
		"spec":%s,
		"created_at":"2026-05-27T00:00:00Z"
	}`, id, automationID, version, status, spec)
}

func automationSpecJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"steps":[]}`, name)
}
