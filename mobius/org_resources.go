package mobius

import (
	"context"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListBlueprintBindingsOptions filters blueprint-owned org resources.
type ListBlueprintBindingsOptions struct {
	Namespace    string
	BlueprintKey string
}

// DeleteBlueprintOptions controls deletion of one blueprint identity.
type DeleteBlueprintOptions struct {
	Namespace      string
	DeleteRetained bool
}

// SetBlueprintProtectionOptions identifies the blueprint namespace.
type SetBlueprintProtectionOptions struct {
	Namespace string
}

// ListInteractionsOptions filters and paginates org interactions.
type ListInteractionsOptions struct {
	Status       api.InteractionStatus
	Kind         api.InteractionKind
	SessionID    string
	TargetUserID string
	Inbox        bool
	Cursor       string
	Limit        int
}

// ListPrincipalsOptions filters org machine principals.
type ListPrincipalsOptions struct {
	Kind            api.PrincipalKind
	IncludeDisabled bool
	Limit           int
}

// ListRoleAssignmentsOptions filters org role assignments.
type ListRoleAssignmentsOptions struct {
	PrincipalID string
	RoleID      string
}

// ListRolesOptions paginates org roles.
type ListRolesOptions struct {
	Cursor string
	Limit  int
}

// ApplyBlueprint applies an org blueprint in preview or apply mode.
func (c *Client) ApplyBlueprint(ctx context.Context, req api.ApplyBlueprintRequest) (*api.BlueprintApplyResult, error) {
	resp, err := c.ac.ApplyBlueprintWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: apply blueprint: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("apply blueprint", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListBlueprintBindings returns resources owned by org blueprints.
func (c *Client) ListBlueprintBindings(ctx context.Context, opts *ListBlueprintBindingsOptions) (*api.BlueprintBindingListResponse, error) {
	resp, err := c.ac.ListBlueprintBindingsWithResponse(ctx, listBlueprintBindingsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list blueprint bindings: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list blueprint bindings", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// SetBlueprintProtection updates protection for all resources in a blueprint.
func (c *Client) SetBlueprintProtection(ctx context.Context, blueprintKey string, protected bool, opts *SetBlueprintProtectionOptions) (*api.BlueprintBindingListResponse, error) {
	params := &api.SetBlueprintProtectionParams{}
	if opts != nil && opts.Namespace != "" {
		params.Namespace = &opts.Namespace
	}
	resp, err := c.ac.SetBlueprintProtectionWithResponse(
		ctx,
		blueprintKey,
		params,
		api.SetBlueprintProtectionRequest{Protected: protected},
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: set blueprint protection: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("set blueprint protection", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteBlueprint removes a blueprint and follows its binding retention policy.
func (c *Client) DeleteBlueprint(ctx context.Context, blueprintKey string, opts *DeleteBlueprintOptions) (*api.BlueprintDeleteResult, error) {
	params := &api.DeleteBlueprintParams{}
	if opts != nil {
		if opts.Namespace != "" {
			params.Namespace = &opts.Namespace
		}
		if opts.DeleteRetained {
			params.DeleteRetained = &opts.DeleteRetained
		}
	}
	resp, err := c.ac.DeleteBlueprintWithResponse(ctx, blueprintKey, params)
	if err != nil {
		return nil, fmt.Errorf("mobius: delete blueprint: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("delete blueprint", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListInteractions returns org interactions, including session-scoped ones.
func (c *Client) ListInteractions(ctx context.Context, opts *ListInteractionsOptions) (*api.InteractionListResponse, error) {
	resp, err := c.ac.ListInteractionsWithResponse(ctx, listInteractionsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list interactions: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list interactions", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListOrgPermissions returns the assignable permission catalog.
func (c *Client) ListOrgPermissions(ctx context.Context) (*api.PermissionCatalogResponse, error) {
	resp, err := c.ac.ListOrgPermissionsWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("mobius: list org permissions: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list org permissions", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListPrincipals returns org machine principals.
func (c *Client) ListPrincipals(ctx context.Context, opts *ListPrincipalsOptions) (*api.PrincipalListResponse, error) {
	resp, err := c.ac.ListPrincipalsWithResponse(ctx, listPrincipalsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list principals: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list principals", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreatePrincipal creates an org machine principal.
func (c *Client) CreatePrincipal(ctx context.Context, req api.CreatePrincipalRequest) (*api.Principal, error) {
	resp, err := c.ac.CreatePrincipalWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create principal: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedResourceStatus("create principal", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// GetPrincipal returns an org machine principal by ID.
func (c *Client) GetPrincipal(ctx context.Context, id string) (*api.Principal, error) {
	resp, err := c.ac.GetPrincipalWithResponse(ctx, api.IDParam(id))
	if err != nil {
		return nil, fmt.Errorf("mobius: get principal: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("get principal", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdatePrincipal updates an org machine principal by ID.
func (c *Client) UpdatePrincipal(ctx context.Context, id string, req api.UpdatePrincipalRequest) (*api.Principal, error) {
	resp, err := c.ac.UpdatePrincipalWithResponse(ctx, api.IDParam(id), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update principal: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("update principal", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeletePrincipal archives an org machine principal by ID.
func (c *Client) DeletePrincipal(ctx context.Context, id string) error {
	resp, err := c.ac.DeletePrincipalWithResponse(ctx, api.IDParam(id))
	if err != nil {
		return fmt.Errorf("mobius: delete principal: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedResourceStatus("delete principal", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// ListRoles returns org roles.
func (c *Client) ListRoles(ctx context.Context, opts *ListRolesOptions) (*api.RoleListResponse, error) {
	resp, err := c.ac.ListRolesWithResponse(ctx, listRolesParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list roles: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list roles", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateRole creates an org role.
func (c *Client) CreateRole(ctx context.Context, req api.CreateRoleRequest) (*api.Role, error) {
	resp, err := c.ac.CreateRoleWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create role: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedResourceStatus("create role", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// GetRole returns an org role by ID.
func (c *Client) GetRole(ctx context.Context, id string) (*api.Role, error) {
	resp, err := c.ac.GetRoleWithResponse(ctx, api.IDParam(id))
	if err != nil {
		return nil, fmt.Errorf("mobius: get role: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("get role", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateRole updates an org role by ID.
func (c *Client) UpdateRole(ctx context.Context, id string, req api.UpdateRoleRequest) (*api.Role, error) {
	resp, err := c.ac.UpdateRoleWithResponse(ctx, api.IDParam(id), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update role: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("update role", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteRole deletes an org role by ID.
func (c *Client) DeleteRole(ctx context.Context, id string) error {
	resp, err := c.ac.DeleteRoleWithResponse(ctx, api.IDParam(id))
	if err != nil {
		return fmt.Errorf("mobius: delete role: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedResourceStatus("delete role", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// ListRoleAssignments returns org role assignments.
func (c *Client) ListRoleAssignments(ctx context.Context, opts *ListRoleAssignmentsOptions) (*api.RoleAssignmentListResponse, error) {
	resp, err := c.ac.ListRoleAssignmentsWithResponse(ctx, listRoleAssignmentsParams(opts))
	if err != nil {
		return nil, fmt.Errorf("mobius: list role assignments: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list role assignments", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateRoleAssignment assigns an org role to a principal.
func (c *Client) CreateRoleAssignment(ctx context.Context, req api.CreateRoleAssignmentRequest) (*api.RoleAssignment, error) {
	resp, err := c.ac.CreateRoleAssignmentWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create role assignment: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedResourceStatus("create role assignment", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// DeleteRoleAssignment deletes an org role assignment by ID.
func (c *Client) DeleteRoleAssignment(ctx context.Context, id string) error {
	resp, err := c.ac.DeleteRoleAssignmentWithResponse(ctx, api.IDParam(id))
	if err != nil {
		return fmt.Errorf("mobius: delete role assignment: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedResourceStatus("delete role assignment", resp.HTTPResponse, resp.Body)
	}
	return nil
}

func listBlueprintBindingsParams(opts *ListBlueprintBindingsOptions) *api.ListBlueprintBindingsParams {
	params := &api.ListBlueprintBindingsParams{}
	if opts == nil {
		return params
	}
	if opts.Namespace != "" {
		params.Namespace = &opts.Namespace
	}
	if opts.BlueprintKey != "" {
		params.BlueprintKey = &opts.BlueprintKey
	}
	return params
}

func listInteractionsParams(opts *ListInteractionsOptions) *api.ListInteractionsParams {
	params := &api.ListInteractionsParams{}
	if opts == nil {
		return params
	}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.Kind != "" {
		params.Kind = &opts.Kind
	}
	if opts.SessionID != "" {
		params.SessionId = &opts.SessionID
	}
	if opts.TargetUserID != "" {
		params.TargetUserId = &opts.TargetUserID
	}
	if opts.Inbox {
		params.Inbox = &opts.Inbox
	}
	if opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	return params
}

func listPrincipalsParams(opts *ListPrincipalsOptions) *api.ListPrincipalsParams {
	params := &api.ListPrincipalsParams{}
	if opts == nil {
		return params
	}
	if opts.Kind != "" {
		params.Kind = &opts.Kind
	}
	if opts.IncludeDisabled {
		params.IncludeDisabled = &opts.IncludeDisabled
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	return params
}

func listRoleAssignmentsParams(opts *ListRoleAssignmentsOptions) *api.ListRoleAssignmentsParams {
	params := &api.ListRoleAssignmentsParams{}
	if opts == nil {
		return params
	}
	if opts.PrincipalID != "" {
		params.PrincipalId = &opts.PrincipalID
	}
	if opts.RoleID != "" {
		params.RoleId = &opts.RoleID
	}
	return params
}

func listRolesParams(opts *ListRolesOptions) *api.ListRolesParams {
	params := &api.ListRolesParams{}
	if opts == nil {
		return params
	}
	if opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	if opts.Limit > 0 {
		params.Limit = &opts.Limit
	}
	return params
}

func unexpectedResourceStatus(op string, response *http.Response, body []byte) error {
	if response == nil {
		return fmt.Errorf("mobius: %s: missing HTTP response", op)
	}
	return unexpectedAPIStatus(op, response.StatusCode, response.Status, response.Header, body)
}
