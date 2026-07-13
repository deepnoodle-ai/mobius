package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

func TestPrincipalsCreateWithRoleAndKey(t *testing.T) {
	var principalBody map[string]any
	var keyBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/default/roles":
			_, _ = w.Write([]byte(`{"items":[{"id":"role_worker","name":"Worker","permissions":[],"system_defined":true,"created_at":"2026-07-13T00:00:00Z","updated_at":"2026-07-13T00:00:00Z"}],"has_more":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/default/principals":
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&principalBody))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"principal_1","name":"worker-prod","kind":"service","state":"active","role_ids":["role_worker"],"created_at":"2026-07-13T00:00:00Z","updated_at":"2026-07-13T00:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/default/api-keys":
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&keyBody))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"key_1","name":"worker-prod-primary","key":"mbx_secret","key_prefix":"mbx_secr","principal_id":"principal_1","created_at":"2026-07-13T00:00:00Z","updated_at":"2026-07-13T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"principals", "create", "worker-prod",
		"--role", "Worker",
		"--with-key",
		"--expires-at", "2026-08-01T12:00:00Z",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--quiet",
	))
	assert.True(t, result.Success(), "principals create failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "worker-prod", principalBody["name"])
	assert.Equal(t, "role_worker", principalBody["role_ids"].([]any)[0])
	assert.Equal(t, "principal_1", keyBody["principal_id"])
	assert.Equal(t, "worker-prod-primary", keyBody["name"])
	assert.Equal(t, "2026-08-01T12:00:00Z", keyBody["expires_at"])
}

func TestAPIKeysCreateAcceptsBareRFC3339Expiry(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"key_1","name":"worker-key","key":"mbx_secret","key_prefix":"mbx_secr","principal_id":"principal_1","created_at":"2026-07-13T00:00:00Z","updated_at":"2026-07-13T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"api-keys", "create",
		"--name", "worker-key",
		"--principal-id", "principal_1",
		"--expires-at", "2026-08-01T12:00:00Z",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--quiet",
	))
	assert.True(t, result.Success(), "api-keys create failed: %v\nstderr: %s", result.Err, result.Stderr)
	want, err := time.Parse(time.RFC3339, "2026-08-01T12:00:00Z")
	assert.NoError(t, err)
	got, err := time.Parse(time.RFC3339, body["expires_at"].(string))
	assert.NoError(t, err)
	assert.Equal(t, want, got)
}
