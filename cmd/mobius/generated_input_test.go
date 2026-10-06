package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

func TestGeneratedMessagingBindingProgressUpdates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   []string
		present bool
	}{
		{name: "omitted"},
		{name: "explicit false", flags: []string{"--show-progress-updates=false"}, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					http.Error(w, "cannot read request", http.StatusBadRequest)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			args := []string{
				"agents", "save-messaging-binding", "agent_test",
				"--provider", "slack", "--integration-id", "integration_test",
				"--api-url", srv.URL, "--api-key", "mbx_test", "--output", "json",
			}
			result := newApp().Test(t, cli.TestArgs(append(args, tc.flags...)...))
			assert.True(t, result.Success(), "save binding failed: %v\nstderr: %s", result.Err, result.Stderr)

			var body map[string]any
			select {
			case raw := <-requests:
				assert.NoError(t, json.Unmarshal(raw, &body))
			default:
				t.Fatal("save binding did not send a request")
			}
			value, present := body["show_progress_updates"]
			assert.Equal(t, tc.present, present, "show_progress_updates presence")
			if tc.present {
				assert.Equal(t, any(false), value, "show_progress_updates value")
			}
		})
	}
}

func TestGeneratedCommandRejectsUnknownRequestFileField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	err := os.WriteFile(path, []byte("system_prompt: Be concise.\nmemory_context_typo:\n  mode: index\n"), 0o644)
	assert.NoError(t, err)

	result := newApp().Test(t, cli.TestArgs(
		"agents", "update", "agent_test",
		"--file", path,
		"--dry-run",
		"--api-key", "mbx_test",
	))

	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), `unknown field "memory_context_typo"`)
}

func TestGeneratedPreviewVisibilityRequiresAndSendsVisibility(t *testing.T) {
	missing := newApp().Test(t, cli.TestArgs(
		"agents", "preview-visibility-change", "agent_test",
		"--api-key", "mbx_test",
	))
	assert.False(t, missing.Success())
	assert.Contains(t, missing.Err.Error(), "visibility")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/agents/agent_test/visibility-impact", r.URL.Path)
		assert.Equal(t, "restricted", r.URL.Query().Get("visibility"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"agents", "preview-visibility-change", "agent_test",
		"--visibility", "restricted",
		"--api-url", srv.URL, "--api-key", "mbx_test", "--output", "json",
	))
	assert.True(t, result.Success(), "preview failed: %v\nstderr: %s", result.Err, result.Stderr)
}

func TestGeneratedSkillInstructionsReadTextFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instructions.md")
	instructions := "Check the diff and leave concise findings.\n"
	assert.NoError(t, os.WriteFile(path, []byte(instructions), 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"skills", "create",
		"--name", "Pull request review",
		"--instructions", "@"+path,
		"--dry-run",
		"--output", "json",
		"--api-key", "mbx_test",
	))

	assert.True(t, result.Success(), "dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	var body map[string]any
	assert.NoError(t, json.Unmarshal([]byte(result.Stdout), &body))
	assert.Equal(t, instructions, body["instructions"])
}

func TestGeneratedSkillInstructionsEscapeLeadingAt(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs(
		"skills", "create",
		"--name", "Pull request review",
		"--instructions", "@@mention this in the body",
		"--dry-run",
		"--output", "json",
		"--api-key", "mbx_test",
	))

	assert.True(t, result.Success(), "dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	var body map[string]any
	assert.NoError(t, json.Unmarshal([]byte(result.Stdout), &body))
	assert.Equal(t, "@mention this in the body", body["instructions"])
}

func TestGeneratedSkillInstructionsHelpDocumentsLeadingAtEscape(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("skills", "create", "--help"))
	assert.True(t, result.Success(), "skills create help failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, "Use @@ to escape a literal leading @")
}

func TestPreviewAgentVisibilityChangeRequiresVisibility(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs(
		"agents", "preview-visibility-change", "agent_test",
		"--api-key", "mbx_test",
	))

	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "missing required flag: --visibility")
}
