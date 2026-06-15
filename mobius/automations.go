package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// AutomationOptions contains common saved automation metadata.
type AutomationOptions struct {
	Name           string
	Description    string
	DefaultAgentID string
	DefaultInputs  map[string]interface{}
	Settings       map[string]interface{}
	Tags           map[string]string
}

// UpdateAutomationOptions contains metadata fields that can be updated on a
// saved automation.
type UpdateAutomationOptions struct {
	Name           string
	Description    string
	DefaultAgentID string
	DefaultInputs  map[string]interface{}
	Settings       map[string]interface{}
	Status         api.LoopStatus
	Tags           map[string]string
}

// AutomationVersionOptions configures immutable automation-version creation.
type AutomationVersionOptions struct {
	// CompiledPlan is retained for source compatibility. The current public API
	// compiles server-side and no longer accepts this field on create-version.
	CompiledPlan map[string]interface{}
	Publish      bool
}

// ListAutomationsOptions filters and paginates saved automations.
type ListAutomationsOptions struct {
	Status api.ListLoopsParamsStatus
	Cursor string
	Limit  int
}

// ListAutomations returns saved automation summaries.
func (c *Client) ListAutomations(ctx context.Context, opts *ListAutomationsOptions) (*api.LoopListResponse, error) {
	resp, err := c.ac.ListLoopsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listAutomationsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list automations: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("list automations", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetAutomation returns a saved automation by loop ID.
func (c *Client) GetAutomation(ctx context.Context, id string) (*api.Loop, error) {
	resp, err := c.ac.GetLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id))
	if err != nil {
		return nil, fmt.Errorf("mobius: get automation: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("get automation", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CreateAutomation creates a saved automation shell. Add runnable behavior with
// CreateAutomationVersion and PublishAutomationVersion.
func (c *Client) CreateAutomation(ctx context.Context, opts AutomationOptions) (*api.Loop, error) {
	req, err := createAutomationRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.CreateLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create automation: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedAutomationStatus("create automation", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// UpdateAutomation updates mutable saved automation metadata by loop ID.
func (c *Client) UpdateAutomation(ctx context.Context, id string, opts UpdateAutomationOptions) (*api.Loop, error) {
	req := updateAutomationRequest(opts)
	resp, err := c.ac.UpdateLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update automation: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("update automation", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteAutomation archives a saved automation by loop ID.
func (c *Client) DeleteAutomation(ctx context.Context, id string) error {
	resp, err := c.ac.DeleteLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id))
	if err != nil {
		return fmt.Errorf("mobius: delete automation: %w", err)
	}
	if resp.StatusCode() != 204 {
		return unexpectedAutomationStatus("delete automation", resp.Status(), resp.Body)
	}
	return nil
}

// CreateAutomationVersion creates an immutable automation version from the
// authoring spec, addressed by loop ID.
func (c *Client) CreateAutomationVersion(ctx context.Context, id string, spec map[string]interface{}, opts *AutomationVersionOptions) (*api.LoopVersion, error) {
	typedSpec, err := automationSpecFromMap(spec)
	if err != nil {
		return nil, err
	}
	req := api.CreateLoopVersionRequest{Spec: typedSpec}
	resp, err := c.ac.CreateLoopVersionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create automation version: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedAutomationStatus("create automation version", resp.Status(), resp.Body)
	}
	version := resp.JSON201
	if opts != nil && opts.Publish {
		if _, err := c.PublishAutomationVersion(ctx, id, version.Version); err != nil {
			return nil, err
		}
	}
	return version, nil
}

// PublishAutomationVersion makes version runnable and returns the updated
// automation, addressed by loop ID.
func (c *Client) PublishAutomationVersion(ctx context.Context, id string, version int) (*api.Loop, error) {
	resp, err := c.ac.PublishLoopVersionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id), version)
	if err != nil {
		return nil, fmt.Errorf("mobius: publish automation version: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("publish automation version", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

func createAutomationRequest(opts AutomationOptions) (api.CreateLoopRequest, error) {
	if opts.Name == "" {
		return api.CreateLoopRequest{}, errors.New("mobius: automation name is required")
	}
	req := api.CreateLoopRequest{Name: opts.Name}
	if opts.Description != "" {
		req.Description = &opts.Description
	}
	if opts.DefaultAgentID != "" {
		req.DefaultAgentId = &opts.DefaultAgentID
	}
	if opts.DefaultInputs != nil {
		req.DefaultInputs = &opts.DefaultInputs
	}
	if opts.Settings != nil {
		req.Settings = &opts.Settings
	}
	if opts.Tags != nil {
		tags := api.TagMap(opts.Tags)
		req.Tags = &tags
	}
	return req, nil
}

func updateAutomationRequest(opts UpdateAutomationOptions) api.UpdateLoopRequest {
	req := api.UpdateLoopRequest{}
	if opts.Name != "" {
		req.Name = &opts.Name
	}
	if opts.Description != "" {
		req.Description = &opts.Description
	}
	if opts.DefaultAgentID != "" {
		req.DefaultAgentId = &opts.DefaultAgentID
	}
	if opts.DefaultInputs != nil {
		req.DefaultInputs = &opts.DefaultInputs
	}
	if opts.Settings != nil {
		req.Settings = &opts.Settings
	}
	if opts.Status != "" {
		req.Status = &opts.Status
	}
	if opts.Tags != nil {
		tags := api.TagMap(opts.Tags)
		req.Tags = &tags
	}
	return req
}

func listAutomationsParams(opts *ListAutomationsOptions) *api.ListLoopsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListLoopsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	return params
}

func automationSpecFromMap(spec map[string]interface{}) (api.LoopSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return api.LoopSpec{}, err
	}
	var out api.LoopSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		return api.LoopSpec{}, fmt.Errorf("mobius: invalid automation spec: %w", err)
	}
	return out, nil
}

func unexpectedAutomationStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
