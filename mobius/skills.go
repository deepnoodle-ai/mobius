package mobius

import (
	"context"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListSkillsOptions filters the project skill catalog.
type ListSkillsOptions struct {
	// ExcludeSystem omits the read-only system skill templates that the
	// server includes by default.
	ExcludeSystem bool
}

// ListSkills lists project-local, organization-shared, and system skills
// visible to the project. Organization skills are read-only through project
// mutation routes.
func (c *Client) ListSkills(ctx context.Context, opts *ListSkillsOptions) (*api.SkillListResponse, error) {
	params := &api.ListSkillsParams{}
	if opts != nil && opts.ExcludeSystem {
		includeSystem := false
		params.IncludeSystem = &includeSystem
	}
	resp, err := c.ac.ListSkillsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list skills: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list skills", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateSkill creates a project-local skill.
func (c *Client) CreateSkill(ctx context.Context, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.CreateSkillWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedProjectResourceStatus("create skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// ImportSkill imports a Claude Code or Dive-style skill document into a
// project-local skill. The content string is sent verbatim, including any
// YAML frontmatter; pass a non-empty name to override the document's own.
func (c *Client) ImportSkill(ctx context.Context, content, name string) (*api.Skill, error) {
	req := api.ImportSkillRequest{Content: content}
	if name != "" {
		req.Name = &name
	}
	resp, err := c.ac.ImportSkillWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: import skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedProjectResourceStatus("import skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// GetSkill returns a single project-local, organization-shared, or system
// skill by ID.
func (c *Client) GetSkill(ctx context.Context, skillID string) (*api.Skill, error) {
	resp, err := c.ac.GetSkillWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), skillID)
	if err != nil {
		return nil, fmt.Errorf("mobius: get skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// UpdateSkill replaces a project-local skill. The server requires the full
// body — including Name and Instructions — on every update; there is no
// partial patch, so read-modify-write is the caller's responsibility.
func (c *Client) UpdateSkill(ctx context.Context, skillID string, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.UpdateSkillWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), skillID, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: update skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("update skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteSkill deletes a project-local skill. The skill is automatically
// detached from any agents that reference it.
func (c *Client) DeleteSkill(ctx context.Context, skillID string) error {
	resp, err := c.ac.DeleteSkillWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), skillID)
	if err != nil {
		return fmt.Errorf("mobius: delete skill: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedProjectResourceStatus("delete skill", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// ListOrganizationSkills lists skills owned by the active organization. Any
// organization member may read this catalog.
func (c *Client) ListOrganizationSkills(ctx context.Context) (*api.SkillListResponse, error) {
	resp, err := c.ac.ListOrganizationSkillsWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("mobius: list organization skills: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list organization skills", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CreateOrganizationSkill creates a skill shared across the active
// organization. Requires Admin or Owner membership.
func (c *Client) CreateOrganizationSkill(ctx context.Context, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.CreateOrganizationSkillWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create organization skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedProjectResourceStatus("create organization skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// ImportOrganizationSkill imports a Claude Code or Dive-style skill document
// as an organization skill. The content string is sent verbatim, including
// any YAML frontmatter; pass a non-empty name to override the document's
// own. Requires Admin or Owner membership.
func (c *Client) ImportOrganizationSkill(ctx context.Context, content, name string) (*api.Skill, error) {
	req := api.ImportSkillRequest{Content: content}
	if name != "" {
		req.Name = &name
	}
	resp, err := c.ac.ImportOrganizationSkillWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: import organization skill: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedProjectResourceStatus("import organization skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON201, nil
}

// GetOrganizationSkill returns one skill owned by the active organization.
func (c *Client) GetOrganizationSkill(ctx context.Context, skillID string) (*api.Skill, error) {
	resp, err := c.ac.GetOrganizationSkillWithResponse(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("mobius: get organization skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get organization skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ReplaceOrganizationSkill replaces the shared skill for subsequent agent
// turns. The server requires the full body — including Name and Instructions
// — on every update; there is no partial patch, so read-modify-write is the
// caller's responsibility. Requires Admin or Owner membership.
func (c *Client) ReplaceOrganizationSkill(ctx context.Context, skillID string, req api.SkillRequest) (*api.Skill, error) {
	resp, err := c.ac.ReplaceOrganizationSkillWithResponse(ctx, skillID, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: replace organization skill: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("replace organization skill", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteOrganizationSkill deletes an unused organization skill. A skill that
// is still assigned returns a 409 skill_in_use [APIError]; inspect
// [Client.GetOrganizationSkillUsage] and remove the assignments first — the
// SDK never detaches agents implicitly.
func (c *Client) DeleteOrganizationSkill(ctx context.Context, skillID string) error {
	resp, err := c.ac.DeleteOrganizationSkillWithResponse(ctx, skillID)
	if err != nil {
		return fmt.Errorf("mobius: delete organization skill: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedProjectResourceStatus("delete organization skill", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// GetOrganizationSkillUsage reports an organization skill's assignment
// impact across projects, so callers can see what a replace or delete would
// touch. Requires Admin or Owner membership.
func (c *Client) GetOrganizationSkillUsage(ctx context.Context, skillID string) (*api.OrganizationSkillUsage, error) {
	resp, err := c.ac.GetOrganizationSkillUsageWithResponse(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("mobius: get organization skill usage: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get organization skill usage", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListAgentSkillAssignments returns the skills assigned to an agent in
// assignment order.
func (c *Client) ListAgentSkillAssignments(ctx context.Context, agentID string) (*api.SkillAssignmentListResponse, error) {
	resp, err := c.ac.ListAgentSkillAssignmentsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID))
	if err != nil {
		return nil, fmt.Errorf("mobius: list agent skill assignments: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list agent skill assignments", resp.HTTPResponse, resp.Body)
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
	resp, err := c.ac.ReplaceAgentSkillAssignmentsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: replace agent skill assignments: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("replace agent skill assignments", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}
