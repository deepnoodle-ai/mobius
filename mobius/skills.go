package mobius

import (
	"context"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListSkillsOptions filters the org's skill catalog.
type ListSkillsOptions struct {
	// ExcludeSystem omits the read-only system skill templates that the
	// server includes by default.
	ExcludeSystem bool
}

// ListSkills lists custom and system skills visible to the caller.
func (c *Client) ListSkills(ctx context.Context, opts *ListSkillsOptions) (*api.SkillListResponse, error) {
	params := &api.ListSkillsParams{}
	if opts != nil && opts.ExcludeSystem {
		includeSystem := false
		params.IncludeSystem = &includeSystem
	}
	resp, err := c.ac.ListSkillsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list skills: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list skills", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateSkill creates an org-owned skill.
func (c *Client) CreateSkill(ctx context.Context, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.CreateSkillWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedResourceStatus("create skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// ImportSkill imports a Claude Code or Dive-style skill document into an
// org-owned skill. The content string is sent verbatim, including any
// YAML frontmatter; pass a non-empty name to override the document's own.
func (c *Client) ImportSkill(ctx context.Context, content, name string) (*api.Skill, error) {
	req := api.ImportSkillRequest{Content: content}
	if name != "" {
		req.Name = &name
	}
	resp, err := c.ac.ImportSkillWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: import skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedResourceStatus("import skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// GetSkill returns a single org-owned, organization-shared, or system
// skill by ID.
func (c *Client) GetSkill(ctx context.Context, skillID string) (*api.Skill, error) {
	resp, err := c.ac.GetSkillWithResponse(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("mobius: get skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("get skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateSkill replaces an org-owned skill. The server requires the full
// body — including Name and Instructions — on every update; there is no
// partial patch, so read-modify-write is the caller's responsibility.
func (c *Client) UpdateSkill(ctx context.Context, skillID string, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.UpdateSkillWithResponse(ctx, skillID, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("update skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteSkill deletes an org-owned skill. The skill is automatically
// detached from any agents that reference it.
func (c *Client) DeleteSkill(ctx context.Context, skillID string) error {
	resp, err := c.ac.DeleteSkillWithResponse(ctx, skillID)
	if err != nil {
		return fmt.Errorf("mobius: delete skill: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedResourceStatus("delete skill", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// ListAgentSkillAssignments returns the skills assigned to an agent in
// assignment order.
func (c *Client) ListAgentSkillAssignments(ctx context.Context, agentID string) (*api.SkillAssignmentListResponse, error) {
	resp, err := c.ac.ListAgentSkillAssignmentsWithResponse(ctx, api.IDParam(agentID))
	if err != nil {
		return nil, fmt.Errorf("mobius: list agent skill assignments: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("list agent skill assignments", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ReplaceAgentSkillAssignments replaces the agent's skill assignment set as
// a whole with skillIDs, in the given order. Pass an empty slice to remove
// every assignment.
func (c *Client) ReplaceAgentSkillAssignments(ctx context.Context, agentID string, skillIDs []string) (*api.SkillAssignmentListResponse, error) {
	if skillIDs == nil {
		skillIDs = []string{}
	}
	req := api.ReplaceSkillsRequest{SkillIds: skillIDs}
	resp, err := c.ac.ReplaceAgentSkillAssignmentsWithResponse(ctx, api.IDParam(agentID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: replace agent skill assignments: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedResourceStatus("replace agent skill assignments", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}
