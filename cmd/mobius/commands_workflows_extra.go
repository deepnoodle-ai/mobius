package main

import (
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerWorkflowsExtras layers hand-written commands on top of the
// generated `workflows` group: `apply` (upsert by handle) and `init`
// (scaffold a starter spec).
func registerWorkflowsExtras(app *cli.App) {
	grp := app.Group("workflows")

	grp.Command("apply").
		Description("Create or update a workflow by handle (idempotent upsert).").
		Flags(
			cli.String("description", "").Help("Workflow description."),
			cli.String("handle", "").Help("URL-safe handle. Determines which existing workflow is updated."),
			cli.String("name", "").Help("Human-readable workflow name."),
			cli.Bool("published-as-tool", "").Help("Expose this workflow as a callable tool."),
			cli.String("spec", "").Help("Workflow definition. Accepts JSON, @file, or @-."),
			cli.Strings("tag", "").Help("Tag in KEY=VALUE form. Repeatable."),
			cli.String("id", "").Help("Skip the handle lookup and update this workflow ID directly."),
			cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin)."),
			cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
		).
		Use(requireAuth()).
		Run(workflowsApplyHandler)

	grp.Command("init").
		Description("Print a starter workflow spec to stdout.").
		Flags(
			cli.String("name", "").Help("Workflow name embedded in the scaffold."),
			cli.String("format", "").Default("yaml").Enum("yaml", "json").Help("Output format."),
		).
		Run(workflowsInitHandler)
}

func workflowsApplyHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	project := authFor(ctx).Project

	// Build a CreateWorkflowRequest from --file + per-field flags. The same
	// shape carries everything we need — for an update we drop the Handle
	// (which is immutable) when projecting onto UpdateWorkflowRequest.
	var body api.CreateWorkflowRequest
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	if ctx.IsSet("description") {
		v := ctx.String("description")
		body.Description = &v
	}
	if ctx.IsSet("handle") {
		v := ctx.String("handle")
		body.Handle = &v
	}
	if ctx.IsSet("name") {
		body.Name = ctx.String("name")
	}
	if ctx.IsSet("published-as-tool") {
		v := ctx.Bool("published-as-tool")
		body.PublishedAsTool = &v
	}
	if ctx.IsSet("spec") {
		if err := decodeFlagJSON(ctx, "spec", ctx.String("spec"), &body.Spec); err != nil {
			return err
		}
	}
	if tags, err := parseTagFlags(ctx); err != nil {
		return err
	} else if tags != nil {
		v := api.TagMap(tags)
		body.Tags = &v
	}

	if body.Name == "" {
		return cli.Errorf("--name is required (or supply it via --file)")
	}
	// Spec required; an empty WorkflowSpec has Name == "" and no Steps.
	if body.Spec.Name == "" && len(body.Spec.Steps) == 0 {
		return cli.Errorf("--spec is required (or supply it via --file)")
	}

	// Decide create vs update: prefer --id, then handle resolution.
	id := ctx.String("id")
	if id == "" {
		handle := ""
		if body.Handle != nil {
			handle = *body.Handle
		}
		if handle == "" {
			return cli.Errorf("--handle is required for apply (or set 'handle' in --file). Pass --id to update by ID instead.")
		}
		found, err := findWorkflowIDByHandle(ctx, client, project, handle)
		if err != nil {
			return err
		}
		id = found
	}

	if id == "" {
		// Create
		if ctx.Bool("dry-run") {
			return printDryRun(ctx, body)
		}
		resp, err := client.CreateWorkflowWithResponse(ctx.Context(), project, body)
		if err != nil {
			return err
		}
		return printResponse(ctx, "createWorkflow", resp.StatusCode(), resp.Body)
	}

	// Update — project the create request onto the update request.
	update := api.UpdateWorkflowRequest{
		Description:     body.Description,
		PublishedAsTool: body.PublishedAsTool,
		Spec:            &body.Spec,
		Tags:            body.Tags,
	}
	if body.Name != "" {
		n := body.Name
		update.Name = &n
	}
	if ctx.Bool("dry-run") {
		return printDryRun(ctx, update)
	}
	resp, err := client.UpdateWorkflowWithResponse(ctx.Context(), project, id, update)
	if err != nil {
		return err
	}
	return printResponse(ctx, "updateWorkflow", resp.StatusCode(), resp.Body)
}

// findWorkflowIDByHandle walks the paginated workflow list looking for a
// workflow whose handle matches. Returns "" with no error when not found,
// signalling the caller to fall through to create.
func findWorkflowIDByHandle(ctx *cli.Context, client api.ClientWithResponsesInterface, project, handle string) (string, error) {
	limit := 100
	var cursor *string
	for {
		params := &api.ListWorkflowsParams{Limit: &limit, Cursor: cursor}
		resp, err := client.ListWorkflowsWithResponse(ctx.Context(), project, params)
		if err != nil {
			return "", err
		}
		if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
			return "", &cli.ExitError{
				Code:    exitCodeForStatus(resp.StatusCode()),
				Message: fmt.Sprintf("list workflows: HTTP %d: %s", resp.StatusCode(), string(resp.Body)),
			}
		}
		page := resp.JSON200
		if page == nil {
			return "", nil
		}
		for _, w := range page.Items {
			if w.Handle == handle {
				return w.Id, nil
			}
		}
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return "", nil
		}
		cursor = page.NextCursor
	}
}

func workflowsInitHandler(ctx *cli.Context) error {
	name := ctx.String("name")
	if name == "" {
		name = "my-workflow"
	}
	switch strings.ToLower(ctx.String("format")) {
	case "json":
		ctx.Println(workflowSkeletonJSON(name))
	default:
		ctx.Println(workflowSkeletonYAML(name))
	}
	return nil
}

func workflowSkeletonYAML(name string) string {
	return strings.TrimSpace(`
name: `+name+`
description: A short, human-readable summary of what this workflow does.
spec:
  name: `+name+`
  description: Spec-level description (shown in the editor and run details).
  inputs: []
  outputs: []
  steps:
    - name: hello
      action: print
      action_kind: server
      parameters:
        message: "Hello from ${name}!"
tags:
  env: dev
`) + "\n"
}

func workflowSkeletonJSON(name string) string {
	return strings.TrimSpace(`
{
  "name": "`+name+`",
  "description": "A short, human-readable summary of what this workflow does.",
  "spec": {
    "name": "`+name+`",
    "description": "Spec-level description (shown in the editor and run details).",
    "inputs": [],
    "outputs": [],
    "steps": [
      {
        "name": "hello",
        "action": "print",
        "action_kind": "server",
        "parameters": {
          "message": "Hello from `+name+`!"
        }
      }
    ]
  },
  "tags": {
    "env": "dev"
  }
}
`) + "\n"
}
