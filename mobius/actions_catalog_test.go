package mobius

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const catalogListJSON = `{
  "items": [
    {
      "name": "render-template",
      "title": "Render template",
      "source": "platform",
      "endpoint_kind": "builtin",
      "risk": "low",
      "readiness": "ready",
      "annotations": {}
    },
    {
      "name": "slack.send_message",
      "title": "Send Slack message",
      "source": "platform",
      "integration": "slack",
      "endpoint_kind": "http",
      "risk": "medium",
      "readiness": "needs_setup",
      "readiness_reason": "not_configured",
      "annotations": {}
    }
  ]
}`

const catalogEntryJSON = `{
  "name": "render-template",
  "title": "Render template",
  "source": "platform",
  "endpoint_kind": "builtin",
  "risk": "low",
  "readiness": "ready",
  "annotations": {}
}`

func TestListActionCatalog(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/catalog/actions")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, catalogListJSON)
	})
	c, _ := newTestClient(t, h)

	entries, err := c.ListActionCatalog(context.Background())
	assert.NoError(t, err)
	assert.Len(t, entries, 2)
	assert.Equal(t, entries[0].Name, "render-template")
	assert.Equal(t, entries[0].Readiness, api.CapabilityReadinessReady)
	assert.Equal(t, entries[1].Name, "slack.send_message")
	// Surface the disambiguation feedback #5 calls out: "exists but
	// integration not configured" must be visible to callers without
	// having to call RunServerAction blind. The entry is registered but
	// needs_setup until the Slack integration is connected.
	assert.Equal(t, entries[1].Readiness, api.CapabilityReadinessNeedsSetup)
	assert.NotNil(t, entries[1].Integration)
	assert.Equal(t, *entries[1].Integration, "slack")
}

func TestGetActionCatalogEntry(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodGet)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/catalog/actions/render-template")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, catalogEntryJSON)
	})
	c, _ := newTestClient(t, h)

	entry, err := c.GetActionCatalogEntry(context.Background(), "render-template")
	assert.NoError(t, err)
	assert.Equal(t, entry.Name, "render-template")
	assert.Equal(t, entry.Readiness, api.CapabilityReadinessReady)
}

func TestGetActionCatalogEntry_RequiresName(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	_, err := c.GetActionCatalogEntry(context.Background(), "")
	assert.True(t, err != nil)
}

func TestGetActionCatalogEntry_NotFoundSurfacesAsError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"action not found: echo"}}`)
	})
	c, _ := newTestClient(t, h)
	_, err := c.GetActionCatalogEntry(context.Background(), "echo")
	assert.True(t, err != nil)
}
