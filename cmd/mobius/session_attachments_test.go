package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"
)

func TestSessionsAttachStreamsFileToSession(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "brief.md")
	assert.NoError(t, os.WriteFile(path, []byte("# brief"), 0o644))

	fields := map[string]string{}
	var fileBody, idempotency, reqPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		reqPath = r.URL.Path
		idempotency = r.Header.Get("Idempotency-Key")
		mr, err := r.MultipartReader()
		assert.NoError(t, err)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			assert.NoError(t, err)
			data, err := io.ReadAll(part)
			assert.NoError(t, err)
			if part.FormName() == "file" {
				fileBody = string(data)
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"artifact":{"id":"art_1","name":"brief.md","mime_type":"text/markdown","size_bytes":7,"state":"ready","visibility":"private","created_at":"2026-07-17T00:00:00Z"},"content_block":{"type":"document"}}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"sessions", "attach", "sess_1", path,
			"--idempotency-key", "turn-1:brief",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "json",
		),
	)
	assert.True(t, result.Success(), "attach failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "/v1/sessions/sess_1/attachments", reqPath)
	assert.Equal(t, "# brief", fileBody)
	assert.Equal(t, "brief.md", fields["name"])
	assert.Equal(t, "7", fields["size_bytes"])
	assert.Equal(t, "turn-1:brief", idempotency)
	assert.Contains(t, result.Stdout, "art_1")
}

func TestSessionsAttachFromStdinRequiresName(t *testing.T) {
	result := newApp().Test(t,
		cli.TestArgs(
			"sessions", "attach", "sess_1", "-",
			"--api-url", "https://api.invalid",
			"--api-key", "mbx_test",
		),
	)
	assert.False(t, result.Success())
	assert.Contains(t, result.Stderr+result.Err.Error(), "--name is required")
}
