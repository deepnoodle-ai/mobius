package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Artifact struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Mime        string         `json:"mime,omitempty"`
	MimeType    string         `json:"mime_type,omitempty"`
	SizeBytes   int64          `json:"size_bytes"`
	SHA256      string         `json:"sha256,omitempty"`
	State       string         `json:"state"`
	Visibility  string         `json:"visibility"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	DownloadURL string         `json:"download_url,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

type ArtifactRef struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func NewArtifactRef(a *Artifact) ArtifactRef {
	if a == nil {
		return ArtifactRef{}
	}
	mime := a.MimeType
	if mime == "" {
		mime = a.Mime
	}
	return ArtifactRef{
		Type:   "artifact",
		ID:     a.ID,
		Name:   a.Name,
		Mime:   mime,
		Size:   a.SizeBytes,
		SHA256: a.SHA256,
	}
}

type ArtifactDownload struct {
	ArtifactID    string
	Path          string
	BytesWritten  int64
	ContentType   string
	ContentLength int64
}

// CreateArtifactOptions configures a project-authorized artifact upload.
// Exactly one of Path or Reader must supply the artifact bytes.
type CreateArtifactOptions struct {
	// Path streams the artifact from a file without buffering it in memory.
	Path string
	// Reader streams the artifact from an arbitrary source. Name is required
	// with Reader.
	Reader io.Reader
	// Name is the display name or relative virtual path (forward slashes
	// organize artifacts, e.g. "renders/report.html"). Defaults to the base
	// name of Path.
	Name string
	// Mime optionally overrides the MIME type recorded for the content.
	Mime string
	// SizeBytes declares the exact content length so the server can verify the
	// streamed byte count. Derived from the file size when uploading from Path.
	SizeBytes int64
	// Metadata is an optional caller metadata object, sent as a JSON multipart
	// field (64 KiB limit).
	Metadata map[string]any
	// IdempotencyKey is a durable retry key for this one artifact (255 chars max).
	IdempotencyKey string
}

// CreateArtifact uploads a private, principal-owned artifact using the
// client's project authorization. Ownership and visibility are derived from
// the authenticated principal; run/step lineage can only be attached by the
// server from a worker lease (see [Client.CreateArtifactRefFromFileWithLease]).
func (c *Client) CreateArtifact(ctx context.Context, opts CreateArtifactOptions) (*Artifact, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if len(opts.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("mobius: artifact IdempotencyKey must be at most 255 characters")
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
	if opts.Metadata != nil {
		raw, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, fmt.Errorf("mobius: encode artifact metadata: %w", err)
		}
		upload.metadataJSON = raw
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
		return nil, fmt.Errorf("mobius: artifact Name is required")
	}
	return c.uploadArtifact(ctx, upload)
}

