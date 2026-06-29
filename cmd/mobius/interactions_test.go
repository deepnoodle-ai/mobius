package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

// TestInteractionsCancelAllowsNoReason guards the generator behavior that an
// operation with an optional requestBody (cancelInteraction declares
// requestBody.required: false) does not force a flag or --file. A bare
// `interactions cancel <id>` is a valid, reasonless cancellation.
func TestInteractionsCancelAllowsNoReason(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/default/interactions/int_1/cancel", r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"int_1","status":"cancelled"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"interactions", "cancel", "int_1",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
		),
	)
	assert.True(t, result.Success(), "cancel failed: %v\nstderr: %s", result.Err, result.Stderr)
	// An empty/reasonless body is sent (no "at least one flag" rejection).
	assert.Equal(t, "{}", gotBody)
}

// TestInteractionsCancelSendsReason confirms the --reason flag still flows into
// the request body when provided.
func TestInteractionsCancelSendsReason(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"int_1","status":"cancelled"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"interactions", "cancel", "int_1",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--reason", "superseded",
		),
	)
	assert.True(t, result.Success(), "cancel failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "superseded", gotBody["reason"])
}
