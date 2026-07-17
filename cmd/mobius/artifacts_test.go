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

func TestArtifactsUploadStreamsFileWithContractFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "report.html")
	assert.NoError(t, os.WriteFile(path, []byte("<h1>report</h1>"), 0o644))

	fields := map[string]string{}
	var fileBody, lease, idempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/projects/default/artifacts", r.URL.Path)
		lease = r.Header.Get("X-Mobius-Lease-Token")
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
		_, _ = w.Write([]byte(`{"id":"art_1","name":"renders/report.html","mime_type":"text/html","size_bytes":15,"visibility":"private","created_at":"2026-07-17T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"artifacts", "upload", path,
			"--name", "renders/report.html",
			"--metadata", `{"renderer":"omni"}`,
			"--idempotency-key", "delivery-1:report",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "json",
		),
	)
	assert.True(t, result.Success(), "upload failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "<h1>report</h1>", fileBody)
	assert.Equal(t, "renders/report.html", fields["name"])
	assert.Equal(t, "text/html", fields["mime"])
	assert.Equal(t, "15", fields["size_bytes"])
	assert.Equal(t, `{"renderer":"omni"}`, fields["metadata"])
	assert.Equal(t, "", lease)
	assert.Equal(t, "delivery-1:report", idempotency)
	for _, banned := range []string{"run_id", "step_id", "visibility", "tags"} {
		_, present := fields[banned]
		assert.False(t, present, "legacy field %q was sent", banned)
	}
	assert.Contains(t, result.Stdout, `"id": "art_1"`)
}

func TestArtifactsUploadDefaultsNameAndInfersMime(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "notes.txt")
	assert.NoError(t, os.WriteFile(path, []byte("notes"), 0o644))

	fields := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		assert.NoError(t, err)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			assert.NoError(t, err)
			data, _ := io.ReadAll(part)
			if part.FormName() != "file" {
				fields[part.FormName()] = string(data)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"art_2","name":"notes.txt","mime_type":"text/plain","size_bytes":5,"visibility":"private","created_at":"2026-07-17T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"artifacts", "upload", path,
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--quiet",
		),
	)
	assert.True(t, result.Success(), "upload failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "notes.txt", fields["name"])
	assert.Equal(t, "text/plain", fields["mime"])
}

func TestArtifactsUploadFromStdinRequiresName(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs(
		"artifacts", "upload", "-",
		"--api-key", "mbx_test",
	))
	assert.False(t, result.Success())
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "--name is required")
}

func TestArtifactsUploadFromStdin(t *testing.T) {
	var fileBody string
	fields := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		assert.NoError(t, err)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			assert.NoError(t, err)
			data, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				fileBody = string(data)
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"art_3","name":"piped.txt","mime_type":"text/plain","size_bytes":11,"visibility":"private","created_at":"2026-07-17T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"artifacts", "upload", "-",
			"--name", "piped.txt",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--quiet",
		),
		cli.TestStdin("piped bytes"),
	)
	assert.True(t, result.Success(), "upload failed: %v\nstderr: %s", result.Err, result.Stderr)
	assert.Equal(t, "piped bytes", fileBody)
	assert.Equal(t, "piped.txt", fields["name"])
	_, sized := fields["size_bytes"]
	assert.False(t, sized, "stdin upload should not declare size_bytes")
}
