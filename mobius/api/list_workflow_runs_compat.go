package api

import (
	"context"
	"net/http"
)

// ListWorkflowRunsResponse is kept as a short-lived compatibility alias for
// callers that used the generated client before the operationId was renamed to
// listRunsForWorkflow.
//
// Deprecated: use ListRunsForWorkflowResponse.
type ListWorkflowRunsResponse = ListRunsForWorkflowResponse

// ListWorkflowRuns calls ListRunsForWorkflow.
//
// Deprecated: use ListRunsForWorkflow.
func (c *Client) ListWorkflowRuns(ctx context.Context, project ProjectHandleParam, id IDParam, reqEditors ...RequestEditorFn) (*http.Response, error) {
	return c.ListRunsForWorkflow(ctx, project, id, reqEditors...)
}

// NewListWorkflowRunsRequest calls NewListRunsForWorkflowRequest.
//
// Deprecated: use NewListRunsForWorkflowRequest.
func NewListWorkflowRunsRequest(server string, project ProjectHandleParam, id IDParam) (*http.Request, error) {
	return NewListRunsForWorkflowRequest(server, project, id)
}

// ListWorkflowRunsWithResponse calls ListRunsForWorkflowWithResponse.
//
// Deprecated: use ListRunsForWorkflowWithResponse.
func (c *ClientWithResponses) ListWorkflowRunsWithResponse(ctx context.Context, project ProjectHandleParam, id IDParam, reqEditors ...RequestEditorFn) (*ListWorkflowRunsResponse, error) {
	return c.ListRunsForWorkflowWithResponse(ctx, project, id, reqEditors...)
}

// ParseListWorkflowRunsResponse calls ParseListRunsForWorkflowResponse.
//
// Deprecated: use ParseListRunsForWorkflowResponse.
func ParseListWorkflowRunsResponse(rsp *http.Response) (*ListWorkflowRunsResponse, error) {
	return ParseListRunsForWorkflowResponse(rsp)
}