// CreateArtifactRefFromFileWithLease uploads an artifact under a worker lease
// and returns its reference. The server derives run, step, job, attempt, and
// shared visibility from the lease; the metadata map is recorded as caller
// metadata on the artifact.
func (c *Client) CreateArtifactRefFromFileWithLease(ctx context.Context, path, name, mime, leaseToken string, metadata map[string]string) (ArtifactRef, error) {
	if c == nil {
		return ArtifactRef{}, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(leaseToken) == "" {
		return ArtifactRef{}, fmt.Errorf("mobius: lease token is required")
	}
	if strings.TrimSpace(path) == "" {
		return ArtifactRef{}, fmt.Errorf("mobius: path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return ArtifactRef{}, err
	}
	upload := artifactUpload{
		name:       strings.TrimSpace(name),
		fileName:   filepath.Base(path),
		mime:       mime,
		sizeBytes:  info.Size(),
		leaseToken: strings.TrimSpace(leaseToken),
		source:     file,
	}
	if upload.name == "" {
		upload.name = upload.fileName
	}
	if len(metadata) > 0 {
		raw, err := json.Marshal(metadata)
		if err != nil {
			return ArtifactRef{}, fmt.Errorf("mobius: encode artifact metadata: %w", err)
		}
		upload.metadataJSON = raw
	}
	artifact, err := c.uploadArtifact(ctx, upload)
	if err != nil {
		return ArtifactRef{}, err
	}
	return NewArtifactRef(artifact), nil
}

// ErrCreateArtifactFromFileRemoved is returned by the deprecated
// [Client.CreateArtifactFromFile]: the API no longer accepts caller-supplied
// run/step lineage or visibility, so the method cannot honor its signature.
var ErrCreateArtifactFromFileRemoved = errors.New(
	"mobius: CreateArtifactFromFile is no longer supported: the API derives artifact lineage and visibility from a worker lease and rejects caller-supplied run_id, step_id, and visibility; use CreateArtifact for a project-authorized private upload, or CreateArtifactRefFromFileWithLease under a worker lease",
)

// CreateArtifactFromFile is unsupported and returns
// [ErrCreateArtifactFromFileRemoved] without making a request.
//
// Deprecated: the v0.0.53 artifact contract rejects caller-supplied lineage
// and visibility, which this signature promises. Use [Client.CreateArtifact]
// or [Client.CreateArtifactRefFromFileWithLease].
func (c *Client) CreateArtifactFromFile(ctx context.Context, path, name, mime, runID, stepID string, tags map[string]string) (*Artifact, error) {
	return nil, ErrCreateArtifactFromFileRemoved
}

// artifactUpload is the wire-level description of one multipart upload. Only
// fields permitted by the CreateArtifactRequest contract are represented:
// lineage and visibility are always server-derived.
type artifactUpload struct {
	name           string
	fileName       string
	mime           string
	sizeBytes      int64
	metadataJSON   []byte
	leaseToken     string
	idempotencyKey string
	source         io.Reader
}

func (c *Client) uploadArtifact(ctx context.Context, upload artifactUpload) (*Artifact, error) {
	if upload.fileName == "" {
		upload.fileName = filepath.Base(upload.name)
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	contentType := multipartWriter.FormDataContentType()
	writeErr := make(chan error, 1)
	go func() {
		err := writeArtifactMultipart(multipartWriter, upload)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		writeErr <- err
	}()

	headers := map[string]string{}
	if upload.leaseToken != "" {
		headers["X-Mobius-Lease-Token"] = upload.leaseToken
	}
	if upload.idempotencyKey != "" {
		headers["Idempotency-Key"] = upload.idempotencyKey
	}
	var out Artifact
	err := c.doMultipartWithHeaders(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(c.projectHandle)+"/artifacts", contentType, reader, headers, &out)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeErr
		return nil, err
	}
	if werr := <-writeErr; werr != nil {
		return nil, werr
	}
	return &out, nil
}

// writeArtifactMultipart writes the metadata fields before the file part so
// the server can stream the bytes to storage without spooling them first.
func writeArtifactMultipart(writer *multipart.Writer, upload artifactUpload) error {
	_ = writer.WriteField("name", upload.name)
	if upload.mime != "" {
		_ = writer.WriteField("mime", upload.mime)
	}
	if upload.sizeBytes > 0 {
		_ = writer.WriteField("size_bytes", strconv.FormatInt(upload.sizeBytes, 10))
	}
	if len(upload.metadataJSON) > 0 {
		_ = writer.WriteField("metadata", string(upload.metadataJSON))
	}
	part, err := writer.CreateFormFile("file", upload.fileName)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, upload.source); err != nil {
		return err
	}
	return writer.Close()
}

func (c *Client) DownloadArtifactToFile(ctx context.Context, artifactID, path string, maxBytes int64) (*ArtifactDownload, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(artifactID) == "" {
		return nil, fmt.Errorf("mobius: artifact_id is required")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("mobius: path is required")
	}
	if c.projectHandle == "" {
		return nil, fmt.Errorf("mobius: no project configured - set MOBIUS_PROJECT or pass --project")
	}
	if maxBytes <= 0 {
		maxBytes = 100 * 1024 * 1024
	}
	reqPath := "/v1/projects/" + url.PathEscape(c.projectHandle) + "/artifacts/" + url.PathEscape(artifactID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.baseURL, "/")+reqPath, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	// Artifact bodies can take longer than the general-purpose client's 60s
	// whole-exchange timeout; the transfer client bounds only the phases that
	// can hang without progress.
	resp, err := c.transferClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mobius: GET %s failed: %s: %s", reqPath, resp.Status, strings.TrimSpace(string(payload)))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return nil, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return nil, closeErr
	}
	if written > maxBytes {
		_ = os.Remove(path)
		return nil, fmt.Errorf("mobius: artifact %s exceeded max_bytes %d", artifactID, maxBytes)
	}
	return &ArtifactDownload{
		ArtifactID:    artifactID,
		Path:          path,
		BytesWritten:  written,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
	}, nil
}
