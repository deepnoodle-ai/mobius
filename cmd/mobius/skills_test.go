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

const skillDoc = "---\nallowed_tools:\n  - github.create_review_comment\n---\nCheck the diff and leave concise findings.\n"

const skillResponse = `{"id":"skill_1","name":"Pull request review","source":"project",` +
	`"instructions":"Check the diff and leave concise findings.",` +
	`"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"}`

func TestSkillsImportSendsDocumentFileVerbatim(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/proj/skills/import", r.URL.Path)
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(raw, &got))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(skillResponse))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "SKILL.md")
	assert.NoError(t, os.WriteFile(path, []byte(skillDoc), 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"skills", "import", path,
		"--name", "Pull request review",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--project", "proj",
	))
	assert.True(t, result.Success(), "import failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, skillDoc, got["content"], "document must be sent verbatim, not parsed as a request body")
	assert.Equal(t, "Pull request review", got["name"])
	assert.Contains(t, result.Stdout, "skill_1")
}

func TestSkillsImportReadsStdinAndOmitsName(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(raw, &got))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(skillResponse))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"skills", "import", "-",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--project", "proj",
		),
		cli.TestStdin(skillDoc),
	)
	assert.True(t, result.Success(), "import failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, skillDoc, got["content"])
	_, present := got["name"]
	assert.False(t, present, "name must be omitted unless --name is set")
}

func TestOrgSkillsImportHitsOrganizationRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/organization/skills/import", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(skillResponse))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "SKILL.md")
	assert.NoError(t, os.WriteFile(path, []byte(skillDoc), 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"org-skills", "import", path,
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "import failed: %v\nstderr: %s", result.Err, result.Stderr)
}

func TestSkillsImportDryRunMakesNoRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "SKILL.md")
	assert.NoError(t, os.WriteFile(path, []byte(skillDoc), 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"skills", "import", path, "--dry-run",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--project", "proj",
	))
	assert.True(t, result.Success(), "dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, 0, requests)
	assert.Contains(t, result.Stdout, "allowed_tools")
}

func TestSkillsImportRejectsEmptyDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	assert.NoError(t, os.WriteFile(path, nil, 0o644))

	result := newApp().Test(t, cli.TestArgs(
		"skills", "import", path,
		"--api-key", "mbx_test",
		"--project", "proj",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "empty")
}
