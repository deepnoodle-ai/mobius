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

// A required bool has no "missing" zero value, so the command must reject an
// omitted value instead of sending false (which would demote a manager).
func TestGeneratedRequiredBoolRejectsOmittedValue(t *testing.T) {
	dir := t.TempDir()
	withManager := filepath.Join(dir, "with.json")
	assert.NoError(t, os.WriteFile(withManager, []byte(`{"manager": false}`), 0o644))
	withoutManager := filepath.Join(dir, "without.json")
	assert.NoError(t, os.WriteFile(withoutManager, []byte(`{}`), 0o644))

	for _, tc := range []struct {
		name  string
		flags []string
		ok    bool
	}{
		{name: "omitted"},
		{name: "file omits field", flags: []string{"--file", withoutManager}},
		{name: "explicit false flag", flags: []string{"--manager=false"}, ok: true},
		{name: "file sets false", flags: []string{"--file", withManager}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				"agents", "update-member", "agent_test", "principal_test",
				"--dry-run", "--api-key", "mbx_test",
			}
			result := newApp().Test(t, cli.TestArgs(append(args, tc.flags...)...))
			if tc.ok {
				assert.True(t, result.Success(), "update-member failed: %v\nstderr: %s", result.Err, result.Stderr)
				assert.Contains(t, result.Stdout, `"manager": false`)
				return
			}
			assert.False(t, result.Success())
			assert.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "--manager is required")
		})
	}
}

// restoreAgent's body is all optional and a bare restore keeps the old name,
// so the command must send an empty body instead of demanding a flag.
func TestGeneratedRestoreAgentAllowsBareRestore(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs(
		"agents", "restore-agent", "agent_test",
		"--dry-run", "--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "bare restore failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, "{}")
}

// skill_ids is a required full replace-set, so an explicit [] clears every
// skill and must reach the API. An omitted key is still an error.
func TestGeneratedRequiredListAllowsExplicitEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	assert.NoError(t, os.WriteFile(empty, []byte(`{"skill_ids": []}`), 0o644))
	omitted := filepath.Join(dir, "omitted.json")
	assert.NoError(t, os.WriteFile(omitted, []byte(`{}`), 0o644))

	run := func(path string) *cli.TestResult {
		return newApp().Test(t, cli.TestArgs(
			"agents", "replace-skill-assignments", "agent_test",
			"--file", path, "--dry-run", "--api-key", "mbx_test",
		))
	}

	result := run(empty)
	assert.True(t, result.Success(), "empty replace-set failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, `"skill_ids": []`)

	result = run(omitted)
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "--skill-ids is required")
}

// A timestamp is not a secret, so --dry-run prints it instead of redacting it.
func TestGeneratedDryRunShowsTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	err := os.WriteFile(path, []byte(`{"name": "pr-reviewer", "expected_updated_at": "2026-07-17T00:00:00Z"}`), 0o644)
	assert.NoError(t, err)

	result := newApp().Test(t, cli.TestArgs(
		"agents", "update", "agent_test",
		"--file", path, "--dry-run", "--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "dry run failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, `"expected_updated_at": "2026-07-17T00:00:00Z"`)
}
