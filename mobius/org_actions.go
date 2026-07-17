package mobius

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListOrganizationActionsOptions paginates organization-owned signed HTTP
// actions.
type ListOrganizationActionsOptions struct {
	Cursor string
	Limit  int
}

// ActivateOrganizationActionSecretVersionOptions configures activation of a
// pending signing-secret version. A nil options value keeps the server
// default verification overlap (24 hours) for the previously active version.
type ActivateOrganizationActionSecretVersionOptions struct {
	// OverlapSeconds bounds how long the previous active version keeps
	// verifying after cutover, from 0 (immediate) to 86400.
	OverlapSeconds int
}

// OrganizationActionSecretMaterial carries the one-time signing secret
// revealed by [Client.CreateOrganizationAction] and
// [Client.RotateOrganizationActionSecret]. The server never returns this
// key again; store it before discarding the value. KeyBytes is the decoded
// signing key ready for signature verification — do not log it.
type OrganizationActionSecretMaterial struct {
	// Action is the created or updated action. Its SigningSecret field is
	// cleared; the revealed key lives only in KeyBytes.
	Action api.OrganizationAction
	// SecretRef is the stable reference sent in X-Mobius-Secret-Ref on
	// signed deliveries for this action.
	SecretRef string
	// Version is the key version the revealed secret belongs to: the active
	// version after create, the pending version after rotate.
	Version int64
	// KeyBytes is the base64-decoded signing key.
	KeyBytes []byte
}

// ListOrganizationActions lists signed HTTP actions owned by the active
// organization. Requires Admin or Owner membership.
func (c *Client) ListOrganizationActions(ctx context.Context, opts *ListOrganizationActionsOptions) (*api.OrganizationActionListResponse, error) {
	params := &api.ListOrganizationActionsParams{}
	if opts != nil {
		if opts.Cursor != "" {
			cursor := api.CursorParam(opts.Cursor)
			params.Cursor = &cursor
		}
		if opts.Limit > 0 {
			limit := api.LimitParam(opts.Limit)
			params.Limit = &limit
		}
	}
	resp, err := c.ac.ListOrganizationActionsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list organization actions: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list organization actions", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateOrganizationAction creates an organization-owned signed HTTP action
// and returns its one-time secret material. The signing key is revealed only
// in this response; persist [OrganizationActionSecretMaterial.KeyBytes]
// before discarding the result.
func (c *Client) CreateOrganizationAction(ctx context.Context, req api.CreateOrganizationActionRequest) (*OrganizationActionSecretMaterial, error) {
	resp, err := c.ac.CreateOrganizationActionWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create organization action: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedProjectResourceStatus("create organization action", resp.HTTPResponse, resp.Body)
	}
	return organizationActionSecretMaterial("create organization action", resp.JSON201, api.OrganizationActionSecretVersionStatusActive)
}

