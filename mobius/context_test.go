package mobius

import (
	"context"
	"log/slog"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

// newTestContext builds an executionContext bound to a job for directly
// exercising worker-side helpers without spinning up a Worker.
func newTestContext(job *runtimeJob) Context {
	return newContext(context.Background(), nil, job, slog.Default(), nil)
}

func TestContext_JobIdentity(t *testing.T) {
	ctx := newTestContext(&runtimeJob{
		JobID:      "job_1",
		LeaseToken: "lease_1",
		Attempt:    3,
		Queue:      "default",
	})

	assert.Equal(t, ctx.JobID(), "job_1")
	assert.Equal(t, ctx.Attempt(), 3)
	assert.Equal(t, ctx.Queue(), "default")
	leaseCtx, ok := ctx.(interface{ LeaseToken() string })
	assert.True(t, ok)
	assert.Equal(t, leaseCtx.LeaseToken(), "lease_1")

	var _ context.Context = ctx
}
