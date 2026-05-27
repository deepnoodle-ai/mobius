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
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/automations")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, automationJSON("aut_1", "research", "research", 0))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	automation, err := c.CreateAutomation(context.Background(), AutomationOptions{
		Name:   "research",
		Handle: "research",
		Tags:   map[string]string{"env": "test"},
	})

	assert.NoError(t, err)
	assert.Equal(t, automation.Id, "aut_1")
	assert.Equal(t, body["name"], "research")
	assert.Equal(t, body["handle"], "research")
	assert.Equal(t, body["tags"].(map[string]any)["env"], "test")
}

func TestUpdateAutomation_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPatch)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/automations/research")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, automationJSON("aut_1", "research v2", "research", 1))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	automation, err := c.UpdateAutomation(context.Background(), "research", UpdateAutomationOptions{
		Name:        "research v2",
		Description: "new description",
		Status:      api.AutomationStatusActive,
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
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/automations/research/versions":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &versionBody))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, automationVersionJSON("autv_1", "aut_1", 1, "draft", automationSpecJSON("research")))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/automations/research/versions/1/publication":
			published = true
			_, _ = io.WriteString(w, automationJSON("aut_1", "research", "research", 1))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	version, err := c.CreateAutomationVersion(context.Background(), "research", map[string]any{"name": "research", "steps": []any{}}, &AutomationVersionOptions{Publish: true})

	assert.NoError(t, err)
	assert.Equal(t, version.Id, "autv_1")
	assert.Equal(t, published, true)
	assert.Equal(t, versionBody["spec"].(map[string]any)["name"], "research")
}

func TestEnsureAutomation_CreatesMissingAutomationAndVersion(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/automations":
			_, _ = io.WriteString(w, `{"items":[],"has_more":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/automations":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, automationJSON("aut_1", "research", "research", 0))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/automations/research/versions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, automationVersionJSON("autv_1", "aut_1", 1, "draft", automationSpecJSON("research")))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	result, err := c.EnsureAutomation(context.Background(), map[string]any{"name": "research", "handle": "research", "steps": []any{}}, AutomationOptions{}, nil)

	assert.NoError(t, err)
	assert.Equal(t, result.Created, true)
	assert.Equal(t, result.Updated, false)
	assert.Equal(t, result.Automation.Id, "aut_1")
	assert.Equal(t, result.Version.Id, "autv_1")
}

func TestEnsureAutomation_UpdatesChangedMetadata(t *testing.T) {
	var updateBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/automations":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"items":[%s],"has_more":false}`, automationJSON("aut_1", "research", "research", 1)))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/test-project/automations/research":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &updateBody))
			_, _ = io.WriteString(w, automationJSON("aut_1", "research v2", "research", 1))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/automations/research/versions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, automationVersionJSON("autv_2", "aut_1", 2, "draft", automationSpecJSON("research v2")))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	result, err := c.EnsureAutomation(context.Background(), map[string]any{"name": "research v2", "handle": "research", "steps": []any{}}, AutomationOptions{Name: "research v2", Handle: "research"}, nil)

	assert.NoError(t, err)
	assert.Equal(t, result.Created, false)
	assert.Equal(t, result.Updated, true)
	assert.Equal(t, updateBody["name"], "research v2")
}

func automationJSON(id, name, handle string, publishedVersion int) string {
	published := "null"
	if publishedVersion > 0 {
		published = fmt.Sprintf("%d", publishedVersion)
	}
	return fmt.Sprintf(`{
		"id":%q,
		"org_id":"org_1",
		"project_id":"proj_1",
		"name":%q,
		"handle":%q,
		"latest_version":1,
		"published_version":%s,
		"status":"active",
		"triggers":[],
		"created_at":"2026-05-27T00:00:00Z",
		"updated_at":"2026-05-27T00:00:00Z"
	}`, id, name, handle, published)
}

func automationVersionJSON(id, automationID string, version int, status string, spec string) string {
	return fmt.Sprintf(`{
		"id":%q,
		"org_id":"org_1",
		"project_id":"proj_1",
		"automation_id":%q,
		"version":%d,
		"status":%q,
		"spec":%s,
		"created_at":"2026-05-27T00:00:00Z"
	}`, id, automationID, version, status, spec)
}

func automationSpecJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"steps":[]}`, name)
}
