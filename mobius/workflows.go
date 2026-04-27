package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// WorkflowOptions contains common saved workflow metadata.
type WorkflowOptions struct {
	// Name defaults to spec.Name when omitted.
	Name string
	// Handle is the stable URL-safe workflow identifier. When omitted on create,
	// the server derives one from Name.
	Handle          string
	Description     string
	PublishedAsTool *bool
	Tags            map[string]string
}

// UpdateWorkflowOptions contains metadata and spec fields that can be updated
// on a saved workflow definition.
type UpdateWorkflowOptions struct {
	Name            string
	Description     string
	PublishedAsTool *bool
	Spec            *api.WorkflowSpec
	Tags            map[string]string
}

// ListWorkflowsOptions filters and paginates saved workflow definitions.
type ListWorkflowsOptions struct {
	Cursor string
	Limit  int
	Tags   []string
}

// WorkflowSyncResult describes one EnsureWorkflow or SyncWorkflows outcome.
type WorkflowSyncResult struct {
	Definition *api.WorkflowDefinition
	Created    bool
	Updated    bool
}

// WorkflowDefinitionConfig is one desired saved workflow definition for
// SyncWorkflows.
type WorkflowDefinitionConfig struct {
	Spec    api.WorkflowSpec
	Options WorkflowOptions
}

