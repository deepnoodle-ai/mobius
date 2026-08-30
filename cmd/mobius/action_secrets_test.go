package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

const testSigningSecret = "whsec_MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE"

func actionResponse(secret string) string {
	body := `{"id":"act_1","name":"crm-sync","endpoint_kind":"http","invocation_format":"signed_context_v1",` +
		`"owner":{"kind":"team"},"posture":"team",` +
		`"created_at":"2026-08-30T00:00:00Z","updated_at":"2026-08-30T00:00:00Z"`
	if secret != "" {
		body += fmt.Sprintf(`,"signing_secret":%q`, secret)
	}
	return body + `}`
}

func TestActionsCreateRequiresSecretSink(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	result := newApp().Test(t, cli.TestArgs("actions", "create", "--name", "crm-sync", "--endpoint-url", "https://example.com/hook", "--api-url", srv.URL, "--api-key", "mbx_test"))
	assert.False(t, result.Success())
	assert.Contains(t, result.Err.Error(), "--secret-file")
	assert.Equal(t, 0, requests)
}

func TestActionsCreateWritesSecretFileAndMasksOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/actions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(actionResponse(testSigningSecret)))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "action.secret")
	result := newApp().Test(t, cli.TestArgs("actions", "create", "--name", "crm-sync", "--endpoint-url", "https://example.com/hook", "--secret-file", dest, "--api-url", srv.URL, "--api-key", "mbx_test", "--output", "json"))
	assert.True(t, result.Success(), "create failed: %v\nstderr: %s", result.Err, result.Stderr)
	saved, err := os.ReadFile(dest)
	assert.NoError(t, err)
	assert.Equal(t, testSigningSecret+"\n", string(saved))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dest)
		assert.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assert.False(t, strings.Contains(result.Stdout, testSigningSecret))
	assert.False(t, strings.Contains(result.Stdout, "signing_secret"))
}

func TestActionsCreateForceReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink replacement semantics differ on Windows")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(actionResponse(testSigningSecret)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	dest := filepath.Join(dir, "action.secret")
	assert.NoError(t, os.WriteFile(victim, []byte("do not replace\n"), 0o644))
	assert.NoError(t, os.Symlink(victim, dest))

	result := newApp().Test(t, cli.TestArgs(
		"actions", "create", "--name", "crm-sync",
		"--endpoint-url", "https://example.com/hook",
		"--secret-file", dest, "--force",
		"--api-url", srv.URL, "--api-key", "mbx_test", "--output", "json",
	))
	assert.True(t, result.Success(), "create failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "do not replace\n", string(assertReadFile(t, victim)))
	assert.Equal(t, testSigningSecret+"\n", string(assertReadFile(t, dest)))
	info, err := os.Lstat(dest)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.False(t, info.Mode()&os.ModeSymlink != 0)
}

func assertReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	assert.NoError(t, err)
	return data
}

func TestActionsRotateSecretRequiresExplicitSink(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("actions", "rotate-secret", "crm-sync", "--api-key", "mbx_test"))
	assert.False(t, result.Success())
	assert.Contains(t, result.Err.Error(), "--show-secret")
}

func TestActionsRotateSecretCanShowReveal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/actions/crm-sync/secret/rotate", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"signing_secret":%q,"secret_version":2}`, testSigningSecret)))
	}))
	defer srv.Close()
	result := newApp().Test(t, cli.TestArgs("actions", "rotate-secret", "crm-sync", "--show-secret", "--api-url", srv.URL, "--api-key", "mbx_test", "--output", "json"))
	assert.True(t, result.Success(), "rotate failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, testSigningSecret)
}
