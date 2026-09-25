package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestUploadSessionPDFStagesPartsThenCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.pdf")
	assert.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("%"), SessionPDFChunkBytes+5), 0o644))

	var calls []string
	var completeBody map[string]any
	var idempotencyKey string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut {
			calls = append(calls, fmt.Sprintf("PUT %s %d %s", r.URL.Path, len(body), r.Header.Get("Content-Type")))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		idempotencyKey = r.Header.Get("Idempotency-Key")
		assert.NoError(t, json.Unmarshal(body, &completeBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"artifact":{"id":"art_1","name":"big.pdf","mime_type":"application/pdf","size_bytes":1,"state":"ready","visibility":"private","created_at":"2026-07-17T00:00:00Z"},"content_block":{"type":"document"}}`))
	}))

	resp, err := c.UploadSessionPDF(context.Background(), "sess_1", UploadSessionPDFOptions{
		Path:           path,
		UploadID:       "7f1c3c9e-5b1a-4c55-9d5e-1c1f5a0b8e21",
		IdempotencyKey: "turn-1:big",
	})
	assert.NoError(t, err)
	assert.Equal(t, resp.Artifact.Id, "art_1")
	base := "/v1/sessions/sess_1/attachments/uploads/7f1c3c9e-5b1a-4c55-9d5e-1c1f5a0b8e21"
	assert.Equal(t, calls, []string{
		fmt.Sprintf("PUT %s/parts/0 %d application/octet-stream", base, SessionPDFChunkBytes),
		fmt.Sprintf("PUT %s/parts/1 5 application/octet-stream", base),
		"POST " + base + "/complete",
	})
	assert.Equal(t, idempotencyKey, "turn-1:big")
	assert.Equal(t, completeBody, map[string]any{
		"name":       "big.pdf",
		"size_bytes": float64(SessionPDFChunkBytes + 5),
		"part_count": float64(2),
	})
}

func TestUploadSessionPDFValidatesInput(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request expected")
	}))
	ctx := context.Background()

	_, err := c.UploadSessionPDF(ctx, "sess_1", UploadSessionPDFOptions{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of Path or Reader")

	_, err = c.UploadSessionPDF(ctx, "sess_1", UploadSessionPDFOptions{Reader: strings.NewReader("%PDF")})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")

	_, err = c.UploadSessionPDF(ctx, "sess_1", UploadSessionPDFOptions{Reader: strings.NewReader(""), Name: "a.pdf"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")

	_, err = c.UploadSessionPDF(ctx, "sess_1", UploadSessionPDFOptions{
		Reader: strings.NewReader("%PDF"), Name: "a.pdf", UploadID: "not-a-uuid",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "upload_id must be a UUID")

	_, err = c.UploadSessionPDF(ctx, "sess_1", UploadSessionPDFOptions{
		Reader: strings.NewReader("%PDF"), Name: "a.pdf", IdempotencyKey: strings.Repeat("k", 256),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at most 255 characters")
}

func TestDeleteSessionTurn(t *testing.T) {
	var seen []string
	status := http.StatusNoContent
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if status != http.StatusNoContent {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			_, _ = w.Write([]byte(`{"error":{"code":"turn_not_last","message":"not the last turn"}}`))
		}
	}))
	ctx := context.Background()

	assert.NoError(t, c.DeleteSessionTurn(ctx, "sess_1", "turn_2"))
	status = http.StatusConflict
	err := c.DeleteSessionTurn(ctx, "sess_1", "turn_1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "turn_not_last")
	assert.Equal(t, seen, []string{
		"DELETE /v1/sessions/sess_1/turns/turn_2",
		"DELETE /v1/sessions/sess_1/turns/turn_1",
	})
}
