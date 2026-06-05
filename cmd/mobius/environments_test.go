package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

func TestStartWorkerCommandAcceptsArgvAfterDelimiter(t *testing.T) {
	var got struct {
		Command        []string `json:"command"`
		ManagedRuntime *bool    `json:"managed_runtime"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/default/environments/env_test/workers/start", r.URL.Path)
		assert.Equal(t, "Bearer mbx_test", r.Header.Get("Authorization"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"environments", "start-worker", "env_test",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--managed-runtime=false",
			"--command", "sh",
			"--quiet",
			"--",
			"-lc",
			"echo hi",
			"--literal",
		),
	)
	assert.True(t, result.Success(), "start-worker failed: %v\nstderr: %s", result.Err, result.Stderr)

	want := []string{"sh", "-lc", "echo hi", "--literal"}
	assert.Equal(t, want, got.Command)
	assert.NotNil(t, got.ManagedRuntime)
	assert.False(t, *got.ManagedRuntime)
}

func TestStartWorkerCommandHelpExplainsDashPrefixedArgv(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("environments", "start-worker", "--help"))
	assert.True(t, result.Success(), "start-worker help failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, "--command=<value>")
	assert.Contains(t, result.Stdout, "argv after --")
}
