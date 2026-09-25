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

func TestArtifactsMoveRenamesTheWholeLineage(t *testing.T) {
	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(raw, &body))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"art_1","name":"reports/2026/weekly.md","mime_type":"text/markdown","size_bytes":9,"visibility":"organization","created_at":"2026-09-14T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "move", "art_1", "reports/2026/weekly.md",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--output", "json",
	))
	assert.True(t, result.Success(), "move failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, http.MethodPatch, method)
	assert.Equal(t, "/v1/artifacts/art_1", path)
	assert.Equal(t, "reports/2026/weekly.md", body["name"])
	assert.Contains(t, result.Stdout, `"name": "reports/2026/weekly.md"`)
}

func TestArtifactsListSendsFolderAndNoRecursive(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"has_more":false}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "list",
		"--folder", "reports/2026",
		"--no-recursive",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--quiet",
	))
	assert.True(t, result.Success(), "list failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, query, "folder=reports%2F2026")
	assert.Contains(t, query, "recursive=false")
}

func TestArtifactsListOmitsRecursiveByDefault(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"has_more":false}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "list",
		"--folder", "reports",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--quiet",
	))
	assert.True(t, result.Success(), "list failed: %v\nstderr: %s", result.Err, result.Stderr)
	// The server's own default is recursive, so an unset flag sends nothing.
	assert.NotContains(t, query, "recursive")
}

func TestArtifactsFoldersListsSubfolders(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/artifact-folders", r.URL.Path)
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"path":"reports/2026","name":"2026","declared":true,"id":"afd_1"},{"path":"reports/drafts","name":"drafts","declared":false}]}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "folders",
		"--parent", "reports",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--output", "pretty",
	))
	assert.True(t, result.Success(), "folders failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "parent=reports", query)
	for _, want := range []string{"name", "path", "declared", "2026", "reports/drafts"} {
		assert.Contains(t, result.Stdout, want)
	}
	assert.NotContains(t, result.Stdout, "afd_1")
}

func TestArtifactsMkdirDeclaresAPrivateFolder(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/artifact-folders", r.URL.Path)
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(raw, &body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"afd_1","path":"reports/2026","name":"2026","parent_path":"reports","owner":{"kind":"person","id":"usr_1"},"visibility":"private","created_at":"2026-09-14T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "mkdir", "reports/2026",
		"--visibility", "private",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--output", "json",
	))
	assert.True(t, result.Success(), "mkdir failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "reports/2026", body["path"])
	assert.Equal(t, "private", body["visibility"])
	assert.Contains(t, result.Stdout, `"id": "afd_1"`)
}

func TestArtifactsRmdirResolvesAPathToAFolderID(t *testing.T) {
	var listQuery, deletePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"path":"reports/2025","name":"2025","declared":true,"id":"afd_old"},{"path":"reports/2026","name":"2026","declared":true,"id":"afd_1"}]}`))
		case http.MethodDelete:
			deletePath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "reports/2026",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "rmdir failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "parent=reports", listQuery)
	assert.Equal(t, "/v1/artifact-folders/afd_1", deletePath)
}

func TestArtifactsRmdirTakesAFolderIDWithoutLookup(t *testing.T) {
	var listed bool
	var deletePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			listed = true
		}
		deletePath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "--id", "afd_1",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "rmdir failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.False(t, listed, "an explicit folder ID should not be looked up")
	assert.Equal(t, "/v1/artifact-folders/afd_1", deletePath)
}

func TestArtifactsRmdirReportsTheFileCountOnConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"folder_not_empty","message":"folder still has files","details":{"file_count":3}}}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "--id", "afd_1",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "still contains 3 files")
}

func TestArtifactsRmdirReportsSubfoldersWhenNoFilesBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"folder_not_empty","message":"folder is not empty","details":{"file_count":0}}}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "--id", "afd_1",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "still has declared subfolders")
}

func TestArtifactsRmdirResolvesAnAfdPrefixedPath(t *testing.T) {
	var listQuery, deletePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"path":"afd_reports/2026","name":"2026","declared":true,"id":"afd_9"}]}`))
		case http.MethodDelete:
			deletePath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "afd_reports/2026",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "rmdir failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "parent=afd_reports", listQuery)
	assert.Equal(t, "/v1/artifact-folders/afd_9", deletePath)
}

func TestArtifactsRmdirRequiresExactlyOneOfPathOrID(t *testing.T) {
	for _, args := range [][]string{
		{"artifacts", "rmdir"},
		{"artifacts", "rmdir", "reports", "--id", "afd_1"},
	} {
		result := newApp().Test(t, cli.TestArgs(append(args, "--api-key", "mbx_test")...))
		assert.False(t, result.Success())
		assert.Error(t, result.Err)
		assert.Contains(t, result.Err.Error(), "exactly one of")
	}
}

func TestArtifactsRmdirRefusesAnImpliedFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"path":"reports/drafts","name":"drafts","declared":false}]}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "rmdir", "reports/drafts",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "not declared")
}

func TestParentFolderPath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"reports", ""},
		{"reports/2026", "reports"},
		{"reports/2026/q3", "reports/2026"},
	} {
		if got := parentFolderPath(tc.path); got != tc.want {
			t.Fatalf("parentFolderPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
