package main

import (
	"fmt"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerSkillImportCommands adds the two skill import commands. They are
// hand-written (the generator skips importSkill and importOrganizationSkill)
// so they take the skill document itself as their argument — a Claude Code or
// Dive-style markdown file, sent verbatim — instead of a JSON request body
// wrapping it.
func registerSkillImportCommands(app *cli.App) {
	importFlags := []cli.Flag{
		cli.String("name", "").Help("Override the skill name derived from the document."),
		cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
	}
	docArg := &cli.Arg{Name: "path", Description: "Skill document path (markdown, optionally with YAML frontmatter), or '-' for stdin.", Required: true}

	app.Group("skills").Command("import").
		Description("Import skill").
		AddArg(docArg).
		Flags(importFlags...).
		Use(requireAuth()).
		Run(func(ctx *cli.Context) error {
			body, err := skillImportBody(ctx)
			if err != nil {
				return err
			}
			if ctx.Bool("dry-run") {
				return printDryRun(ctx, body)
			}
			mc, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			resp, err := mc.RawClient().ImportSkillWithResponse(ctx.Context(), authFor(ctx).Project, *body)
			if err != nil {
				return err
			}
			return printResponse(ctx, "importSkill", resp.StatusCode(), resp.Body)
		})

	app.Group("org-skills").Command("import").
		Description("Import organization skill").
		AddArg(docArg).
		Flags(importFlags...).
		Use(requireAuth()).
		Run(func(ctx *cli.Context) error {
			body, err := skillImportBody(ctx)
			if err != nil {
				return err
			}
			if ctx.Bool("dry-run") {
				return printDryRun(ctx, body)
			}
			mc, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			resp, err := mc.RawClient().ImportOrganizationSkillWithResponse(ctx.Context(), *body)
			if err != nil {
				return err
			}
			return printResponse(ctx, "importOrganizationSkill", resp.StatusCode(), resp.Body)
		})
}

// skillImportBody reads the skill document named by the path argument ('-'
// for stdin) verbatim — no --var substitution, no JSON/YAML parsing — and
// wraps it in the import request.
func skillImportBody(ctx *cli.Context) (*api.ImportSkillRequest, error) {
	path := ctx.Arg(0)
	data, _, err := readBodyBytes(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("skill document %s is empty", path)
	}
	body := &api.ImportSkillRequest{Content: string(data)}
	if ctx.IsSet("name") {
		name := ctx.String("name")
		body.Name = &name
	}
	return body, nil
}
