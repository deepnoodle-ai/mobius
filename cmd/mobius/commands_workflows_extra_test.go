package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

// TestWorkflowsCreateDryRunFromYAMLFile exercises the `--dry-run` short
// circuit: a YAML file is read, parsed, and printed without any HTTP call.
// Catches regressions in YAML detection, YAML→JSON normalization (oneOf
// step), and the dry-run gate.
func TestWorkflowsCreateDryRunFromYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.yaml")
	yaml := `
name: greeter
spec:
  name: greeter
  steps:
    - name: hello
      action: print
      action_kind: server
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	result := newApp().Test(t,
		cli.TestArgs("workflows", "create", "--api-key", "mbx_test", "--dry-run", "-f", path),
	)
	if !result.Success() {
		t.Fatalf("dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &body); err != nil {
		t.Fatalf("dry-run output is not valid JSON: %v\n%s", err, result.Stdout)
	}
	spec, _ := body["spec"].(map[string]any)
	steps, _ := spec["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d: %s", len(steps), result.Stdout)
	}
	step, _ := steps[0].(map[string]any)
	if step["action"] != "print" || step["action_kind"] != "server" {
		t.Fatalf("step missing fields: %+v", step)
	}
}

// TestWorkflowsCreateRepeatableTagFlag verifies --tag KEY=VALUE replaces the
// old JSON --tags blob.
func TestWorkflowsCreateRepeatableTagFlag(t *testing.T) {
	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "create", "--api-key", "mbx_test", "--dry-run",
			"--name", "wf",
			"--spec", `{"name":"wf","steps":[{"name":"a","action":"print"}]}`,
			"--tag", "env=prod",
			"--tag", "team=infra",
		),
	)
	if !result.Success() {
		t.Fatalf("create dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &body); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, result.Stdout)
	}
	tags, ok := body["tags"].(map[string]any)
	if !ok {
		t.Fatalf("tags missing or wrong type: %+v", body["tags"])
	}
	if tags["env"] != "prod" || tags["team"] != "infra" {
		t.Fatalf("unexpected tags: %+v", tags)
	}
}

// TestWorkflowsCreateAtFileSpecFlag covers @path resolution on a JSON-typed
// flag (the --spec field). The file uses YAML to also exercise format
// auto-detection on the @path side.
func TestWorkflowsCreateAtFileSpecFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	body := `
name: from-file
steps:
  - name: a
    action: print
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "create", "--api-key", "mbx_test", "--dry-run",
			"--name", "wf",
			"--spec", "@"+path,
		),
	)
	if !result.Success() {
		t.Fatalf("create dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, `"name": "from-file"`) {
		t.Fatalf("spec name not propagated from @file: %s", result.Stdout)
	}
}

