package mobius

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

// newTestContext builds an executionContext bound to client/job for
// directly exercising worker-side helpers without spinning up a Worker.
func newTestContext(client *Client, job *runtimeJob) Context {
	return newContext(context.Background(), client, job, slog.Default(), nil)
}

func TestContext_ProjectHandle_AndDeprecatedProjectIDAlias(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx := newTestContext(c, &runtimeJob{
		JobID:         "job_1",
		RunID:         "run_1",
		ProjectHandle: "test-project",
	})
	// New name returns the handle.
	assert.Equal(t, ctx.ProjectHandle(), "test-project")
	// Deprecated alias returns the same value, not a TypeID.
	assert.Equal(t, ctx.ProjectID(), "test-project")
}

func TestClient_ProjectHandleAccessor(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	assert.Equal(t, c.ProjectHandle(), "test-project")
}

func TestContext_RunServerAction_PostsToJobActionsRoute(t *testing.T) {
	var sentBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/jobs/job_1/actions/render-template")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sentBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"output":{"text":"hello"}}`)
	})
	c, _ := newTestClient(t, h)
	ctx := newTestContext(c, &runtimeJob{
		JobID:         "job_1",
		RunID:         "run_1",
		ProjectHandle: "test-project",
		StepName:      "render",
	})

	out, err := ctx.RunServerAction("render-template", map[string]any{"template": "hi"}, nil)
	assert.NoError(t, err)
	m, ok := out.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, m["text"], "hello")

	params, ok := sentBody["parameters"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, params["template"], "hi")
}

func TestContext_RunServerAction_OptsForwardDryRunAndTimeout(t *testing.T) {
	var sentBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sentBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"output":42}`)
	})
	c, _ := newTestClient(t, h)
	ctx := newTestContext(c, &runtimeJob{JobID: "job_1", ProjectHandle: "test-project"})

	out, err := ctx.RunServerAction("noop", nil, &RunServerActionOptions{DryRun: true, TimeoutSeconds: 30})
	assert.NoError(t, err)
	// JSON numbers decode to float64; the union helper returns the
	// matching primitive.
	switch v := out.(type) {
	case float32:
		assert.Equal(t, v, float32(42))
	case int:
		assert.Equal(t, v, 42)
	case float64:
		assert.Equal(t, v, float64(42))
	default:
		t.Fatalf("unexpected output type: %T", out)
	}
	assert.Equal(t, sentBody["dry_run"], true)
	assert.Equal(t, sentBody["timeout_seconds"], float64(30))
}

func TestContext_RunServerAction_RequiresActionName(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request")
	}))
	ctx := newTestContext(c, &runtimeJob{JobID: "job_1", ProjectHandle: "test-project"})
	_, err := ctx.RunServerAction("", nil, nil)
	assert.True(t, err != nil)
}

func TestContext_RequestInteraction_BuildsJobScopedRequest(t *testing.T) {
	var sentBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Method, http.MethodPost)
		assert.Equal(t, r.URL.Path, "/v1/projects/test-project/jobs/job_1/interactions")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sentBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
            "id":"int_1",
            "status":"pending",
            "type":"review",
            "target":{"type":"user","id":"usr_1"},
            "created_at":"2026-04-29T00:00:00Z",
            "updated_at":"2026-04-29T00:00:00Z"
        }`)
	})
	c, _ := newTestClient(t, h)
	ctx := newTestContext(c, &runtimeJob{
		JobID:         "job_1",
		ProjectHandle: "test-project",
		StepName:      "review-step",
	})

	got, err := ctx.RequestInteraction(InteractionRequest{
		Target:  InteractionTarget{Type: InteractionTargetTypeUser, ID: "usr_1"},
		Kind:    InteractionKindReview,
		Message: "Please ack",
		Timeout: "15m",
	})
	assert.NoError(t, err)
	assert.Equal(t, got.Id, "int_1")
	assert.Equal(t, string(got.Type), "review")
	// Step name is auto-threaded so the server can derive a default
	// signal name without the worker passing it explicitly.
	assert.Equal(t, sentBody["step_name"], "review-step")
	assert.Equal(t, sentBody["timeout"], "15m")
	target, _ := sentBody["target"].(map[string]any)
	assert.Equal(t, target["type"], "user")
	assert.Equal(t, target["id"], "usr_1")
}

func TestContext_RequestInteraction_ValidatesRequiredFields(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request")
	}))
	ctx := newTestContext(c, &runtimeJob{JobID: "job_1", ProjectHandle: "test-project"})

	cases := []InteractionRequest{
		{}, // all empty
		{Kind: InteractionKindReview, Message: "x", Target: InteractionTarget{Type: InteractionTargetTypeUser}},
		{Kind: InteractionKindReview, Message: "x", Target: InteractionTarget{ID: "usr_1"}},
		{Kind: InteractionKindReview, Target: InteractionTarget{Type: InteractionTargetTypeUser, ID: "usr_1"}},
		{Message: "x", Target: InteractionTarget{Type: InteractionTargetTypeUser, ID: "usr_1"}},
	}
	for i, req := range cases {
		_, err := ctx.RequestInteraction(req)
		assert.True(t, err != nil)
		_ = i
	}
}

// Confirms ProjectHandle accessor matches what the runtime claim
// populates from the bound client's handle.
func TestContext_ProjectHandle_MatchesRuntimeJobField(t *testing.T) {
	job := &runtimeJob{ProjectHandle: "my-project"}
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx := newContext(context.Background(), c, job, slog.Default(), nil)
	assert.Equal(t, ctx.ProjectHandle(), "my-project")
	// Sanity: Context still satisfies context.Context.
	var _ context.Context = ctx
	// And the api package reference is a real one (compile-time check).
	_ = api.ProjectHandleParam("")
}
