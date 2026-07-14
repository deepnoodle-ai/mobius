package mobius

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestProjectResourceListHelpers(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/projects/test-project/blueprints/bindings":
			assert.Equal(t, r.URL.Query().Get("namespace"), "starter")
			assert.Equal(t, r.URL.Query().Get("blueprint_key"), "support")
			_, _ = io.WriteString(w, `{"items":[]}`)
		case "/v1/projects/test-project/interactions":
			assert.Equal(t, r.URL.Query().Get("session_id"), "sess_1")
			assert.Equal(t, r.URL.Query().Get("status"), "pending")
			assert.Equal(t, r.URL.Query().Get("inbox"), "true")
			_, _ = io.WriteString(w, `{"items":[],"has_more":false}`)
		case "/v1/projects/test-project/permissions":
			_, _ = io.WriteString(w, `{"items":[],"presets":[],"action_groups":[]}`)
		case "/v1/projects/test-project/principals":
			assert.Equal(t, r.URL.Query().Get("kind"), "service")
			assert.Equal(t, r.URL.Query().Get("include_disabled"), "true")
			_, _ = io.WriteString(w, `{"items":[]}`)
		case "/v1/projects/test-project/roles":
			assert.Equal(t, r.URL.Query().Get("cursor"), "role_cursor")
			_, _ = io.WriteString(w, `{"items":[],"has_more":false}`)
		case "/v1/projects/test-project/role-assignments":
			assert.Equal(t, r.URL.Query().Get("principal_id"), "principal_1")
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	c, srv := newTestClient(t, h)
	defer srv.Close()
	ctx := context.Background()

	bindings, err := c.ListBlueprintBindings(ctx, &ListBlueprintBindingsOptions{Namespace: "starter", BlueprintKey: "support"})
	assert.NoError(t, err)
	assert.Equal(t, len(bindings.Items), 0)

	interactions, err := c.ListInteractions(ctx, &ListInteractionsOptions{
		Status:    api.ListInteractionsParamsStatus("pending"),
		SessionID: "sess_1",
		Inbox:     true,
	})
	assert.NoError(t, err)
	assert.Equal(t, len(interactions.Items), 0)

	permissions, err := c.ListProjectPermissions(ctx)
	assert.NoError(t, err)
	assert.Equal(t, len(permissions.Items), 0)

	principals, err := c.ListPrincipals(ctx, &ListPrincipalsOptions{
		Kind:            api.PrincipalKind("service"),
		IncludeDisabled: true,
	})
	assert.NoError(t, err)
	assert.Equal(t, len(principals.Items), 0)

	roles, err := c.ListRoles(ctx, &ListRolesOptions{Cursor: "role_cursor"})
	assert.NoError(t, err)
	assert.Equal(t, len(roles.Items), 0)

	assignments, err := c.ListRoleAssignments(ctx, &ListRoleAssignmentsOptions{PrincipalID: "principal_1"})
	assert.NoError(t, err)
	assert.Equal(t, len(assignments.Items), 0)
}
