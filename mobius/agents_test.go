package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func agentJSON(id string) string {
	return fmt.Sprintf(`{
		"id":%q,"principal_id":%q,"name":"PR reviewer","status":"active",
		"external_ref":"tenant-42/pr-reviewer",
		"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"
	}`, id, id)
}

func TestCreateAgentSendsAdoptFields(t *testing.T) {
	var got api.CreateAgentRequest
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/test-project/agents", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		// Adopt of an existing agent answers 200, not 201.
		writeJSON(w, http.StatusOK, agentJSON("agent_1"))
	}))

	agent, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent:         api.CreateAgentRequest{Name: "PR reviewer"},
		AdoptExisting: true,
		ExternalRef:   "tenant-42/pr-reviewer",
	})
	assert.NoError(t, err)
	assert.Equal(t, "agent_1", agent.Id)
	assert.NotNil(t, got.IfExists)
	assert.Equal(t, api.IfExistsAdopt, *got.IfExists)
	assert.NotNil(t, got.ExternalRef)
	assert.Equal(t, "tenant-42/pr-reviewer", *got.ExternalRef)
}

func TestCreateAgentPlainCreateOmitsIfExists(t *testing.T) {
	var got map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		writeJSON(w, http.StatusCreated, agentJSON("agent_1"))
	}))

	agent, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent: api.CreateAgentRequest{Name: "PR reviewer"},
	})
	assert.NoError(t, err)
	assert.Equal(t, "agent_1", agent.Id)
	if _, ok := got["if_exists"]; ok {
		t.Fatalf("plain create must not send if_exists, got %v", got["if_exists"])
	}
	if _, ok := got["external_ref"]; ok {
		t.Fatalf("plain create without ExternalRef must not send external_ref, got %v", got["external_ref"])
	}
}

func TestCreateAgentAdoptRequiresExternalRefBeforeHTTP(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request may be sent when the options are invalid")
	}))

	_, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent:         api.CreateAgentRequest{Name: "PR reviewer"},
		AdoptExisting: true,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ExternalRef")
}

func TestCreateAgentAdoptRetriesTransient503(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, agentJSON("agent_1"))
	}))

	agent, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent:         api.CreateAgentRequest{Name: "PR reviewer"},
		AdoptExisting: true,
		ExternalRef:   "tenant-42/pr-reviewer",
	})
	assert.NoError(t, err)
	assert.Equal(t, "agent_1", agent.Id)
	assert.Equal(t, 2, requests, "the transient 503 must be retried in adopt mode")
}

func TestCreateAgentPlainCreateIsNotRetried(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	_, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent: api.CreateAgentRequest{Name: "PR reviewer"},
	})
	assert.Error(t, err)
	assert.Equal(t, 1, requests, "a plain create POST must not be replayed")
}

func TestCreateAgentAdoptConflictCode(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict,
			`{"error":{"code":"external_identity_conflict","message":"external_ref is owned by a deleted agent"}}`)
	}))

	_, err := c.CreateAgent(context.Background(), CreateAgentOptions{
		Agent:         api.CreateAgentRequest{Name: "PR reviewer"},
		AdoptExisting: true,
		ExternalRef:   "tenant-42/pr-reviewer",
	})
	var apiErr *APIError
	assert.True(t, errors.As(err, &apiErr), "want APIError, got %v", err)
	assert.Equal(t, ErrCodeExternalIdentityConflict, apiErr.Code)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
}

func TestAgentLifecycleRoutes(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/test-project/agents":
			assert.Equal(t, "PR reviewer", r.URL.Query().Get("name"))
			assert.Equal(t, "active", r.URL.Query().Get("status"))
			assert.Equal(t, "5", r.URL.Query().Get("limit"))
			writeJSON(w, http.StatusOK, `{"items":[`+agentJSON("agent_1")+`]}`)
		case "GET /v1/projects/test-project/agents/agent_1":
			writeJSON(w, http.StatusOK, agentJSON("agent_1"))
		case "PATCH /v1/projects/test-project/agents/agent_1":
			var req api.UpdateAgentRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.NotNil(t, req.Description)
			writeJSON(w, http.StatusOK, agentJSON("agent_1"))
		case "DELETE /v1/projects/test-project/agents/agent_1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	page, err := c.ListAgents(ctx, &ListAgentsOptions{Name: "PR reviewer", Status: "active", Limit: 5})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(page.Items))
	assert.Equal(t, "agent_1", page.Items[0].Id)

	agent, err := c.GetAgent(ctx, "agent_1")
	assert.NoError(t, err)
	assert.Equal(t, "agent_1", agent.Id)

	description := "Reviews pull requests."
	_, err = c.UpdateAgent(ctx, "agent_1", api.UpdateAgentRequest{Description: &description})
	assert.NoError(t, err)

	assert.NoError(t, c.DeleteAgent(ctx, "agent_1"))
}
