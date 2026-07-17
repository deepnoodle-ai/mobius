package main

import (
	"encoding/base64"
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

func orgActionResponse(secret string) string {
	body := `{"id":"act_1","name":"crm.sync","endpoint_url":"https://example.com/hook",` +
		`"invocation_format":"signed_context_v1","enabled":true,"secret_ref":"osec_abc",`
	if secret != "" {
		body += fmt.Sprintf(`"signing_secret":%q,`, secret)
	}
	return body + `"secret_versions":[{"version":1,"status":"active","created_at":"2026-07-17T00:00:00Z"}],` +
		`"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"}`
}

func TestOrgActionsCreateRequiresSecretSink(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "create",
		"--name", "crm.sync",
		"--endpoint-url", "https://example.com/hook",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "--secret-file")
	assert.Contains(t, result.Err.Error(), "--show-secret")
	assert.Equal(t, 0, requests, "a refused invocation must not burn the one-time reveal")
}

func TestOrgActionsCreateWritesSecretFileAndMasksOutput(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("signing-key-bytes"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/organization/actions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(orgActionResponse(secret)))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "action.secret")
	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "create",
		"--name", "crm.sync",
		"--endpoint-url", "https://example.com/hook",
		"--secret-file", dest,
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--output", "json",
	))
	assert.True(t, result.Success(), "create failed: %v\nstderr: %s", result.Err, result.Stderr)

	saved, err := os.ReadFile(dest)
	assert.NoError(t, err)
	assert.Equal(t, secret+"\n", string(saved))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dest)
		assert.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	assert.False(t, strings.Contains(result.Stdout, secret), "stdout must not contain the secret")
	assert.False(t, strings.Contains(result.Stdout, "signing_secret"), "masked output should omit signing_secret")
	assert.Contains(t, result.Stdout, `"secret_ref": "osec_abc"`)
	assert.Contains(t, result.Stderr, "version 1")
	assert.Contains(t, result.Stderr, dest)
}

func TestOrgActionsCreateShowSecretPrintsFullResponse(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("signing-key-bytes"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(orgActionResponse(secret)))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "create",
		"--name", "crm.sync",
		"--endpoint-url", "https://example.com/hook",
		"--show-secret",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--output", "json",
	))
	assert.True(t, result.Success(), "create failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Contains(t, result.Stdout, secret)
}

func TestOrgActionsRotateSecretFileRefusesOverwrite(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "action.secret")
	assert.NoError(t, os.WriteFile(dest, []byte("existing"), 0o600))

	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "rotate-secret", "act_1",
		"--secret-file", dest,
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "--force")
	assert.Equal(t, 0, requests)

	saved, err := os.ReadFile(dest)
	assert.NoError(t, err)
	assert.Equal(t, "existing", string(saved))
}

func TestOrgActionsRotateSecretFileWithForce(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("rotated-key"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/organization/actions/act_1/secret/rotate", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(orgActionResponse(secret)))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "action.secret")
	assert.NoError(t, os.WriteFile(dest, []byte("old"), 0o600))

	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "rotate-secret", "act_1",
		"--secret-file", dest, "--force",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
		"--quiet",
	))
	assert.True(t, result.Success(), "rotate failed: %v\nstderr: %s", result.Err, result.Stderr)

	saved, err := os.ReadFile(dest)
	assert.NoError(t, err)
	assert.Equal(t, secret+"\n", string(saved))
}

func TestOrgActionsSecretSinkFlagsAreMutuallyExclusive(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs(
		"org-actions", "rotate-secret", "act_1",
		"--secret-file", "x", "--show-secret",
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "mutually exclusive")
}
