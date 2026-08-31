package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

// TestRegisterResponseRendererTakesPrecedence ensures a renderer registered
// at runtime wins over the generic pretty path for that operationId only.
func TestRegisterResponseRendererTakesPrecedence(t *testing.T) {
	called := false
	prev := responseRenderers["getAgent"]
	RegisterResponseRenderer("getAgent", func(ctx *cli.Context, body []byte) error {
		called = true
		ctx.Println("CUSTOM RENDERER OUTPUT")
		return nil
	})
	t.Cleanup(func() {
		if prev == nil {
			delete(responseRenderers, "getAgent")
		} else {
			responseRenderers["getAgent"] = prev
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"agent_1","org_id":"org_1","name":"Scout","status":"active","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "get", "agent_1",
			"--api-url", srv.URL,
			"--api-key", "mbx_test",
			"--output", "pretty",
		),
	)
	if !result.Success() {
		t.Fatalf("agents get failed: %v\nstderr: %s", result.Err, result.Stderr)
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