// ListWorkflows returns saved workflow definition summaries.
func (c *Client) ListWorkflows(ctx context.Context, opts *ListWorkflowsOptions) (*api.WorkflowDefinitionListResponse, error) {
	resp, err := c.ac.ListWorkflowsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listWorkflowsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list workflows: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedWorkflowStatus("list workflows", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetWorkflow returns a saved workflow definition including its latest spec.
func (c *Client) GetWorkflow(ctx context.Context, workflowID string) (*api.WorkflowDefinition, error) {
	resp, err := c.ac.GetWorkflowWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(workflowID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get workflow: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedWorkflowStatus("get workflow", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CreateWorkflow creates a saved workflow definition and its first immutable
// version.
func (c *Client) CreateWorkflow(ctx context.Context, spec api.WorkflowSpec, opts *WorkflowOptions) (*api.WorkflowDefinition, error) {
	req := createWorkflowRequest(spec, opts)
	resp, err := c.ac.CreateWorkflowWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create workflow: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedWorkflowStatus("create workflow", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// UpdateWorkflow updates saved workflow metadata and optionally creates a new
// immutable version when Spec is supplied.
func (c *Client) UpdateWorkflow(ctx context.Context, workflowID string, opts *UpdateWorkflowOptions) (*api.WorkflowDefinition, error) {
	req := updateWorkflowRequest(opts)
	resp, err := c.ac.UpdateWorkflowWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(workflowID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update workflow: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedWorkflowStatus("update workflow", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// EnsureWorkflow creates or updates the saved workflow matching opts.Handle
// (preferred) or opts.Name/spec.Name. It returns whether the call created or
// updated the definition.
func (c *Client) EnsureWorkflow(ctx context.Context, spec api.WorkflowSpec, opts *WorkflowOptions) (*WorkflowSyncResult, error) {
	desired := normalizeWorkflowOptions(spec, opts)
	existing, err := c.findWorkflow(ctx, desired)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		def, err := c.CreateWorkflow(ctx, spec, &desired)
		if err != nil {
			return nil, err
		}
		return &WorkflowSyncResult{Definition: def, Created: true}, nil
	}

	current, err := c.GetWorkflow(ctx, existing.Id)
	if err != nil {
		return nil, err
	}
	update := workflowUpdateForDiff(current, spec, desired)
	if update == nil {
		return &WorkflowSyncResult{Definition: current}, nil
	}
	def, err := c.UpdateWorkflow(ctx, current.Id, update)
	if err != nil {
		return nil, err
	}
	return &WorkflowSyncResult{Definition: def, Updated: true}, nil
}

// SyncWorkflows ensures each desired saved workflow definition in order.
func (c *Client) SyncWorkflows(ctx context.Context, defs ...WorkflowDefinitionConfig) ([]WorkflowSyncResult, error) {
	results := make([]WorkflowSyncResult, 0, len(defs))
	for _, def := range defs {
		result, err := c.EnsureWorkflow(ctx, def.Spec, &def.Options)
		if err != nil {
			return results, err
		}
		results = append(results, *result)
	}
	return results, nil
}

func createWorkflowRequest(spec api.WorkflowSpec, opts *WorkflowOptions) api.CreateWorkflowRequest {
	normalized := normalizeWorkflowOptions(spec, opts)
	req := api.CreateWorkflowRequest{
		Name: normalized.Name,
		Spec: spec,
	}
	if normalized.Handle != "" {
		req.Handle = &normalized.Handle
	}
	if normalized.Description != "" {
		req.Description = &normalized.Description
	}
	if normalized.PublishedAsTool != nil {
		req.PublishedAsTool = normalized.PublishedAsTool
	}
	if normalized.Tags != nil {
		tags := api.TagMap(normalized.Tags)
		req.Tags = &tags
	}
	return req
}

func updateWorkflowRequest(opts *UpdateWorkflowOptions) api.UpdateWorkflowRequest {
	if opts == nil {
		return api.UpdateWorkflowRequest{}
	}
	req := api.UpdateWorkflowRequest{}
	if opts.Name != "" {
		req.Name = &opts.Name
	}
	if opts.Description != "" {
		req.Description = &opts.Description
	}
	if opts.PublishedAsTool != nil {
		req.PublishedAsTool = opts.PublishedAsTool
	}
	if opts.Spec != nil {
		req.Spec = opts.Spec
	}
	if opts.Tags != nil {
		tags := api.TagMap(opts.Tags)
		req.Tags = &tags
	}
	return req
}

func listWorkflowsParams(opts *ListWorkflowsOptions) *api.ListWorkflowsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListWorkflowsParams{}
	if opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	if len(opts.Tags) > 0 {
		tags := api.TagFilterParam(opts.Tags)
		params.Tag = &tags
	}
	return params
}

func normalizeWorkflowOptions(spec api.WorkflowSpec, opts *WorkflowOptions) WorkflowOptions {
	var normalized WorkflowOptions
	if opts != nil {
		normalized = *opts
	}
	if normalized.Name == "" {
		normalized.Name = spec.Name
	}
	return normalized
}

func (c *Client) findWorkflow(ctx context.Context, desired WorkflowOptions) (*api.WorkflowDefinitionSummary, error) {
	if desired.Handle == "" && desired.Name == "" {
		return nil, errors.New("mobius: ensure workflow requires a handle, name, or spec name")
	}
	cursor := ""
	for {
		page, err := c.ListWorkflows(ctx, &ListWorkflowsOptions{Cursor: cursor, Limit: 100})
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			item := &page.Items[i]
			if desired.Handle != "" && item.Handle == desired.Handle {
				return item, nil
			}
			if desired.Handle == "" && item.Name == desired.Name {
				return item, nil
			}
		}
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return nil, nil
		}
		cursor = *page.NextCursor
	}
}

func workflowUpdateForDiff(current *api.WorkflowDefinition, spec api.WorkflowSpec, desired WorkflowOptions) *UpdateWorkflowOptions {
	update := &UpdateWorkflowOptions{}
	changed := false
	if desired.Name != "" && current.Name != desired.Name {
		update.Name = desired.Name
		changed = true
	}
	if stringPtrValue(current.Description) != desired.Description && desired.Description != "" {
		update.Description = desired.Description
		changed = true
	}
	if boolPtrValue(current.PublishedAsTool) != boolPtrValue(desired.PublishedAsTool) && desired.PublishedAsTool != nil {
		update.PublishedAsTool = desired.PublishedAsTool
		changed = true
	}
	if desired.Tags != nil && !reflect.DeepEqual(workflowTags(current.Tags), desired.Tags) {
		update.Tags = desired.Tags
		changed = true
	}
	if !jsonEqual(current.Spec, spec) {
		update.Spec = &spec
		changed = true
	}
	if !changed {
		return nil
	}
	return update
}

func jsonEqual(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func stringPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func boolPtrValue(b *bool) bool {
	return b != nil && *b
}

func workflowTags(tags *api.TagMap) map[string]string {
	if tags == nil {
		return nil
	}
	return map[string]string(*tags)
}

func unexpectedWorkflowStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
