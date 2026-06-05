package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// AutomationOptions contains common saved automation metadata.
type AutomationOptions struct {
	Name           string
	Handle         string
	Description    string
	DefaultAgentID string
	DefaultInputs  map[string]interface{}
	Settings       map[string]interface{}
	Tags           map[string]string
	Triggers       []api.AutomationSpecTrigger
}

// UpdateAutomationOptions contains metadata fields that can be updated on a
// saved automation.
type UpdateAutomationOptions struct {
	Name            string
	Description     string
	DefaultAgentID  string
	DefaultInputs   map[string]interface{}
	Settings        map[string]interface{}
	Status          api.AutomationStatus
	Tags            map[string]string
	Triggers        []api.AutomationSpecTrigger
	ReplaceTriggers bool
}

// AutomationVersionOptions configures immutable automation-version creation.
type AutomationVersionOptions struct {
	CompiledPlan map[string]interface{}
	Publish      bool
}

// ListAutomationsOptions filters and paginates saved automations.
type ListAutomationsOptions struct {
	Status api.ListAutomationsParamsStatus
	Cursor string
	Limit  int
}

// AutomationSyncResult describes one EnsureAutomation or SyncAutomations result.
type AutomationSyncResult struct {
	Automation *api.Automation
	Version    *api.AutomationVersion
	Created    bool
	Updated    bool
	Published  bool
}

// AutomationConfig is one desired saved automation definition for
// SyncAutomations.
type AutomationConfig struct {
	Spec           map[string]interface{}
	Options        AutomationOptions
	VersionOptions AutomationVersionOptions
}

// ListAutomations returns saved automation summaries.
func (c *Client) ListAutomations(ctx context.Context, opts *ListAutomationsOptions) (*api.AutomationListResponse, error) {
	resp, err := c.ac.ListAutomationsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), listAutomationsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list automations: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("list automations", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// GetAutomation returns a saved automation by handle.
func (c *Client) GetAutomation(ctx context.Context, handle string) (*api.Automation, error) {
	resp, err := c.ac.GetAutomationWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), handle)
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
func (c *Client) CreateAutomation(ctx context.Context, opts AutomationOptions) (*api.Automation, error) {
	req, err := createAutomationRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.CreateAutomationWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create automation: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedAutomationStatus("create automation", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

// UpdateAutomation updates mutable saved automation metadata by handle.
func (c *Client) UpdateAutomation(ctx context.Context, handle string, opts UpdateAutomationOptions) (*api.Automation, error) {
	req := updateAutomationRequest(opts)
	resp, err := c.ac.UpdateAutomationWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), handle, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update automation: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("update automation", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteAutomation archives a saved automation by handle.
func (c *Client) DeleteAutomation(ctx context.Context, handle string) error {
	resp, err := c.ac.DeleteAutomationWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), handle)
	if err != nil {
		return fmt.Errorf("mobius: delete automation: %w", err)
	}
	if resp.StatusCode() != 204 {
		return unexpectedAutomationStatus("delete automation", resp.Status(), resp.Body)
	}
	return nil
}

// CreateAutomationVersion creates an immutable automation version from the
// authoring spec.
func (c *Client) CreateAutomationVersion(ctx context.Context, handle string, spec map[string]interface{}, opts *AutomationVersionOptions) (*api.AutomationVersion, error) {
	typedSpec, err := automationSpecFromMap(spec)
	if err != nil {
		return nil, err
	}
	req := api.CreateAutomationVersionRequest{Spec: typedSpec}
	if opts != nil && opts.CompiledPlan != nil {
		req.CompiledPlan = &opts.CompiledPlan
	}
	resp, err := c.ac.CreateAutomationVersionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), handle, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create automation version: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedAutomationStatus("create automation version", resp.Status(), resp.Body)
	}
	version := resp.JSON201
	if opts != nil && opts.Publish {
		_, err := c.PublishAutomationVersion(ctx, handle, version.Version)
		if err != nil {
			return nil, err
		}
	}
	return version, nil
}

