package mobius

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListActionCatalog returns every action available to the bound
// project — both project-owned actions and platform-provided
// integration actions. The returned `Available` flag distinguishes
// "action exists" from "action exists but the required integration is
// not configured": an entry with `Available == false` is registered in
// the catalog but cannot currently be invoked. Callers that want to
// disambiguate "404 because the action does not exist" from "fails
// because the integration is missing credentials" should consult this
// list before calling [Context.RunServerAction].
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

// GetActionCatalogEntry returns the catalog entry for a single action
// by name. Returns an error wrapping the underlying status when the
// action is not found (404). Use the entry's `Available` and
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
