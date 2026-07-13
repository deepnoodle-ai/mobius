package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerPrincipalCreateCommand adds the ergonomic onboarding path that a
// generated one-request command cannot express: resolve a role name, create a
// role-bearing principal, then optionally mint its first key.
func registerPrincipalCreateCommand(app *cli.App) {
	grp := app.Group("principals").Description("Machine identities and their roles")
	grp.Command("create").
		Description("Create a service principal, optionally with its first key").
		AddArg(&cli.Arg{Name: "name", Description: "Human-readable principal name."}).
		Flags(
			cli.String("name", "").Help("Principal name (alternative to the positional name)."),
			cli.String("description", "").Help("Optional human-readable description."),
			cli.String("owner-id", "").Help("Human principal accountable for this service principal."),
			cli.String("metadata", "").Help("Arbitrary metadata. Accepts JSON, @file, or @-."),
			cli.Strings("role-ids", "").Help("Role IDs to assign atomically. Repeatable."),
			cli.String("role", "").Help("Project role name to resolve and assign atomically."),
			cli.Strings("tag", "").Help("Tag in KEY=VALUE form. Repeatable."),
			cli.Bool("with-key", "").Help("Mint the principal's first project API key."),
			cli.String("key-name", "").Help("Key name. Defaults to <principal>-primary."),
			cli.String("expires-at", "").Help("Optional RFC3339 key expiry."),
			cli.Bool("allow-unassigned-principal", "").Help("Allow a dormant key when no role is assigned."),
			cli.String("file", "f").Help("Principal request body from a file (JSON or YAML, '-' for stdin)."),
			cli.Bool("dry-run", "").Help("Print the assembled principal request and exit."),
		).
		Use(requireAuth()).
		Run(runPrincipalCreate)
}

func runPrincipalCreate(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	project := authFor(ctx).Project
	var body api.CreatePrincipalJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	name := ctx.String("name")
	if name == "" {
		name = ctx.Arg(0)
	}
	if name != "" {
		body.Name = name
	}
	if body.Name == "" {
		return fmt.Errorf("principal name is required (positional, --name, or --file)")
	}
	if ctx.IsSet("description") {
		v := ctx.String("description")
		body.Description = &v
	}
	if ctx.IsSet("owner-id") {
		v := ctx.String("owner-id")
		body.OwnerId = &v
	}
	if ctx.IsSet("metadata") {
		if err := decodeFlagJSON(ctx, "metadata", ctx.String("metadata"), &body.Metadata); err != nil {
			return err
		}
	}
	if ctx.IsSet("role-ids") {
		v := ctx.Strings("role-ids")
		body.RoleIds = &v
	}
	if roleName := ctx.String("role"); roleName != "" {
		roleID, err := resolveRoleName(ctx, client, project, roleName)
		if err != nil {
			return err
		}
		roleIDs := []string{}
		if body.RoleIds != nil {
			roleIDs = append(roleIDs, (*body.RoleIds)...)
		}
		roleIDs = append(roleIDs, roleID)
		body.RoleIds = &roleIDs
	}
	if tags, err := parseTagFlags(ctx); err != nil {
		return err
	} else if tags != nil {
		v := api.TagMap(tags)
		body.Tags = &v
	}
	if ctx.Bool("dry-run") {
		return printDryRun(ctx, body)
	}

	principalResp, err := client.CreatePrincipalWithResponse(ctx.Context(), project, body)
	if err != nil {
		return err
	}
	if principalResp.JSON201 == nil {
		return printResponse(ctx, "createPrincipal", principalResp.StatusCode(), principalResp.Body)
	}
	if !ctx.Bool("with-key") {
		return printResponse(ctx, "createPrincipal", principalResp.StatusCode(), principalResp.Body)
	}

	keyName := ctx.String("key-name")
	if keyName == "" {
		keyName = body.Name + "-primary"
	}
	keyBody := api.CreateAPIKeyJSONRequestBody{
		Name:        keyName,
		PrincipalId: principalResp.JSON201.Id,
	}
	if ctx.Bool("allow-unassigned-principal") {
		v := true
		keyBody.AllowUnassignedPrincipal = &v
	}
	if raw := ctx.String("expires-at"); raw != "" {
		expiresAt, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return fmt.Errorf("--expires-at must be RFC3339: %w", err)
		}
		keyBody.ExpiresAt = &expiresAt
	}
	keyResp, err := client.CreateAPIKeyWithResponse(ctx.Context(), project, keyBody)
	if err != nil {
		return fmt.Errorf("principal %s was created, but key creation failed: %w", principalResp.JSON201.Id, err)
	}
	if keyResp.JSON201 == nil {
		return fmt.Errorf("principal %s was created, but key creation failed (%s): %s", principalResp.JSON201.Id, keyResp.Status(), string(keyResp.Body))
	}
	result := struct {
		Principal *api.Principal          `json:"principal"`
		Key       *api.APIKeyCreateResult `json:"key"`
	}{Principal: principalResp.JSON201, Key: keyResp.JSON201}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return printResponse(ctx, "createPrincipalWithKey", http.StatusCreated, raw)
}

func resolveRoleName(ctx *cli.Context, client *api.ClientWithResponses, project, name string) (string, error) {
	limit := api.LimitParam(100)
	resp, err := client.ListRolesWithResponse(ctx.Context(), project, &api.ListRolesParams{Limit: &limit})
	if err != nil {
		return "", err
	}
	if resp.JSON200 == nil {
		return "", printResponse(ctx, "listRoles", resp.StatusCode(), resp.Body)
	}
	for _, role := range resp.JSON200.Items {
		if role.Name == name {
			return role.Id, nil
		}
	}
	return "", fmt.Errorf("role %q not found in project %q", name, project)
}
