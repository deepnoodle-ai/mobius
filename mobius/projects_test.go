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

func projectJSON(id string) string {
	return fmt.Sprintf(`{
		"id":%q,"name":"Product Ops","handle":"product-ops","access_mode":"restricted",
		"external_ref":"workspace-42",
		"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"
	}`, id)
}

func TestCreateProjectSendsAdoptFields(t *testing.T) {
	var got api.CreateProjectRequest
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		// Adopt of an existing project answers 200, not 201.
		writeJSON(w, http.StatusOK, projectJSON("prj_1"))
	}))

	project, err := c.CreateProject(context.Background(), CreateProjectOptions{
		Project:       api.CreateProjectRequest{Name: "Product Ops"},
		AdoptExisting: true,
		ExternalRef:   "workspace-42",
	})
	assert.NoError(t, err)
	assert.Equal(t, "prj_1", project.Id)
	assert.NotNil(t, got.IfExists)
	assert.Equal(t, api.IfExistsAdopt, *got.IfExists)
	assert.NotNil(t, got.ExternalRef)
	assert.Equal(t, "workspace-42", *got.ExternalRef)
}

func TestCreateProjectPlainCreateReturns201(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(w, http.StatusCreated, projectJSON("prj_1"))
	}))

	project, err := c.CreateProject(context.Background(), CreateProjectOptions{
		Project: api.CreateProjectRequest{Name: "Product Ops"},
	})
	assert.NoError(t, err)
	assert.Equal(t, "prj_1", project.Id)
	assert.Equal(t, 1, requests)
}

func TestCreateProjectAdoptRequiresExternalRefBeforeHTTP(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request may be sent when the options are invalid")
	}))

	_, err := c.CreateProject(context.Background(), CreateProjectOptions{
		Project:       api.CreateProjectRequest{Name: "Product Ops"},
		AdoptExisting: true,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ExternalRef")
}

func TestCreateProjectAdoptRetriesTransient503(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, projectJSON("prj_1"))
	}))

	project, err := c.CreateProject(context.Background(), CreateProjectOptions{
		Project:       api.CreateProjectRequest{Name: "Product Ops"},
		AdoptExisting: true,
		ExternalRef:   "workspace-42",
	})
	assert.NoError(t, err)
	assert.Equal(t, "prj_1", project.Id)
	assert.Equal(t, 2, requests, "the transient 503 must be retried in adopt mode")
}

func TestCreateProjectPlainCreateIsNotRetried(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	_, err := c.CreateProject(context.Background(), CreateProjectOptions{
		Project: api.CreateProjectRequest{Name: "Product Ops"},
	})
	assert.Error(t, err)
	assert.Equal(t, 1, requests, "a plain create POST must not be replayed")
}

func TestCreateProjectAdoptConflictCodes(t *testing.T) {
	for _, code := range []string{ErrCodeExternalIdentityConflict, ErrCodeProjectArchived} {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, fmt.Sprintf(`{"error":{"code":%q,"message":"adopt conflict"}}`, code))
		}))

		_, err := c.CreateProject(context.Background(), CreateProjectOptions{
			Project:       api.CreateProjectRequest{Name: "Product Ops"},
			AdoptExisting: true,
			ExternalRef:   "workspace-42",
		})
		var apiErr *APIError
		assert.True(t, errors.As(err, &apiErr), "code %s: want APIError, got %v", code, err)
		assert.Equal(t, code, apiErr.Code)
		assert.Equal(t, http.StatusConflict, apiErr.Status)
	}
}
