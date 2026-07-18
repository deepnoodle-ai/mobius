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

func TestReplaceOAuthReturnOriginsClearSendsEmptyList(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/v1/organization/oauth-return-origins", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"origins":[]}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"organizations", "replace-oauth-return-origins",
		"--clear",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "clear failed: %v\nstderr: %s", result.Err, result.Stderr)

	var body struct {
		Origins []string `json:"origins"`
	}
	assert.NoError(t, json.Unmarshal(got, &body))
	assert.NotEqual(t, nil, body.Origins, "origins must be [], not null")
	assert.Equal(t, 0, len(body.Origins))
}

func TestReplaceOAuthReturnOriginsClearAndOriginsAreExclusive(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"organizations", "replace-oauth-return-origins",
		"--clear",
		"--origins", "https://app.partner.example",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "mutually exclusive")
	assert.Equal(t, 0, requests)
}

func TestReplaceOAuthReturnOriginsEmptyMentionsClear(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"organizations", "replace-oauth-return-origins",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "--origins is required")
	assert.Contains(t, result.Err.Error(), "--clear")
	assert.Equal(t, 0, requests)
}

func TestReplaceOAuthReturnOriginsSendsFlagOrigins(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"origins":["https://app.partner.example"]}`))
	}))
	defer srv.Close()

	result := newApp().Test(t, cli.TestArgs(
		"organizations", "replace-oauth-return-origins",
		"--origins", "https://app.partner.example",
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "replace failed: %v\nstderr: %s", result.Err, result.Stderr)

	var body struct {
		Origins []string `json:"origins"`
	}
	assert.NoError(t, json.Unmarshal(got, &body))
	assert.Equal(t, []string{"https://app.partner.example"}, body.Origins)
}

func TestReplaceOAuthReturnOriginsFromFile(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"origins":["https://app.partner.example"]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "origins.json")
	assert.NoError(t, os.WriteFile(path, []byte(`{"origins":["https://app.partner.example"]}`), 0o600))

	result := newApp().Test(t, cli.TestArgs(
		"organizations", "replace-oauth-return-origins",
		"--file", path,
		"--api-url", srv.URL,
		"--api-key", "mbx_test",
	))
	assert.True(t, result.Success(), "replace failed: %v\nstderr: %s", result.Err, result.Stderr)

	var body struct {
		Origins []string `json:"origins"`
	}
	assert.NoError(t, json.Unmarshal(got, &body))
	assert.Equal(t, []string{"https://app.partner.example"}, body.Origins)
}
