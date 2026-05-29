package mobius

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateArtifactRefFromFileWithLeaseSendsHeaderAndJSONTags(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "result.txt")
	if err := os.WriteFile(path, []byte("artifact bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got artifactUploadCapture
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureArtifactUpload(t, r)
		if got.leaseToken != "lease-token" {
			t.Fatalf("lease header = %q", got.leaseToken)
		}
		if got.fields["run_id"] != "" || got.fields["step_id"] != "" || got.fields["visibility"] != "" {
			t.Fatalf("lease upload should not send caller lineage fields: %#v", got.fields)
		}
		_, _ = w.Write([]byte(`{"id":"art_1","name":"result.txt","mime_type":"text/plain","size_bytes":14,"sha256":"abc123","state":"available","visibility":"shared","created_at":"2026-05-29T00:00:00Z"}`))
	}))

	ref, err := c.CreateArtifactRefFromFileWithLease(context.Background(), path, "", "text/plain", "lease-token", map[string]string{"source": "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Type != "artifact" || ref.ID != "art_1" || ref.Mime != "text/plain" || ref.Size != 14 || ref.SHA256 != "abc123" {
		t.Fatalf("ref = %#v", ref)
	}
	if got.fileBody != "artifact bytes" {
		t.Fatalf("file body = %q", got.fileBody)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(got.fields["tags"]), &tags); err != nil {
		t.Fatalf("tags were not JSON: %q", got.fields["tags"])
	}
	if tags["source"] != "worker" {
		t.Fatalf("tags = %#v", tags)
	}
}

func TestCreateArtifactFromFileSendsSharedVisibilityAndJSONTags(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "manual.txt")
	if err := os.WriteFile(path, []byte("manual bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got artifactUploadCapture
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureArtifactUpload(t, r)
		if got.leaseToken != "" {
			t.Fatalf("ordinary upload should not send lease header")
		}
		_, _ = w.Write([]byte(`{"id":"art_2","name":"manual.txt","mime_type":"text/plain","size_bytes":12,"sha256":"def456","state":"available","visibility":"shared","created_at":"2026-05-29T00:00:00Z"}`))
	}))

	artifact, err := c.CreateArtifactFromFile(context.Background(), path, "", "text/plain", "run_1", "step_1", map[string]string{"source": "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "art_2" || artifact.MimeType != "text/plain" || artifact.SHA256 != "def456" {
		t.Fatalf("artifact = %#v", artifact)
	}
	if got.fields["run_id"] != "run_1" || got.fields["step_id"] != "step_1" || got.fields["visibility"] != "shared" {
		t.Fatalf("ordinary upload lineage fields = %#v", got.fields)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(got.fields["tags"]), &tags); err != nil {
		t.Fatalf("tags were not JSON: %q", got.fields["tags"])
	}
	if tags["source"] != "manual" {
		t.Fatalf("tags = %#v", tags)
	}
}

type artifactUploadCapture struct {
	leaseToken string
	fields     map[string]string
	fileBody   string
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
		leaseToken: r.Header.Get("X-Mobius-Lease-Token"),
		fields:     map[string]string{},
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", part.FormName(), err)
		}
		if part.FormName() == "file" {
			got.fileBody = string(data)
			continue
		}
		got.fields[part.FormName()] = string(data)
	}
	return got
}
