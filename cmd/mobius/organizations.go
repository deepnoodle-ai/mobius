package main

import (
	"fmt"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerReplaceOAuthReturnOriginsCommand adds the hand-written
// `organizations replace-oauth-return-origins` command (the generator skips
// replaceOAuthReturnOrigins). It is hand-written because an empty origins
// list is the documented way to disable embedded return, and the generated
// required-flag check made that state unreachable — --clear sends
// {"origins": []} explicitly.
func registerReplaceOAuthReturnOriginsCommand(app *cli.App) {
	grp := app.Group("organizations")

	grp.Command("replace-oauth-return-origins").
		Description("Replace the org's OAuth return-origin allowlist").
		Flags(
			cli.Strings("origins", "").Help("Exact HTTPS return origins to allow. Required unless --clear or --file is used."),
			cli.Bool("clear", "").Help("Replace the allowlist with an empty list, disabling embedded return. Mutually exclusive with --origins."),
			cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin). Flags override file contents."),
			cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
		).
		Use(requireAuth()).
		Run(runReplaceOAuthReturnOrigins)
}

func runReplaceOAuthReturnOrigins(ctx *cli.Context) error {
	if ctx.Bool("clear") && ctx.IsSet("origins") {
		return fmt.Errorf("--clear and --origins are mutually exclusive")
	}
	var body api.ReplaceOAuthReturnOriginsJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	if ctx.IsSet("origins") {
		body.Origins = ctx.Strings("origins")
	}
	if ctx.Bool("clear") {
		body.Origins = []string{}
	} else if len(body.Origins) == 0 {
		return fmt.Errorf("--origins is required (or supply it via --file); pass --clear to disable embedded return with an empty list")
	}
	if ctx.Bool("dry-run") {
		return printDryRun(ctx, body)
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	resp, err := mc.RawClient().ReplaceOAuthReturnOriginsWithResponse(ctx.Context(), body)
	if err != nil {
		return err
	}
	return printResponse(ctx, "replaceOAuthReturnOrigins", resp.StatusCode(), resp.Body)
}
