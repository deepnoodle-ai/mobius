package mobius

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// GetOAuthReturnOrigins returns the active organization's allowlist of exact
// HTTPS origins an embedded partner may name as an OAuth connect return_url.
// An empty list means embedded return is disabled. Requires Admin or Owner
// membership.
func (c *Client) GetOAuthReturnOrigins(ctx context.Context) (*api.OAuthReturnOrigins, error) {
	resp, err := c.ac.GetOAuthReturnOriginsWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("mobius: get oauth return origins: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get oauth return origins", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ReplaceOAuthReturnOrigins full-replaces the organization's OAuth
// return-origin allowlist. The server normalizes each entry (lowercase host,
// default ports stripped) and rejects invalid origins; no validation happens
// client-side. Pass an empty (or nil) slice to disable embedded return.
// Requires Admin or Owner membership.
func (c *Client) ReplaceOAuthReturnOrigins(ctx context.Context, origins []string) (*api.OAuthReturnOrigins, error) {
	if origins == nil {
		origins = []string{}
	}
	resp, err := c.ac.ReplaceOAuthReturnOriginsWithResponse(ctx, api.PutOAuthReturnOriginsRequest{Origins: origins})
	if err != nil {
		return nil, fmt.Errorf("mobius: replace oauth return origins: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("replace oauth return origins", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}
