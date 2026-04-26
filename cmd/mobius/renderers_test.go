package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

const sampleRun = `{
  "id": "run_abc",
  "workflow_name": "greeter",
  "status": "running",
  "attempt": 1,
  "queue": "default",
  "ephemeral": false,
  "created_at": "2025-01-01T00:00:00Z",
  "updated_at": "2025-01-01T00:00:01Z",
  "errors": [],
  "path_counts": {"total":3,"working":1,"waiting":1,"completed":1,"failed":0,"active":2},
  "paths": [
    {"path_id":"main","state":"working"},
    {"path_id":"main/a","state":"waiting","waiting_on":{"kind":"interaction","reason":"approval"}},
    {"path_id":"main/b","state":"completed"}
  ],
  "wait_summary": {"total":1}
}`

func newRunGetServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/runs/run_abc") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleRun))
	}))
}

// TestGetRunRendererFiresOnPretty exercises the registered renderer for the
// getRun operationId. Forces --output pretty so the test runner (where
// stdout isn't a TTY) still routes through the custom renderer.
func TestGetRunRendererFiresOnPretty(t *testing.T) {
	srv := newRunGetServer(t)
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"runs", "get", "run_abc",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "pretty",
		),
	)
	if !result.Success() {
		t.Fatalf("runs get failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	mustContain(t, result.Stdout, "greeter")
	mustContain(t, result.Stdout, "run_abc")
	// Status row picks up our visual cue glyphs.
	mustContain(t, result.Stdout, "running")
	// Path table shows path ids and waiting reason.
	mustContain(t, result.Stdout, "main/a")
	mustContain(t, result.Stdout, "interaction: approval")
	// Path counts summary line.
	mustContain(t, result.Stdout, "3 total")
}

// TestGetRunJSONOutputBypassesRenderer verifies machine-parseable modes
// always emit canonical JSON regardless of the registered renderer — that's
// the contract that lets scripts depend on --output json shape.
func TestGetRunJSONOutputBypassesRenderer(t *testing.T) {
	srv := newRunGetServer(t)
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"runs", "get", "run_abc",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "json",
		),
	)
	if !result.Success() {
		t.Fatalf("runs get failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	out := strings.TrimSpace(result.Stdout)
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("expected JSON object, got: %s", out)
	}
	// Should not contain our visual cue glyphs.
	if strings.Contains(out, "⟳") || strings.Contains(out, "✓") {
		t.Fatalf("--output json leaked pretty glyphs: %s", out)
	}
}

// TestRegisterResponseRendererTakesPrecedence ensures a renderer registered
// at runtime wins over the generic pretty path for that operationId only.
func TestRegisterResponseRendererTakesPrecedence(t *testing.T) {
	called := false
	prev := responseRenderers["getWorkflow"]
	RegisterResponseRenderer("getWorkflow", func(ctx *cli.Context, body []byte) error {
		called = true
		ctx.Println("CUSTOM RENDERER OUTPUT")
		return nil
	})
	t.Cleanup(func() {
		if prev == nil {
			delete(responseRenderers, "getWorkflow")
		} else {
			responseRenderers["getWorkflow"] = prev
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"wf_1","handle":"x","name":"x","latest_version":1,"spec":{"name":"x","steps":[]},"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","created_by":"u"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"workflows", "get", "wf_1",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "pretty",
		),
	)
	if !result.Success() {
		t.Fatalf("workflows get failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !called {
		t.Fatalf("custom renderer was not invoked")
	}
	mustContain(t, result.Stdout, "CUSTOM RENDERER OUTPUT")
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q:\n%s", needle, haystack)
	}
}
