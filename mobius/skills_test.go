package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

func skillJSON(id, source string) string {
	return fmt.Sprintf(`{
		"id":%q,"name":"Pull request review","source":%q,
		"instructions":"Check the diff and leave concise findings.",
		"allowed_tools":["github.create_review_comment"],
		"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"
	}`, id, source)
}

func TestSkillProjectLifecycleRoutes(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/test-project/skills":
			if got := r.URL.Query().Get("include_system"); got != "false" {
				t.Fatalf("include_system = %q, want false", got)
			}
			writeJSON(w, http.StatusOK, `{"items":[`+skillJSON("skill_1", "project")+`]}`)
		case "POST /v1/projects/test-project/skills":
			var req api.SkillRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name != "Pull request review" {
				t.Fatalf("create body = %#v (%v)", req, err)
			}
			writeJSON(w, http.StatusCreated, skillJSON("skill_1", "project"))
		case "GET /v1/projects/test-project/skills/skill_1":
			writeJSON(w, http.StatusOK, skillJSON("skill_1", "project"))
		case "PUT /v1/projects/test-project/skills/skill_1":
			var req api.SkillRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Instructions == "" {
				t.Fatalf("update must send the full body, got %#v (%v)", req, err)
			}
			writeJSON(w, http.StatusOK, skillJSON("skill_1", "project"))
		case "DELETE /v1/projects/test-project/skills/skill_1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	page, err := c.ListSkills(ctx, &ListSkillsOptions{ExcludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "skill_1" {
		t.Fatalf("items = %#v", page.Items)
	}
	req := api.SkillRequest{Name: "Pull request review", Instructions: "Check the diff and leave concise findings."}
	if _, err := c.CreateSkill(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSkill(ctx, "skill_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateSkill(ctx, "skill_1", req); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSkill(ctx, "skill_1"); err != nil {
		t.Fatal(err)
	}
}

func TestImportSkillSendsDocumentVerbatim(t *testing.T) {
	doc := "---\nallowed_tools:\n  - github.create_review_comment\n---\nCheck the diff and leave concise findings.\n"
	var got api.ImportSkillRequest
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/test-project/skills/import" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, skillJSON("skill_1", "project"))
	}))

	skill, err := c.ImportSkill(context.Background(), doc, "Pull request review")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != doc {
		t.Fatalf("content was not sent verbatim: %q", got.Content)
	}
	if got.Name == nil || *got.Name != "Pull request review" {
		t.Fatalf("name override = %v", got.Name)
	}
	if skill.Source != api.SkillSourceProject {
		t.Fatalf("source = %q", skill.Source)
	}
}

func TestImportSkillOmitsEmptyNameOverride(t *testing.T) {
	var raw map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, skillJSON("skill_1", "project"))
	}))

	if _, err := c.ImportSkill(context.Background(), "Just instructions.", ""); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["name"]; present {
		t.Fatalf("empty name must be omitted, body = %#v", raw)
	}
}

func TestOrganizationSkillRoutesAndProvenance(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/organization/skills":
			writeJSON(w, http.StatusOK, `{"items":[`+skillJSON("skill_org", "organization")+`]}`)
		case "POST /v1/organization/skills/import":
			writeJSON(w, http.StatusCreated, skillJSON("skill_org", "organization"))
		case "PUT /v1/organization/skills/skill_org":
			writeJSON(w, http.StatusOK, skillJSON("skill_org", "organization"))
		case "GET /v1/organization/skills/skill_org/usage":
			writeJSON(w, http.StatusOK, `{
				"skill_id":"skill_org","assignment_count":3,"project_count":2,
				"projects":[{"project_id":"proj_a","agent_count":2},{"project_id":"proj_b","agent_count":1}]
			}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	page, err := c.ListOrganizationSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Source != api.SkillSourceOrganization {
		t.Fatalf("organization provenance lost: %#v", page.Items)
	}
	if _, err := c.ImportOrganizationSkill(ctx, "doc", ""); err != nil {
		t.Fatal(err)
	}
	req := api.SkillRequest{Name: "Pull request review", Instructions: "Check the diff."}
	if _, err := c.ReplaceOrganizationSkill(ctx, "skill_org", req); err != nil {
		t.Fatal(err)
	}
	usage, err := c.GetOrganizationSkillUsage(ctx, "skill_org")
	if err != nil {
		t.Fatal(err)
	}
	if usage.AssignmentCount != 3 || len(usage.Projects) != 2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestDeleteOrganizationSkillSurfacesInUseConflict(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, `{"error":{"code":"skill_in_use","message":"detach agents first"}}`)
	}))

	err := c.DeleteOrganizationSkill(context.Background(), "skill_org")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Code != "skill_in_use" {
		t.Fatalf("apiErr = %#v", apiErr)
	}
}

func TestReplaceAgentSkillAssignmentsPreservesOrder(t *testing.T) {
	var got api.ReplaceSkillsRequest
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/test-project/agents/agent_1/skill-assignments":
			writeJSON(w, http.StatusOK, `{"items":[{"agent_id":"agent_1","skill_id":"skill_2","enabled":true,"position":0,"created_at":"2026-07-17T00:00:00Z"}]}`)
		case "PUT /v1/projects/test-project/agents/agent_1/skill-assignments":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			writeJSON(w, http.StatusOK, `{"items":[
				{"agent_id":"agent_1","skill_id":"skill_2","enabled":true,"position":0,"created_at":"2026-07-17T00:00:00Z"},
				{"agent_id":"agent_1","skill_id":"skill_1","enabled":true,"position":1,"created_at":"2026-07-17T00:00:00Z"}
			]}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	if _, err := c.ListAgentSkillAssignments(ctx, "agent_1"); err != nil {
		t.Fatal(err)
	}
	page, err := c.ReplaceAgentSkillAssignments(ctx, "agent_1", []string{"skill_2", "skill_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SkillIds) != 2 || got.SkillIds[0] != "skill_2" || got.SkillIds[1] != "skill_1" {
		t.Fatalf("skill_ids order not preserved: %#v", got.SkillIds)
	}
	if len(page.Items) != 2 || page.Items[0].Position != 0 {
		t.Fatalf("items = %#v", page.Items)
	}
}

func TestReplaceAgentSkillAssignmentsSendsEmptySet(t *testing.T) {
	var raw map[string]json.RawMessage
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusOK, `{"items":[]}`)
	}))

	if _, err := c.ReplaceAgentSkillAssignments(context.Background(), "agent_1", nil); err != nil {
		t.Fatal(err)
	}
	if string(raw["skill_ids"]) != "[]" {
		t.Fatalf("nil slice must serialize as an empty array, got %s", raw["skill_ids"])
	}
}
