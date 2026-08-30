package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

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
		"agents", "preview-agent-visibility-change", "agent_test",
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
		"agents", "preview-agent-visibility-change", "agent_test",
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
		"agents", "preview-agent-visibility-change", "agent_test",
		"--api-key", "mbx_test",
	))

	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "missing required flag: --visibility")
}

func TestLoopCreateHelpDocumentsStepAuthoring(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("loops", "create", "--help"))
	assert.True(t, result.Success(), "loops create help failed: %v\nstderr: %s", result.Err, result.Stderr)
	for _, want := range []string{
		"agent, action, sleep, wait_for_event, interaction, loop, or check",
		"config.parameters",
		"if is a predicate",
	} {
		assert.Contains(t, result.Stdout, want)
	}
}

func TestLoopUpdateHelpDocumentsStepAuthoring(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("loops", "update", "loop_test", "--help"))
	assert.True(t, result.Success(), "loops update help failed: %v\nstderr: %s", result.Err, result.Stderr)
	for _, want := range []string{
		"agent, action, sleep, wait_for_event, interaction, loop, or check",
		"config.parameters",
		"if is a predicate",
	} {
		assert.Contains(t, result.Stdout, want)
	}
}

func TestLoopCreateRequestAcceptsInteractionStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.yaml")
	assert.NoError(t, os.WriteFile(path, []byte(`name: approval
steps:
  - id: review
    kind: interaction
    config:
      protocol: request_approval
      targets: [usr_reviewer]
`), 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"loops", "create",
		"--file", path,
		"--dry-run",
		"--output", "json",
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "interaction dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	var body struct {
		Steps []struct {
			Kind   string `json:"kind"`
			Config struct {
				Protocol string   `json:"protocol"`
				Targets  []string `json:"targets"`
			} `json:"config"`
		} `json:"steps"`
	}
	assert.NoError(t, json.Unmarshal([]byte(result.Stdout), &body))
	assert.Equal(t, "***REDACTED***", body.Steps[0].Kind)
	assert.Equal(t, "***REDACTED***", body.Steps[0].Config.Protocol)
	assert.Equal(t, []string{"***REDACTED***"}, body.Steps[0].Config.Targets)
}