// GetOrganizationAction returns one organization action. Reads never include
// secret material.
func (c *Client) GetOrganizationAction(ctx context.Context, actionID string) (*api.OrganizationAction, error) {
	resp, err := c.ac.GetOrganizationActionWithResponse(ctx, actionID)
	if err != nil {
		return nil, fmt.Errorf("mobius: get organization action: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get organization action", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateOrganizationAction updates the shared definition or enables/disables
// invocation.
func (c *Client) UpdateOrganizationAction(ctx context.Context, actionID string, req api.UpdateOrganizationActionRequest) (*api.OrganizationAction, error) {
	resp, err := c.ac.UpdateOrganizationActionWithResponse(ctx, actionID, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update organization action: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("update organization action", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteOrganizationAction deletes the shared definition from future project
// catalogs.
func (c *Client) DeleteOrganizationAction(ctx context.Context, actionID string) error {
	resp, err := c.ac.DeleteOrganizationActionWithResponse(ctx, actionID)
	if err != nil {
		return fmt.Errorf("mobius: delete organization action: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedProjectResourceStatus("delete organization action", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// RotateOrganizationActionSecret creates a pending key version and returns
// its one-time secret material. Mobius keeps signing with the current active
// version until [Client.ActivateOrganizationActionSecretVersion] promotes the
// pending one — distribute the new key to verifiers first, then activate.
func (c *Client) RotateOrganizationActionSecret(ctx context.Context, actionID string) (*OrganizationActionSecretMaterial, error) {
	resp, err := c.ac.RotateOrganizationActionSecretWithResponse(ctx, actionID)
	if err != nil {
		return nil, fmt.Errorf("mobius: rotate organization action secret: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("rotate organization action secret", resp.HTTPResponse, resp.Body)
	}
	return organizationActionSecretMaterial("rotate organization action secret", resp.JSON200, api.OrganizationActionSecretVersionStatusPending)
}

// ActivateOrganizationActionSecretVersion atomically makes a pending version
// active and moves the previous active version, if any, into its bounded
// verification overlap.
func (c *Client) ActivateOrganizationActionSecretVersion(ctx context.Context, actionID string, version int64, opts *ActivateOrganizationActionSecretVersionOptions) (*api.OrganizationAction, error) {
	body := api.ActivateOrganizationActionSecretVersionJSONRequestBody{}
	if opts != nil {
		if opts.OverlapSeconds < 0 || opts.OverlapSeconds > 86400 {
			return nil, fmt.Errorf("mobius: activate organization action secret version: OverlapSeconds must be between 0 and 86400, got %d", opts.OverlapSeconds)
		}
		body.OverlapSeconds = &opts.OverlapSeconds
	}
	resp, err := c.ac.ActivateOrganizationActionSecretVersionWithResponse(ctx, actionID, version, body)
	if err != nil {
		return nil, fmt.Errorf("mobius: activate organization action secret version: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("activate organization action secret version", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// RevokeOrganizationActionSecretVersion immediately revokes a non-active key
// version. The active signing version can be revoked only after another
// version is activated or the action is disabled.
func (c *Client) RevokeOrganizationActionSecretVersion(ctx context.Context, actionID string, version int64) (*api.OrganizationAction, error) {
	resp, err := c.ac.RevokeOrganizationActionSecretVersionWithResponse(ctx, actionID, version)
	if err != nil {
		return nil, fmt.Errorf("mobius: revoke organization action secret version: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("revoke organization action secret version", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// organizationActionSecretMaterial extracts the one-time secret from a create
// or rotate response. The revealed signing_secret always belongs to the
// newest entry in secret_versions, whose status must match wantStatus; any
// other shape means the response is internally inconsistent, and the error
// never includes the secret itself.
func organizationActionSecretMaterial(op string, action *api.OrganizationAction, wantStatus api.OrganizationActionSecretVersionStatus) (*OrganizationActionSecretMaterial, error) {
	if action.SigningSecret == nil || *action.SigningSecret == "" {
		return nil, fmt.Errorf("mobius: %s: response is missing the one-time signing_secret", op)
	}
	var newest *api.OrganizationActionSecretVersion
	for i := range action.SecretVersions {
		v := &action.SecretVersions[i]
		if newest == nil || v.Version > newest.Version {
			newest = v
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("mobius: %s: response has no secret_versions for the revealed secret", op)
	}
	if newest.Status != wantStatus {
		return nil, fmt.Errorf("mobius: %s: newest secret version %d has status %q, want %q", op, newest.Version, newest.Status, wantStatus)
	}
	keyBytes, err := base64.StdEncoding.DecodeString(*action.SigningSecret)
	if err != nil {
		return nil, fmt.Errorf("mobius: %s: signing_secret is not valid base64", op)
	}
	material := &OrganizationActionSecretMaterial{
		Action:    *action,
		SecretRef: action.SecretRef,
		Version:   newest.Version,
		KeyBytes:  keyBytes,
	}
	material.Action.SigningSecret = nil
	return material, nil
}
