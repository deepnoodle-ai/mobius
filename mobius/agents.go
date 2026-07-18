package mobius

import (
	"context"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// CreateAgentOptions configures [Client.CreateAgent].
type CreateAgentOptions struct {
	// Agent is the definition to create: name (required), model, system
	// prompt, and the rest of the create contract. Its ExternalRef and
	// IfExists fields are managed by the options below — set ExternalRef
	// here, not on the request.
	Agent api.CreateAgentRequest

	// AdoptExisting makes the call safely retryable: when a live agent
	// already carries ExternalRef, it is returned unchanged instead of
	// erroring with 409, and no fields are written. Requires ExternalRef.
	// Conflicts that cannot be adopted surface as [APIError] with
	// [ErrCodeExternalIdentityConflict].
	AdoptExisting bool

	// ExternalRef is the client-owned durable identity key for this agent,
	// unique within the project and assign-once. Required when AdoptExisting
	// is set; optional otherwise.
	ExternalRef string
}

// CreateAgent creates an agent, or — with [CreateAgentOptions.AdoptExisting]
// — adopts the live agent that already carries the same ExternalRef.
// Adopt-mode calls are retried on transient failures like idempotent
// requests, because external_ref makes the create replay-safe server-side.
func (c *Client) CreateAgent(ctx context.Context, opts CreateAgentOptions) (*api.Agent, error) {
	req := opts.Agent
	if opts.ExternalRef != "" {
		ref := opts.ExternalRef
		req.ExternalRef = &ref
	}
	if opts.AdoptExisting {
		if req.ExternalRef == nil || *req.ExternalRef == "" {
			return nil, fmt.Errorf("mobius: create agent: AdoptExisting requires ExternalRef to be set")
		}
		adopt := api.IfExistsAdopt
		req.IfExists = &adopt
		ctx = contextWithReplaySafe(ctx)
	}
	resp, err := c.ac.CreateAgentWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create agent: %w", err)
	}
	// 201 is a fresh create; 200 is the existing agent adopted by ExternalRef.
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	return nil, unexpectedProjectResourceStatus("create agent", resp.HTTPResponse, resp.Body)
}

// ListAgentsOptions filters [Client.ListAgents].
type ListAgentsOptions struct {
	// Name filters to the project-unique agent with this exact name.
	Name string
	// PrincipalID filters to the agent backed by this principal.
	PrincipalID string
	// Status filters by administrative status (active/inactive),
	// independent of presence.
	Status api.AgentStatus
	// Limit caps the number of items returned.
	Limit int
}

// ListAgents returns active and inactive agents with computed presence;
// deleted agents are excluded. Filter by exact name or principal ID to
// resolve a configured agent without copying its Mobius ID into
// application state.
func (c *Client) ListAgents(ctx context.Context, opts *ListAgentsOptions) (*api.AgentListResponse, error) {
	params := &api.ListAgentsParams{}
	if opts != nil {
		if opts.Name != "" {
			params.Name = &opts.Name
		}
		if opts.PrincipalID != "" {
			params.PrincipalId = &opts.PrincipalID
		}
		if opts.Status != "" {
			status := opts.Status
			params.Status = &status
		}
		if opts.Limit > 0 {
			limit := api.LimitParam(opts.Limit)
			params.Limit = &limit
		}
	}
	resp, err := c.ac.ListAgentsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list agents: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list agents", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// GetAgent returns one agent by ID.
func (c *Client) GetAgent(ctx context.Context, agentID string) (*api.Agent, error) {
	resp, err := c.ac.GetAgentWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get agent: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get agent", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateAgent patches mutable agent fields. The agent's backing identity
// (principal_id) is immutable; reassigning identity is delete-and-recreate.
func (c *Client) UpdateAgent(ctx context.Context, agentID string, req api.UpdateAgentRequest) (*api.Agent, error) {
	resp, err := c.ac.UpdateAgentWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update agent: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("update agent", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteAgent soft-deletes an agent. A deleted agent still owns its
// external_ref: it is never resurrected or replaced, so a later
// create-or-adopt against the same ExternalRef returns 409 even with
// AdoptExisting.
func (c *Client) DeleteAgent(ctx context.Context, agentID string) error {
	resp, err := c.ac.DeleteAgentWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID))
	if err != nil {
		return fmt.Errorf("mobius: delete agent: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedProjectResourceStatus("delete agent", resp.HTTPResponse, resp.Body)
	}
	return nil
}
