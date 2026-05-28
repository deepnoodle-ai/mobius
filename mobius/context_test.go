package mobius

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

// newTestContext builds an executionContext bound to a job for directly
// exercising worker-side helpers without spinning up a Worker.
func newTestContext(job *runtimeJob) Context {
	return newContext(context.Background(), nil, job, slog.Default(), nil)
}

func TestContext_ProjectHandle_AndDeprecatedProjectIDAlias(t *testing.T) {
	ctx := newTestContext(&runtimeJob{
		JobID:         "job_1",
		RunID:         "run_1",
		ProjectHandle: "test-project",
	})
	assert.Equal(t, ctx.ProjectHandle(), "test-project")
	assert.Equal(t, ctx.ProjectID(), "test-project")
}

func TestClient_ProjectHandleAccessor(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	assert.Equal(t, c.ProjectHandle(), "test-project")
}

func TestContext_JobIdentity(t *testing.T) {
	ctx := newTestContext(&runtimeJob{
		JobID:         "job_1",
		RunID:         "run_1",
		ProjectHandle: "test-project",
		StepID:        "step_1",
		Attempt:       3,
		Queue:         "default",
	})

	assert.Equal(t, ctx.JobID(), "job_1")
	assert.Equal(t, ctx.RunID(), "run_1")
	assert.Equal(t, ctx.StepName(), "step_1")
	assert.Equal(t, ctx.Attempt(), 3)
	assert.Equal(t, ctx.Queue(), "default")
	assert.Equal(t, ctx.WorkflowName(), "")

	var _ context.Context = ctx
}