// TestWorkflowsCreateVarSubstitution checks that --var KEY=VAL replaces
// ${KEY} placeholders in the file, and unspecified placeholders pass through
// unchanged.
func TestWorkflowsCreateVarSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.yaml")
	yaml := `
name: ${ENV}-greeter
spec:
  name: ${ENV}-greeter
  steps:
    - name: hello
      action: print
      parameters:
        message: "${UNKNOWN_LEAVE_ME}"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "create", "--api-key", "mbx_test", "--dry-run",
			"-f", path, "--var", "ENV=prod",
		),
	)
	if !result.Success() {
		t.Fatalf("dry-run failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, `"name": "prod-greeter"`) {
		t.Fatalf("ENV not substituted: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "${UNKNOWN_LEAVE_ME}") {
		t.Fatalf("unknown placeholder was substituted (should pass through): %s", result.Stdout)
	}
}

// TestWorkflowsCreateTextID covers `-o text -F id` on a single-resource
// response — the pipe-friendly "just the id" pattern (use with xargs).
func TestWorkflowsCreateTextID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"wf_abc","handle":"wf","name":"wf"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "create",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--name", "wf",
			"--spec", `{"name":"wf","steps":[{"name":"a","action":"print"}]}`,
			"-o", "text",
			"-F", "id",
		),
	)
	if !result.Success() {
		t.Fatalf("create failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := strings.TrimSpace(result.Stdout); got != "wf_abc" {
		t.Fatalf("-o text -F id: got %q, want %q", got, "wf_abc")
	}
}

// TestWorkflowsListTextWithFields exercises -o text with --fields against a
// paginated list. Text format is the pipe-friendly tab-separated mode.
func TestWorkflowsListTextWithFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
            {"id":"wf_1","handle":"a","name":"Alpha","latest_version":3},
            {"id":"wf_2","handle":"b","name":"Beta","latest_version":7}
        ],"has_more":false}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "list",
			"--api-url", srv.URL,
			"--api-key", "mbx",
			"-o", "text",
			"-F", "handle,id,latest_version",
		),
	)
	if !result.Success() {
		t.Fatalf("workflows list failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	// Columns sorted alphabetically in text mode (deterministic).
	want := "a\twf_1\t3\nb\twf_2\t7\n"
	if result.Stdout != want {
		t.Fatalf("output mismatch:\ngot:  %q\nwant: %q", result.Stdout, want)
	}
}

// TestFieldsTypoIsRejected verifies the typo-protection contract: a field
// not present in the response errors out with the available list.
func TestFieldsTypoIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"wf_1","handle":"a","name":"Alpha","latest_version":3}],"has_more":false}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "list",
			"--api-url", srv.URL,
			"--api-key", "mbx",
			"-F", "id,latest_versionz", // typo
		),
	)
	if result.Success() {
		t.Fatalf("expected failure for unknown field, got success: %s", result.Stdout)
	}
	msg := ""
	if result.Err != nil {
		msg = result.Err.Error()
	}
	if msg == "" {
		msg = result.Stderr
	}
	if !strings.Contains(msg, "unknown field") || !strings.Contains(msg, "latest_versionz") {
		t.Fatalf("error should name the unknown field. err:\n%s\nstderr:\n%s", msg, result.Stderr)
	}
	if !strings.Contains(msg, "available:") || !strings.Contains(msg, "latest_version") {
		t.Fatalf("error should list available fields. err:\n%s", msg)
	}
}

// TestFieldsProjectsJSONEnvelope confirms json + --fields preserves the
// list envelope (has_more, next_cursor) while projecting items[].
func TestFieldsProjectsJSONEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"wf_1","handle":"a","name":"Alpha"},{"id":"wf_2","handle":"b","name":"Beta"}],"has_more":true,"next_cursor":"abc"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "list",
			"--api-url", srv.URL,
			"--api-key", "mbx",
			"-o", "json",
			"-F", "id,name",
		),
	)
	if !result.Success() {
		t.Fatalf("list failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, `"has_more": true`) {
		t.Fatalf("envelope dropped: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, `"next_cursor": "abc"`) {
		t.Fatalf("cursor dropped: %s", result.Stdout)
	}
	if strings.Contains(result.Stdout, "handle") {
		t.Fatalf("handle should be projected out: %s", result.Stdout)
	}
}

// TestWorkflowsCreateExitCodeOnHTTP400 verifies validation errors from the
// server map to exit code 5 (4xx).
func TestWorkflowsCreateExitCodeOnHTTP400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"nope"}}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "create",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--name", "wf",
			"--spec", `{"name":"wf","steps":[{"name":"a","action":"print"}]}`,
		),
	)
	if result.ExitCode != 5 {
		t.Fatalf("exit code: got %d, want 5 (4xx); err: %v\nstderr: %s", result.ExitCode, result.Err, result.Stderr)
	}
}

// TestWorkflowsApplyCreatesWhenHandleNotFound walks the empty list, then
// posts to create.
func TestWorkflowsApplyCreatesWhenHandleNotFound(t *testing.T) {
	var posted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/workflows"):
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"handle":"new-wf"`) {
				t.Errorf("expected handle in body, got %s", body)
			}
			posted.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"wf_new","handle":"new-wf","name":"new-wf"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "apply",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--name", "new-wf",
			"--handle", "new-wf",
			"--spec", `{"name":"new-wf","steps":[{"name":"a","action":"print"}]}`,
			"-o", "text",
			"-F", "id",
		),
	)
	if !result.Success() {
		t.Fatalf("apply failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if posted.Load() != 1 {
		t.Fatalf("expected 1 POST, got %d", posted.Load())
	}
	if got := strings.TrimSpace(result.Stdout); got != "wf_new" {
		t.Fatalf("text -F id: got %q, want %q", got, "wf_new")
	}
}

// TestWorkflowsApplyUpdatesWhenHandleFound finds an existing workflow on the
// list, then PATCHes it.
func TestWorkflowsApplyUpdatesWhenHandleFound(t *testing.T) {
	var patched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"id":"wf_1","handle":"existing","name":"existing","latest_version":1,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","created_by":"user_1"}],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/workflows/wf_1"):
			patched.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"wf_1","handle":"existing","name":"existing-updated"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "apply",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--name", "existing-updated",
			"--handle", "existing",
			"--spec", `{"name":"existing","steps":[{"name":"a","action":"print"}]}`,
			"-o", "text",
			"-F", "id,name",
		),
	)
	if !result.Success() {
		t.Fatalf("apply failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if patched.Load() != 1 {
		t.Fatalf("expected 1 PATCH, got %d", patched.Load())
	}
	if got := strings.TrimSpace(result.Stdout); got != "wf_1\texisting-updated" {
		t.Fatalf("text id,name: got %q, want %q", got, "wf_1\texisting-updated")
	}
}

// TestWorkflowsInitYAMLScaffoldRoundTrips ensures `init`'s YAML output parses
// back through `create --dry-run`, catching shape/indentation regressions.
func TestWorkflowsInitYAMLScaffoldRoundTrips(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("workflows", "init", "--name", "rt"))
	if !result.Success() {
		t.Fatalf("init failed: %v", result.Err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rt.yaml")
	if err := os.WriteFile(path, []byte(result.Stdout), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dryRun := newApp().Test(t,
		cli.TestArgs("workflows", "create", "--api-key", "mbx_test", "--dry-run", "-f", path),
	)
	if !dryRun.Success() {
		t.Fatalf("scaffold round-trip failed: %v\nstderr: %s", dryRun.Err, dryRun.Stderr)
	}
	if !strings.Contains(dryRun.Stdout, `"name": "rt"`) {
		t.Fatalf("name not preserved through round trip: %s", dryRun.Stdout)
	}
}
