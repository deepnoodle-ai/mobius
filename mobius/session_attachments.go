package mobius

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
	if (opts.Path == "") == (opts.Reader == nil) {
		return nil, fmt.Errorf("mobius: exactly one of Path or Reader is required")
	}
	upload := artifactUpload{
		name:           strings.TrimSpace(opts.Name),
		mime:           opts.Mime,
		sizeBytes:      opts.SizeBytes,
		source:         opts.Reader,
		idempotencyKey: opts.IdempotencyKey,
	}
	if opts.Path != "" {
		file, err := os.Open(opts.Path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		upload.source = file
		upload.fileName = filepath.Base(opts.Path)
		if upload.name == "" {
			upload.name = upload.fileName
		}
		if upload.sizeBytes == 0 {
			upload.sizeBytes = info.Size()
		}
	}
	if upload.name == "" {
		return nil, fmt.Errorf("mobius: attachment Name is required")
	}
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
