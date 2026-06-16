package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// AutomationOptions contains saved automation metadata plus the optional
// authoring spec that makes the automation runnable.
//
// The public API no longer models automations as a shell plus immutable
// versions: the runnable definition (steps, event, config, triggers, defaults,
// limits, …) lives inline on the loop. Set Spec to author that definition in
// the same call that creates the automation. Explicit fields below take
// precedence over the same keys in Spec.
type AutomationOptions struct {
	Name          string
	Description   string
	AgentID       string
	DefaultConfig map[string]interface{}
	Settings      map[string]interface{}
	Tags          map[string]string

	// Spec is the authoring definition for the automation. Recognised keys
	// mirror the loop spec (schema_version, steps, event, config, triggers,
	// defaults, limits, output, repositories, cleanup, …). When it carries
	// steps the automation is runnable immediately.
	Spec map[string]interface{}
}

// UpdateAutomationOptions contains metadata fields and authoring spec that can
// be updated on a saved automation. Only the fields that are set are sent.
type UpdateAutomationOptions struct {
	Name          string
	Description   string
	AgentID       string
	DefaultConfig map[string]interface{}
	Settings      map[string]interface{}
	Status        api.LoopStatus
	Tags          map[string]string

	// Spec replaces the authoring definition for the automation. See
	// AutomationOptions.Spec for the recognised keys.
	Spec map[string]interface{}
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

// CreateAutomation creates a saved automation. Provide AutomationOptions.Spec
// with steps to make it runnable in the same call.
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

// UpdateAutomation updates mutable saved automation metadata and authoring
// spec by loop ID.
func (c *Client) UpdateAutomation(ctx context.Context, id string, opts UpdateAutomationOptions) (*api.Loop, error) {
	req, err := updateAutomationRequest(opts)
	if err != nil {
		return nil, err
	}
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

func createAutomationRequest(opts AutomationOptions) (api.CreateLoopRequest, error) {
	if opts.Name == "" {
		return api.CreateLoopRequest{}, errors.New("mobius: automation name is required")
	}
	req := api.CreateLoopRequest{}
	if err := applyAutomationSpec(opts.Spec, &req); err != nil {
		return api.CreateLoopRequest{}, err
	}
	req.Name = opts.Name
	if opts.Description != "" {
		req.Description = &opts.Description
	}
	if opts.AgentID != "" {
		req.AgentId = &opts.AgentID
	}
	if opts.DefaultConfig != nil {
		req.DefaultConfig = &opts.DefaultConfig
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

func updateAutomationRequest(opts UpdateAutomationOptions) (api.UpdateLoopRequest, error) {
	req := api.UpdateLoopRequest{}
	if err := applyAutomationSpec(opts.Spec, &req); err != nil {
		return api.UpdateLoopRequest{}, err
	}
	if opts.Name != "" {
		req.Name = &opts.Name
	}
	if opts.Description != "" {
		req.Description = &opts.Description
	}
	if opts.AgentID != "" {
		req.AgentId = &opts.AgentID
	}
	if opts.DefaultConfig != nil {
		req.DefaultConfig = &opts.DefaultConfig
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
	return req, nil
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

// applyAutomationSpec decodes the authoring spec map into the typed request.
// The loop spec fields are top-level on the create/update request, so a JSON
// round-trip populates steps, event, config, triggers, defaults, limits, and
// the rest in one shot. Explicit option fields are layered on top by the caller.
func applyAutomationSpec(spec map[string]interface{}, req interface{}) error {
	if spec == nil {
		return nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("mobius: invalid automation spec: %w", err)
	}
	if err := json.Unmarshal(raw, req); err != nil {
		return fmt.Errorf("mobius: invalid automation spec: %w", err)
	}
	return nil
}

func unexpectedAutomationStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
