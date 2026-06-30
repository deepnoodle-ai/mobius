package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// LoopOptions contains saved loop metadata plus the optional authoring spec
// that makes the loop runnable.
//
// The public API models the runnable definition (steps, event, config,
// triggers, defaults, limits, …) inline on the loop. Set Spec to author that
// definition in the same call that creates the loop. Explicit fields below
// take precedence over the same keys in Spec.
type LoopOptions struct {
	Name          string
	Description   string
	AgentID       string
	DefaultConfig map[string]interface{}
	Settings      map[string]interface{}
	Tags          map[string]string

	// Spec is the authoring definition for the loop. Recognised keys are
	// schema_version, steps, event, config, triggers, defaults, limits,
	// output, repositories, cleanup, …. When it carries steps the loop is
	// runnable immediately.
	Spec map[string]interface{}
}

// UpdateLoopOptions contains metadata fields and authoring spec that can be
// updated on a saved loop. Only the fields that are set are sent.
type UpdateLoopOptions struct {
	Name          string
	Description   string
	AgentID       string
	DefaultConfig map[string]interface{}
	Settings      map[string]interface{}
	Status        api.LoopStatus
	Tags          map[string]string

	// Spec replaces the authoring definition for the loop. See
	// LoopOptions.Spec for the recognised keys.
	Spec map[string]interface{}
}

// ListLoopsOptions filters and paginates saved loops.
type ListLoopsOptions struct {
	Status api.ListLoopsParamsStatus
	Cursor string
	Limit  int
}

// ListLoops returns saved loop summaries.
func (c *Client) ListLoops(ctx context.Context, opts *ListLoopsOptions) (*api.LoopListResponse, error) {
	resp, err := c.ac.ListLoopsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listLoopsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list loops: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedLoopStatus("list loops", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetLoop returns a saved loop by ID.
func (c *Client) GetLoop(ctx context.Context, id string) (*api.Loop, error) {
	resp, err := c.ac.GetLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id))
	if err != nil {
		return nil, fmt.Errorf("mobius: get loop: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedLoopStatus("get loop", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// CreateLoop creates a saved loop. Provide LoopOptions.Spec with steps to
// make it runnable in the same call.
func (c *Client) CreateLoop(ctx context.Context, opts LoopOptions) (*api.Loop, error) {
	req, err := createLoopRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.CreateLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create loop: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedLoopStatus("create loop", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// UpdateLoop updates mutable saved loop metadata and authoring spec by ID.
func (c *Client) UpdateLoop(ctx context.Context, id string, opts UpdateLoopOptions) (*api.Loop, error) {
	req, err := updateLoopRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.UpdateLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update loop: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedLoopStatus("update loop", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteLoop archives a saved loop by ID.
func (c *Client) DeleteLoop(ctx context.Context, id string) error {
	resp, err := c.ac.DeleteLoopWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(id))
	if err != nil {
		return fmt.Errorf("mobius: delete loop: %w", err)
	}
	if resp.StatusCode() != 204 {
		return unexpectedLoopStatus("delete loop", resp.Status(), resp.Body)
	}
	return nil
}

func createLoopRequest(opts LoopOptions) (api.CreateLoopRequest, error) {
	if opts.Name == "" {
		return api.CreateLoopRequest{}, errors.New("mobius: loop name is required")
	}
	req := api.CreateLoopRequest{}
	if err := applyLoopSpec(opts.Spec, &req); err != nil {
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

func updateLoopRequest(opts UpdateLoopOptions) (api.UpdateLoopRequest, error) {
	req := api.UpdateLoopRequest{}
	if err := applyLoopSpec(opts.Spec, &req); err != nil {
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

func listLoopsParams(opts *ListLoopsOptions) *api.ListLoopsParams {
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

// applyLoopSpec decodes the authoring spec map into the typed request. The
// loop spec fields are top-level on the create/update request, so a JSON
// round-trip populates steps, event, config, triggers, defaults, limits, and
// the rest in one shot. Explicit option fields are layered on top by the caller.
func applyLoopSpec(spec map[string]interface{}, req interface{}) error {
	if spec == nil {
		return nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("mobius: invalid loop spec: %w", err)
	}
	if err := json.Unmarshal(raw, req); err != nil {
		return fmt.Errorf("mobius: invalid loop spec: %w", err)
	}
	return nil
}

func unexpectedLoopStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
