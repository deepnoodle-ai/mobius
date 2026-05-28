package mobius

import (
	"bytes"
	"context"
	"encoding/json"
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

type EnvironmentGitCredentialRequest struct {
	RepoFullName string `json:"repo_full_name"`
	Operation    string `json:"operation,omitempty"`
}

type EnvironmentGitCredential struct {
	Host         string    `json:"host"`
	Username     string    `json:"username"`
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RepoFullName string    `json:"repo_full_name"`
}

type Artifact struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Mime        string    `json:"mime"`
	SizeBytes   int64     `json:"size_bytes"`
	State       string    `json:"state"`
	Visibility  string    `json:"visibility"`
	DownloadURL string    `json:"download_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type ArtifactDownload struct {
	ArtifactID    string
	Path          string
	BytesWritten  int64
	ContentType   string
	ContentLength int64
}

func (c *Client) CreateEnvironmentGitCredential(ctx context.Context, environmentID string, req EnvironmentGitCredentialRequest) (*EnvironmentGitCredential, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(environmentID) == "" {
		return nil, fmt.Errorf("mobius: environment_id is required")
	}
	if strings.TrimSpace(req.RepoFullName) == "" {
		return nil, fmt.Errorf("mobius: repo_full_name is required")
	}
	var out EnvironmentGitCredential
	if err := c.doJSON(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(c.projectHandle)+"/environments/"+url.PathEscape(environmentID)+"/git/credentials", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateArtifactFromFile(ctx context.Context, path, name, mime, runID, stepID string, tags map[string]string) (*Artifact, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("mobius: path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, err
	}
	_ = writer.WriteField("name", name)
	if mime != "" {
		_ = writer.WriteField("mime", mime)
	}
	if runID != "" {
		_ = writer.WriteField("run_id", runID)
	}
	if stepID != "" {
		_ = writer.WriteField("step_id", stepID)
	}
	_ = writer.WriteField("visibility", "shared")
	_ = writer.WriteField("size_bytes", strconv.FormatInt(info.Size(), 10))
	for key, value := range tags {
		_ = writer.WriteField("tags["+key+"]", value)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	var out Artifact
	if err := c.doMultipart(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(c.projectHandle)+"/artifacts", writer.FormDataContentType(), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, "application/json", bytes.NewReader(raw), out)
}

func (c *Client) doMultipart(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	return c.do(ctx, method, path, contentType, body, out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	if c.projectHandle == "" {
		return fmt.Errorf("mobius: no project configured - set MOBIUS_PROJECT or pass --project")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mobius: %s %s failed: %s: %s", method, path, resp.Status, strings.TrimSpace(string(payload)))
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("mobius: decode response: %w", err)
	}
	return nil
}
