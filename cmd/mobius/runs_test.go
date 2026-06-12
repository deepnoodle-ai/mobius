package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

func TestRunsStartUsesPolishedCommandName(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/default/loops/smoke/runs", r.URL.Path)
		assert.Equal(t, "Bearer mbx_test", r.Header.Get("Authorization"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"runs", "start", "smoke",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--inputs", `{"hello":"world"}`,
			"--quiet",
		),
	)
	assert.True(t, result.Success(), "runs start failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, map[string]any{"hello": "world"}, got["inputs"])
}

func TestRunsHelpUsesPolishedCommandNames(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("runs", "--help"))
	assert.True(t, result.Success(), "runs help failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, "start")
	assert.Contains(t, result.Stdout, "stream")
	assert.Contains(t, result.Stdout, "signal")
	assert.NotContains(t, result.Stdout, "start-run")
	assert.NotContains(t, result.Stdout, "stream-run")
	assert.NotContains(t, result.Stdout, "signal-run")
}
