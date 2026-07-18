package mobius

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// CreateProjectOptions configures [Client.CreateProject].
type CreateProjectOptions struct {
	// Project is the definition to create: name (required), handle,
	// description, access mode, and tags. Its ExternalRef and IfExists
	// fields are managed by the options below — set ExternalRef here, not
	// on the request.
	Project api.CreateProjectRequest

	// AdoptExisting makes the call safely retryable: when an active project
	// already carries ExternalRef, it is returned unchanged instead of
	// erroring with 409, and no fields are written. Requires ExternalRef.
	// Conflicts that cannot be adopted surface as [APIError] with
	// [ErrCodeExternalIdentityConflict] (handle mismatch or deleted match)
	// or [ErrCodeProjectArchived] (archived match).
	AdoptExisting bool

	// ExternalRef is the client-owned tenant/workspace correlation key,
	// unique within the org and assign-once. Required when AdoptExisting is
	// set; optional otherwise.
	ExternalRef string
}

// CreateProject creates a project within the authenticated org, or — with
// [CreateProjectOptions.AdoptExisting] — adopts the active project that
// already carries the same ExternalRef. Adopt-mode calls are retried on
// transient failures like idempotent requests, because external_ref makes
// the create replay-safe server-side. A new project can be rejected with
// [ErrCodeProjectCapacityReached] (429) at the org's project limit; an
// existing ExternalRef match still adopts even at that limit.
func (c *Client) CreateProject(ctx context.Context, opts CreateProjectOptions) (*api.Project, error) {
	req := opts.Project
	if opts.ExternalRef != "" {
		ref := opts.ExternalRef
		req.ExternalRef = &ref
	}
	if opts.AdoptExisting {
		if req.ExternalRef == nil || *req.ExternalRef == "" {
			return nil, fmt.Errorf("mobius: create project: AdoptExisting requires ExternalRef to be set")
		}
		adopt := api.IfExistsAdopt
		req.IfExists = &adopt
		ctx = contextWithReplaySafe(ctx)
	}
	resp, err := c.ac.CreateProjectWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create project: %w", err)
	}
	// 201 is a fresh create; 200 is the existing project adopted by ExternalRef.
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	return nil, unexpectedProjectResourceStatus("create project", resp.HTTPResponse, resp.Body)
}
