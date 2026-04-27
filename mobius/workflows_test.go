package mobius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestCreateWorkflow_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/workflows")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research", "research", workflowSpecJSON("research")))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	def, err := c.CreateWorkflow(context.Background(), api.WorkflowSpec{Name: "research", Steps: []api.WorkflowStep{}}, &WorkflowOptions{
		Handle: "research",
		Tags:   map[string]string{"env": "test"},
	})

	assert.NoError(t, err)
	assert.Equal(t, def.Id, "wf_1")
	assert.Equal(t, body["name"], "research")
	assert.Equal(t, body["handle"], "research")
	assert.Equal(t, body["tags"].(map[string]any)["env"], "test")
}

func TestUpdateWorkflow_HighLevelClient(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPatch)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/workflows/wf_1")
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research v2", "research", workflowSpecJSON("research v2")))
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	spec := api.WorkflowSpec{Name: "research v2", Steps: []api.WorkflowStep{}}
	def, err := c.UpdateWorkflow(context.Background(), "wf_1", &UpdateWorkflowOptions{
		Name: "research v2",
		Spec: &spec,
	})

	assert.NoError(t, err)
	assert.Equal(t, def.Name, "research v2")
	assert.Equal(t, body["name"], "research v2")
	assert.NotEqual(t, body["spec"], nil)
}

func TestEnsureWorkflow_CreatesMissingDefinition(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/workflows":
			_, _ = io.WriteString(w, `{"items":[],"has_more":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/workflows":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research", "research", workflowSpecJSON("research")))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	result, err := c.EnsureWorkflow(context.Background(), api.WorkflowSpec{Name: "research", Steps: []api.WorkflowStep{}}, &WorkflowOptions{Handle: "research"})

	assert.NoError(t, err)
	assert.Equal(t, result.Created, true)
	assert.Equal(t, result.Updated, false)
	assert.Equal(t, result.Definition.Id, "wf_1")
}

func TestEnsureWorkflow_UpdatesChangedDefinition(t *testing.T) {
	var updateBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/workflows":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"items":[%s],"has_more":false}`, workflowDefinitionSummaryJSON("wf_1", "research", "research")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/workflows/wf_1":
			_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research", "research", workflowSpecJSON("old")))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/test-project/workflows/wf_1":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &updateBody))
			_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research", "research", workflowSpecJSON("research")))
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	result, err := c.EnsureWorkflow(context.Background(), api.WorkflowSpec{Name: "research", Steps: []api.WorkflowStep{}}, &WorkflowOptions{Handle: "research"})

	assert.NoError(t, err)
	assert.Equal(t, result.Created, false)
	assert.Equal(t, result.Updated, true)
	assert.NotEqual(t, updateBody["spec"], nil)
}

func TestEnsureWorkflow_NoopsUnchangedDefinition(t *testing.T) {
	patchSeen := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/workflows":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"items":[%s],"has_more":false}`, workflowDefinitionSummaryJSON("wf_1", "research", "research")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/test-project/workflows/wf_1":
			_, _ = io.WriteString(w, workflowDefinitionJSON("wf_1", "research", "research", workflowSpecJSON("research")))
		case r.Method == http.MethodPatch:
			patchSeen = true
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()

	result, err := c.EnsureWorkflow(context.Background(), api.WorkflowSpec{Name: "research", Steps: []api.WorkflowStep{}}, &WorkflowOptions{Handle: "research"})

	assert.NoError(t, err)
	assert.Equal(t, result.Created, false)
	assert.Equal(t, result.Updated, false)
	assert.Equal(t, patchSeen, false)
}

func workflowDefinitionJSON(id, name, handle, spec string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"handle":%q,"latest_version":1,"created_by":"user_1","created_at":"2026-04-27T00:00:00Z","updated_at":"2026-04-27T00:00:00Z","spec":%s}`, id, name, handle, spec)
}

func workflowDefinitionSummaryJSON(id, name, handle string) string {
	return strings.TrimSuffix(workflowDefinitionJSON(id, name, handle, workflowSpecJSON(name)), fmt.Sprintf(`,"spec":%s}`, workflowSpecJSON(name))) + "}"
}

func workflowSpecJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"steps":[]}`, name)
}
