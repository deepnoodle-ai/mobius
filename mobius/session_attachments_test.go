package mobius

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

func TestCreateSessionAttachmentUploadsToSessionScopedPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "brief.md")
	assert.NoError(t, os.WriteFile(path, []byte("# brief"), 0o644))

	var got artifactUploadCapture
	var gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got = captureMultipartUpload(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"artifact":{"id":"art_1","name":"brief.md","mime_type":"text/markdown","size_bytes":7,"state":"ready","visibility":"private","created_at":"2026-07-17T00:00:00Z"},"content_block":{"type":"document"}}`))
	}))

	resp, err := c.CreateSessionAttachment(context.Background(), "sess_1", CreateSessionAttachmentOptions{
		Path:           path,
		Mime:           "text/markdown",
		IdempotencyKey: "turn-1:brief",
	})
	assert.NoError(t, err)
	assert.Equal(t, gotPath, "/v1/sessions/sess_1/attachments")
	assert.Equal(t, resp.Artifact.Id, "art_1")
	assert.Equal(t, got.fileBody, "# brief")
	assert.Equal(t, got.idempotencyKey, "turn-1:brief")
	assert.Equal(t, got.fields["name"], "brief.md")
	assert.Equal(t, got.fields["mime"], "text/markdown")
	assert.Equal(t, got.fields["size_bytes"], "7")
}

func TestCreateSessionAttachmentValidatesOptions(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request expected")
	}))
	ctx := context.Background()

	_, err := c.CreateSessionAttachment(ctx, "  ", CreateSessionAttachmentOptions{Path: "x"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "session_id is required")

	_, err = c.CreateSessionAttachment(ctx, "sess_1", CreateSessionAttachmentOptions{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of Path or Reader")

	_, err = c.CreateSessionAttachment(ctx, "sess_1", CreateSessionAttachmentOptions{
		Reader: strings.NewReader("x"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Name is required")

	_, err = c.CreateSessionAttachment(ctx, "sess_1", CreateSessionAttachmentOptions{
		Path:           "x",
		IdempotencyKey: strings.Repeat("k", 256),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at most 255 characters")
}

func TestDeleteSessionAttachmentRequiresIdentifiers(t *testing.T) {
	deleted := ""
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleted = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx := context.Background()

	assert.Error(t, c.DeleteSessionAttachment(ctx, "", "art_1"))
	assert.Error(t, c.DeleteSessionAttachment(ctx, "sess_1", ""))
	assert.NoError(t, c.DeleteSessionAttachment(ctx, "sess_1", "art_1"))
	assert.Equal(t, deleted, "DELETE /v1/sessions/sess_1/attachments/art_1")
}