// PublishAutomationVersion makes version runnable and returns the updated
// automation.
func (c *Client) PublishAutomationVersion(ctx context.Context, handle string, version int) (*api.Automation, error) {
	resp, err := c.ac.PublishAutomationVersionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), handle, version)
	if err != nil {
		return nil, fmt.Errorf("mobius: publish automation version: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedAutomationStatus("publish automation version", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// EnsureAutomation creates or updates the automation matching opts.Handle and
// creates a new version when spec differs from the latest known version input.
func (c *Client) EnsureAutomation(ctx context.Context, spec map[string]interface{}, opts AutomationOptions, versionOpts *AutomationVersionOptions) (*AutomationSyncResult, error) {
	desired, err := normalizeAutomationOptions(spec, opts)
	if err != nil {
		return nil, err
	}
	spec = automationSpecWithTriggers(spec, desired.Triggers)
	existing, err := c.findAutomation(ctx, desired)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		automation, err := c.CreateAutomation(ctx, desired)
		if err != nil {
			return nil, err
		}
		version, err := c.CreateAutomationVersion(ctx, automation.Handle, spec, versionOpts)
		if err != nil {
			return nil, err
		}
		result := &AutomationSyncResult{Automation: automation, Version: version, Created: true}
		if versionOpts != nil && versionOpts.Publish {
			automation, err := c.GetAutomation(ctx, automation.Handle)
			if err != nil {
				return nil, err
			}
			result.Automation = automation
			result.Published = true
		}
		return result, nil
	}

	update := automationUpdateForDiff(existing, desired)
	result := &AutomationSyncResult{Automation: existing}
	if update != nil {
		automation, err := c.UpdateAutomation(ctx, existing.Handle, *update)
		if err != nil {
			return nil, err
		}
		result.Automation = automation
		result.Updated = true
	}
	version, err := c.CreateAutomationVersion(ctx, result.Automation.Handle, spec, versionOpts)
	if err != nil {
		return nil, err
	}
	result.Version = version
	if versionOpts != nil && versionOpts.Publish {
		automation, err := c.GetAutomation(ctx, result.Automation.Handle)
		if err != nil {
			return nil, err
		}
		result.Automation = automation
		result.Published = true
	}
	return result, nil
}

// SyncAutomations ensures each desired saved automation in order.
func (c *Client) SyncAutomations(ctx context.Context, configs ...AutomationConfig) ([]AutomationSyncResult, error) {
	results := make([]AutomationSyncResult, 0, len(configs))
	for _, cfg := range configs {
		result, err := c.EnsureAutomation(ctx, cfg.Spec, cfg.Options, &cfg.VersionOptions)
		if err != nil {
			return results, err
		}
		results = append(results, *result)
	}
	return results, nil
}

func createAutomationRequest(opts AutomationOptions) (api.CreateAutomationRequest, error) {
	normalized, err := normalizeAutomationOptions(nil, opts)
	if err != nil {
		return api.CreateAutomationRequest{}, err
	}
	req := api.CreateAutomationRequest{
		Handle: normalized.Handle,
		Name:   normalized.Name,
	}
	if normalized.Description != "" {
		req.Description = &normalized.Description
	}
	if normalized.DefaultAgentID != "" {
		req.DefaultAgentId = &normalized.DefaultAgentID
	}
	if normalized.DefaultInputs != nil {
		req.DefaultInputs = &normalized.DefaultInputs
	}
	if normalized.Settings != nil {
		req.Settings = &normalized.Settings
	}
	if normalized.Tags != nil {
		req.Tags = &normalized.Tags
	}
	return req, nil
}

func updateAutomationRequest(opts UpdateAutomationOptions) api.UpdateAutomationRequest {
	req := api.UpdateAutomationRequest{}
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
		req.Tags = &opts.Tags
	}
	return req
}

func listAutomationsParams(opts *ListAutomationsOptions) *api.ListAutomationsParams {
	if opts == nil {
		return nil
	}
	params := &api.ListAutomationsParams{}
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

func normalizeAutomationOptions(spec map[string]interface{}, opts AutomationOptions) (AutomationOptions, error) {
	normalized := opts
	if normalized.Name == "" {
		normalized.Name = stringMapValue(spec, "name")
	}
	if normalized.Handle == "" {
		normalized.Handle = stringMapValue(spec, "handle")
	}
	if normalized.Name == "" {
		return normalized, errors.New("mobius: automation name is required")
	}
	if normalized.Handle == "" {
		return normalized, errors.New("mobius: automation handle is required")
	}
	return normalized, nil
}

func stringMapValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func (c *Client) findAutomation(ctx context.Context, desired AutomationOptions) (*api.Automation, error) {
	if desired.Handle == "" && desired.Name == "" {
		return nil, errors.New("mobius: ensure automation requires a handle or name")
	}
	cursor := ""
	for {
		page, err := c.ListAutomations(ctx, &ListAutomationsOptions{Cursor: cursor, Limit: 100})
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
		if page.HasMore == nil || !*page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return nil, nil
		}
		cursor = *page.NextCursor
	}
}

func automationUpdateForDiff(current *api.Automation, desired AutomationOptions) *UpdateAutomationOptions {
	update := &UpdateAutomationOptions{}
	changed := false
	if desired.Name != "" && current.Name != desired.Name {
		update.Name = desired.Name
		changed = true
	}
	if desired.Description != "" && stringPtrValue(current.Description) != desired.Description {
		update.Description = desired.Description
		changed = true
	}
	if desired.DefaultAgentID != "" && stringPtrValue(current.DefaultAgentId) != desired.DefaultAgentID {
		update.DefaultAgentID = desired.DefaultAgentID
		changed = true
	}
	if desired.DefaultInputs != nil && !reflect.DeepEqual(mapPtrValue(current.DefaultInputs), desired.DefaultInputs) {
		update.DefaultInputs = desired.DefaultInputs
		changed = true
	}
	if desired.Settings != nil && !reflect.DeepEqual(mapPtrValue(current.Settings), desired.Settings) {
		update.Settings = desired.Settings
		changed = true
	}
	if desired.Tags != nil && !reflect.DeepEqual(stringMapPtrValue(current.Tags), desired.Tags) {
		update.Tags = desired.Tags
		changed = true
	}
	if !changed {
		return nil
	}
	return update
}

func mapPtrValue(m *map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	return *m
}

func stringMapPtrValue(m *map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return *m
}

func automationSpecFromMap(spec map[string]interface{}) (api.AutomationSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return api.AutomationSpec{}, err
	}
	var out api.AutomationSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		return api.AutomationSpec{}, fmt.Errorf("mobius: invalid automation spec: %w", err)
	}
	return out, nil
}

func automationSpecWithTriggers(spec map[string]interface{}, triggers []api.AutomationSpecTrigger) map[string]interface{} {
	if len(triggers) == 0 {
		return spec
	}
	out := map[string]interface{}{}
	for key, value := range spec {
		out[key] = value
	}
	out["triggers"] = triggers
	return out
}

func stringPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func unexpectedAutomationStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
