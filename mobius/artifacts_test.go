package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateArtifactFromPathSendsOnlyContractFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "report.html")
	if err := os.WriteFile(path, []byte("<h1>report</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got artifactUploadCapture
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureArtifactUpload(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"art_1","name":"renders/report.html","mime_type":"text/html","size_bytes":15,"visibility":"private","metadata":{"renderer":"omni"},"created_at":"2026-07-17T00:00:00Z"}`))
	}))

	artifact, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Path:           path,
		Name:           "renders/report.html",
		Mime:           "text/html",
		Metadata:       map[string]any{"renderer": "omni"},
		IdempotencyKey: "delivery-1:report",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "art_1" || artifact.Visibility != "private" {
		t.Fatalf("artifact = %#v", artifact)
	}
	if artifact.Metadata["renderer"] != "omni" {
		t.Fatalf("metadata = %#v", artifact.Metadata)
	}
	if got.fileBody != "<h1>report</h1>" {
		t.Fatalf("file body = %q", got.fileBody)
	}
	if got.leaseToken != "" {
		t.Fatalf("project-authorized upload must not send a lease header")
	}
	if got.idempotencyKey != "delivery-1:report" {
		t.Fatalf("idempotency key = %q", got.idempotencyKey)
	}
	want := map[string]string{
		"name":       "renders/report.html",
		"mime":       "text/html",
		"size_bytes": "15",
		"metadata":   `{"renderer":"omni"}`,
	}
	for field, value := range want {
		if got.fields[field] != value {
			t.Fatalf("field %s = %q, want %q", field, got.fields[field], value)
		}
	}
	for _, banned := range []string{"run_id", "step_id", "visibility", "tags"} {
		if _, ok := got.fields[banned]; ok {
			t.Fatalf("upload sent legacy field %q: %#v", banned, got.fields)
		}
	}
}

func TestCreateArtifactFromReaderStreamsWithoutSizeField(t *testing.T) {
	var got artifactUploadCapture
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureArtifactUpload(t, r)
		_, _ = w.Write([]byte(`{"id":"art_2","name":"notes.txt","mime_type":"text/plain","size_bytes":5,"visibility":"private","created_at":"2026-07-17T00:00:00Z"}`))
	}))

	artifact, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Reader: strings.NewReader("notes"),
		Name:   "notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "art_2" {
		t.Fatalf("artifact = %#v", artifact)
	}
	if got.fileBody != "notes" {
		t.Fatalf("file body = %q", got.fileBody)
	}
	if _, ok := got.fields["size_bytes"]; ok {
		t.Fatalf("reader upload without SizeBytes should omit size_bytes: %#v", got.fields)
	}
	if got.idempotencyKey != "" {
		t.Fatalf("idempotency header should be absent, got %q", got.idempotencyKey)
	}
}

func TestCreateArtifactValidatesOptions(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request expected")
	}))

	if _, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{Name: "x"}); err == nil {
		t.Fatal("expected error when no source is set")
	}
	if _, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Path: "a", Reader: strings.NewReader("b"), Name: "x",
	}); err == nil {
		t.Fatal("expected error when both sources are set")
	}
	if _, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Reader: strings.NewReader("b"),
	}); err == nil {
		t.Fatal("expected error when Reader has no Name")
	}
	if _, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Reader: strings.NewReader("b"), Name: "x", IdempotencyKey: strings.Repeat("k", 256),
	}); err == nil {
		t.Fatal("expected error for oversized idempotency key")
	}
}

func TestCreateArtifactSurfacesStructuredAPIError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"artifact_conflict","message":"artifact exists"}}`))
	}))

	_, err := c.CreateArtifact(context.Background(), CreateArtifactOptions{
		Reader: strings.NewReader("b"), Name: "x",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Code != "artifact_conflict" {
		t.Fatalf("apiErr = %#v", apiErr)
	}
}

func TestCreateArtifactRefFromFileWithLeaseSendsMetadataNotTags(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "result.txt")
	if err := os.WriteFile(path, []byte("artifact bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got artifactUploadCapture
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureArtifactUpload(t, r)
		_, _ = w.Write([]byte(`{"id":"art_3","name":"result.txt","mime_type":"text/plain","size_bytes":14,"sha256":"abc123","visibility":"shared","created_at":"2026-07-17T00:00:00Z"}`))
	}))

	ref, err := c.CreateArtifactRefFromFileWithLease(context.Background(), path, "", "text/plain", "lease-token", map[string]string{"source": "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Type != "artifact" || ref.ID != "art_3" || ref.Mime != "text/plain" || ref.Size != 14 || ref.SHA256 != "abc123" {
		t.Fatalf("ref = %#v", ref)
	}
	if got.leaseToken != "lease-token" {
		t.Fatalf("lease header = %q", got.leaseToken)
	}
	if got.fileBody != "artifact bytes" {
		t.Fatalf("file body = %q", got.fileBody)
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(got.fields["metadata"]), &metadata); err != nil {
		t.Fatalf("metadata was not JSON: %q", got.fields["metadata"])
	}
	if metadata["source"] != "worker" {
		t.Fatalf("metadata = %#v", metadata)
	}
	for _, banned := range []string{"run_id", "step_id", "visibility", "tags"} {
		if _, ok := got.fields[banned]; ok {
			t.Fatalf("lease upload sent caller-owned field %q: %#v", banned, got.fields)
		}
	}
}

func TestCreateArtifactFromFileReturnsMigrationError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("deprecated helper must not reach the API")
	}))

	_, err := c.CreateArtifactFromFile(context.Background(), "some/file", "", "", "run_1", "step_1", nil)
	if !errors.Is(err, ErrCreateArtifactFromFileRemoved) {
		t.Fatalf("err = %v", err)
	}
}

type artifactUploadCapture struct {
	leaseToken     string
	idempotencyKey string
	fields         map[string]string
	fileBody       string
	fileFirst      bool
}

func captureArtifactUpload(t *testing.T, r *http.Request) artifactUploadCapture {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/test-project/artifacts" {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		t.Fatalf("MultipartReader: %v", err)
	}
	got := artifactUploadCapture{
		leaseToken:     r.Header.Get("X-Mobius-Lease-Token"),
		idempotencyKey: r.Header.Get("Idempotency-Key"),
		fields:         map[string]string{},
	}
	first := true
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		var data bytes.Buffer
		_, err = io.Copy(&data, part)
		_ = part.Close()
		if err != nil {
			t.Fatalf("read part %s: %v", part.FormName(), err)
		}
		if part.FormName() == "file" {
			got.fileBody = data.String()
			got.fileFirst = first
		} else {
			got.fields[part.FormName()] = data.String()
		}
		first = false
	}
	return got
}
