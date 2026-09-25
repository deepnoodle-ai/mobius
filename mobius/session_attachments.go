package mobius

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// CreateSessionAttachmentOptions configures a session attachment upload.
// Exactly one of Path or Reader must supply the file bytes.
type CreateSessionAttachmentOptions struct {
	// Path streams the attachment from a file without buffering it in memory.
	Path string
	// Reader streams the attachment from an arbitrary source. Name is
	// required with Reader.
	Reader io.Reader
	// Name is the display filename recorded for the attachment. Defaults to
	// the base name of Path.
	Name string
	// Mime is a hint only: the server detects and validates the media type
	// from the uploaded bytes.
	Mime string
	// SizeBytes declares the exact content length; when supplied it must
	// match the uploaded bytes. Derived from the file size when using Path.
	SizeBytes int64
	// IdempotencyKey is a retry key scoped to this session and caller (255
	// chars max). An identical retry returns the original attachment.
	IdempotencyKey string
}

// CreateSessionAttachment uploads one document or image into server-managed
// artifact storage and binds it immutably to the session. The returned
// content block is the canonical block to append in a session message or turn
// input.
func (c *Client) CreateSessionAttachment(ctx context.Context, sessionID string, opts CreateSessionAttachmentOptions) (*api.SessionAttachmentResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("mobius: session_id is required")
	}
	if len(opts.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("mobius: attachment IdempotencyKey must be at most 255 characters")
	}
	upload, closeSource, err := newArtifactUpload("attachment", uploadSource{
		Path:      opts.Path,
		Reader:    opts.Reader,
		Name:      opts.Name,
		Mime:      opts.Mime,
		SizeBytes: opts.SizeBytes,
	})
	if err != nil {
		return nil, err
	}
	defer closeSource()
	upload.idempotencyKey = opts.IdempotencyKey
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/attachments"
	var out api.SessionAttachmentResponse
	if err := c.postArtifactMultipart(ctx, path, upload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSessionAttachment deletes one artifact created through this session's
// attachment endpoint. Deleting an artifact the session already deleted
// succeeds.
func (c *Client) DeleteSessionAttachment(ctx context.Context, sessionID, artifactID string) error {
	if c == nil {
		return fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("mobius: session_id is required")
	}
	if strings.TrimSpace(artifactID) == "" {
		return fmt.Errorf("mobius: artifact_id is required")
	}
	resp, err := c.ac.DeleteSessionAttachmentWithResponse(ctx,
		api.SessionIdParam(sessionID), api.ArtifactIdParam(artifactID))
	if err != nil {
		return fmt.Errorf("mobius: delete session attachment: %w", err)
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return unexpectedSessionStatus("delete session attachment", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return nil
}

const (
	// SessionPDFChunkBytes is the staged PDF part size: every part but the
	// last is exactly this long.
	SessionPDFChunkBytes = 8 * 1024 * 1024
	// SessionPDFMaxBytes is the largest PDF the staged upload accepts.
	SessionPDFMaxBytes = 100 * 1024 * 1024
)

// UploadSessionPDFOptions configures a staged PDF upload. Exactly one of Path
// or Reader must supply the file bytes.
type UploadSessionPDFOptions struct {
	// Path reads the PDF from a file.
	Path string
	// Reader reads the PDF from an arbitrary source. Name is required with
	// Reader.
	Reader io.Reader
	// Name is the display filename recorded for the PDF. Defaults to the base
	// name of Path.
	Name string
	// IdempotencyKey is a retry key for the completing request, scoped to
	// this session and caller (255 chars max).
	IdempotencyKey string
	// UploadID is the v4 UUID the parts are staged under. Generated when
	// empty; reuse it to resume a failed upload.
	UploadID string
}

// UploadSessionPDF uploads a PDF of up to 100 MiB through the staged chunk
// endpoints and binds it to the session. PDFs over 30 MiB must take this path;
// smaller ones may. Parts are 8 MiB and each is retried independently; the
// completing request validates the whole PDF and returns the same response as
// CreateSessionAttachment.
func (c *Client) UploadSessionPDF(ctx context.Context, sessionID string, opts UploadSessionPDFOptions) (*api.SessionAttachmentResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("mobius: session_id is required")
	}
	if (opts.Path == "") == (opts.Reader == nil) {
		return nil, fmt.Errorf("mobius: PDF upload requires exactly one of Path or Reader")
	}
	name := opts.Name
	source := opts.Reader
	if opts.Path != "" {
		f, err := os.Open(opts.Path)
		if err != nil {
			return nil, fmt.Errorf("mobius: open PDF: %w", err)
		}
		defer f.Close()
		source = f
		if name == "" {
			name = filepath.Base(opts.Path)
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("mobius: PDF name is required")
	}
	uploadID := opts.UploadID
	if uploadID == "" {
		uploadID = uuid.NewString()
	}
	buf := make([]byte, SessionPDFChunkBytes)
	var size int64
	part := 0
	for {
		n, err := io.ReadFull(source, buf)
		if n > 0 {
			size += int64(n)
			if size > SessionPDFMaxBytes {
				return nil, fmt.Errorf("mobius: PDF uploads are limited to 100 MiB")
			}
			if perr := c.PutSessionPDFUploadPart(ctx, sessionID, uploadID, part, buf[:n]); perr != nil {
				return nil, perr
			}
			part++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("mobius: read PDF: %w", err)
		}
	}
	if size == 0 {
		return nil, fmt.Errorf("mobius: PDF must not be empty")
	}
	return c.CompleteSessionPDFUpload(ctx, sessionID, uploadID, CompleteSessionPDFUploadOptions{
		Name:           name,
		SizeBytes:      size,
		PartCount:      part,
		IdempotencyKey: opts.IdempotencyKey,
	})
}

// PutSessionPDFUploadPart stages one chunk of a large PDF. Prefer
// UploadSessionPDF. Every part but the last must be exactly
// SessionPDFChunkBytes; parts number from 0.
func (c *Client) PutSessionPDFUploadPart(ctx context.Context, sessionID, uploadID string, partNumber int, chunk []byte) error {
	id, err := uuid.Parse(uploadID)
	if err != nil {
		return fmt.Errorf("mobius: upload_id must be a UUID: %w", err)
	}
	resp, err := c.ac.PutSessionPDFUploadPartWithBodyWithResponse(ctx,
		api.SessionIdParam(sessionID), id, partNumber, "application/octet-stream", bytes.NewReader(chunk))
	if err != nil {
		return fmt.Errorf("mobius: put session PDF upload part: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedSessionStatus("put session PDF upload part", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return nil
}

// CompleteSessionPDFUploadOptions describes the staged parts to commit.
type CompleteSessionPDFUploadOptions struct {
	// Name is the display filename recorded for the PDF.
	Name string
	// SizeBytes is the total length across all staged parts.
	SizeBytes int64
	// PartCount is the number of staged parts, numbered from 0.
	PartCount int
	// IdempotencyKey is a retry key scoped to this session and caller (255
	// chars max). An identical retry returns the original attachment.
	IdempotencyKey string
}

// CompleteSessionPDFUpload validates the staged chunks as one PDF, commits it,
// and binds it to the session. Prefer UploadSessionPDF, which stages and
// completes in one call.
func (c *Client) CompleteSessionPDFUpload(ctx context.Context, sessionID, uploadID string, opts CompleteSessionPDFUploadOptions) (*api.SessionAttachmentResponse, error) {
	id, err := uuid.Parse(uploadID)
	if err != nil {
		return nil, fmt.Errorf("mobius: upload_id must be a UUID: %w", err)
	}
	if len(opts.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("mobius: attachment IdempotencyKey must be at most 255 characters")
	}
	resp, err := c.ac.CompleteSessionPDFUploadWithResponse(ctx, api.SessionIdParam(sessionID), id,
		&api.CompleteSessionPDFUploadParams{IdempotencyKey: stringPointer(opts.IdempotencyKey)},
		api.CompleteSessionPDFUploadJSONRequestBody{
			Name:      opts.Name,
			SizeBytes: opts.SizeBytes,
			PartCount: opts.PartCount,
		})
	if err != nil {
		return nil, fmt.Errorf("mobius: complete session PDF upload: %w", err)
	}
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	return nil, unexpectedSessionStatus("complete session PDF upload", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
}
