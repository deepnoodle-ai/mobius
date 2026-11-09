package mobius

import (
	"context"
	"log/slog"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// Test hooks for the external mobius_test package, which can import
// mobius/action without an import cycle.

// ClaimFrameActionNamesForTest returns the action names the worker
// advertises when it claims jobs.
func (w *Worker) ClaimFrameActionNamesForTest() []string {
	frame := w.claimFrame(1)
	if frame.ActionNames == nil {
		return nil
	}
	return *frame.ActionNames
}

// RunActionJobForTest dispatches an action job by name through the worker's
// registry, the path a claimed job takes.
func (w *Worker) RunActionJobForTest(actionName string, params map[string]any) (any, error) {
	job := &runtimeJob{
		JobID:      "job_test",
		Kind:       api.WorkerSocketClaimedJobKindActionExecution,
		Action:     actionName,
		Parameters: params,
	}
	ctx := newContext(context.Background(), w.client, job, slog.Default(), nil)
	return w.executeAction(ctx, job)
}
