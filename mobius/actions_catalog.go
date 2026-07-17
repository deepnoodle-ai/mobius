package mobius

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListActionCatalog returns every action available to the bound
// project — both project-owned actions and platform-provided
// integration actions. The returned `Readiness` field distinguishes
// "action exists" from "action exists but the required integration is
// not configured": an entry with `Readiness == needs_setup` is
// registered in the catalog but cannot currently be invoked (see
// `ReadinessReason` for why). Callers that want to disambiguate "404
// because the action does not exist" from "fails because the
// integration is missing credentials" should consult this list before
// calling [Context.RunServerAction].
func (c *Client) ListActionCatalog(ctx context.Context) ([]api.ActionCatalogEntry, error) {
	resp, err := c.ac.ListCatalogActionsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle))
	if err != nil {
		return nil, fmt.Errorf("mobius: list action catalog: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("mobius: list action catalog: unexpected status %s: %s", resp.Status(), string(resp.Body))
	}
	return resp.JSON200.Items, nil
}

// ListActionInvocationsOptions filters and paginates the project's action
// invocation audit records. Zero-valued fields are omitted from the query.
type ListActionInvocationsOptions struct {
	// RunID filters to invocations from a specific loop run.
	RunID string
	// JobID filters to invocations from a specific job.
	JobID string
	// EnvironmentID filters to invocations executed in a specific environment.
	EnvironmentID string
	// ActionName filters to invocations of a specific action.
	ActionName string
	// ActionID filters to an immutable project or organization Action ID.
	ActionID string
	// DefinitionScope filters by the scope that owned the selected
	// definition: platform, project, or organization.
	DefinitionScope api.ListActionInvocationsParamsDefinitionScope
	// SecretVersion filters to deliveries signed with a specific
	// signing-secret version.
	SecretVersion int64
	// DeliveryID filters to a signed delivery identity.
	DeliveryID string
	// CorrelationID filters to the request or dispatch correlation identity.
	CorrelationID string
	// Status filters by terminal status (e.g. "success", "failed").
	Status string
	Cursor string
	Limit  int
}

// ListActionInvocations lists recent action invocation audit records from
// loops, agents, direct invocations, and job-backed execution. Each
// [api.ActionInvocationEntry] carries definition provenance (ActionId,
// DefinitionScope) and, for signed HTTP deliveries, the delivery identity
// (DeliveryId, CorrelationId, SecretVersion).
func (c *Client) ListActionInvocations(ctx context.Context, opts *ListActionInvocationsOptions) (*api.ActionInvocationListResponse, error) {
	params := &api.ListActionInvocationsParams{}
	if opts != nil {
		if opts.RunID != "" {
			params.RunId = &opts.RunID
		}
		if opts.JobID != "" {
			params.JobId = &opts.JobID
		}
		if opts.EnvironmentID != "" {
			params.EnvironmentId = &opts.EnvironmentID
		}
		if opts.ActionName != "" {
			params.ActionName = &opts.ActionName
		}
		if opts.ActionID != "" {
			params.ActionId = &opts.ActionID
		}
		if opts.DefinitionScope != "" {
			params.DefinitionScope = &opts.DefinitionScope
		}
		if opts.SecretVersion > 0 {
			params.SecretVersion = &opts.SecretVersion
		}
		if opts.DeliveryID != "" {
			params.DeliveryId = &opts.DeliveryID
		}
		if opts.CorrelationID != "" {
			params.CorrelationId = &opts.CorrelationID
		}
		if opts.Status != "" {
			params.Status = &opts.Status
		}
		if opts.Cursor != "" {
			cursor := api.CursorParam(opts.Cursor)
			params.Cursor = &cursor
		}
		if opts.Limit > 0 {
			limit := api.LimitParam(opts.Limit)
			params.Limit = &limit
		}
	}
	resp, err := c.ac.ListActionInvocationsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list action invocations: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list action invocations", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// GetActionCatalogEntry returns the catalog entry for a single action
// by name. Returns an error wrapping the underlying status when the
// action is not found (404). Use the entry's `Readiness` and
// `Integration` fields to distinguish "missing action" from "missing
// integration credentials" before calling [Context.RunServerAction].
func (c *Client) GetActionCatalogEntry(ctx context.Context, actionName string) (*api.ActionCatalogEntry, error) {
	if actionName == "" {
		return nil, fmt.Errorf("mobius: get action catalog entry: actionName is required")
	}
	resp, err := c.ac.GetCatalogActionWithResponse(ctx,
		api.ProjectHandleParam(c.projectHandle),
		api.ActionNameParam(actionName),
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: get action catalog entry: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("mobius: get action catalog entry %q: unexpected status %s: %s", actionName, resp.Status(), string(resp.Body))
	}
	return resp.JSON200, nil
}
