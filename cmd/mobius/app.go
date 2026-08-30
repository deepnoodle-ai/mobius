package main

//go:generate go run ../../internal/cligen --client ../../api/client.gen.go --spec ../../openapi.yaml --out-dir ../../cmd/mobius

import (
	"log/slog"
	"os"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// newApp constructs the root `mobius` CLI application with all subcommands
// registered. Global flags and their environment-variable fallbacks are
// declared here so every subcommand inherits them.
func newApp() *cli.App {
	app := cli.New("mobius").
		Description("CLI for the Mobius agent loop platform").
		Version(cliVersion()).
		AddCompletionCommand()

	app.GlobalFlags(
		cli.String("api-url", "").
			Env("MOBIUS_API_URL").
			Default(mobius.DefaultBaseURL).
			Help("Mobius API base URL"),
		cli.String("api-key", "").
			Env("MOBIUS_API_KEY").
			Help("API key (mbx_...)"),
		cli.String("profile", "").
			Env("MOBIUS_PROFILE").
			Help("Credential profile"),
		cli.String("log-level", "").
			Env("MOBIUS_LOG_LEVEL").
			Default("info").
			Enum("debug", "info", "warn", "error").
			Help("Log level"),
		cli.String("output", "o").
			Env("MOBIUS_OUTPUT").
			Default("auto").
			Enum("auto", "pretty", "json", "yaml", "text").
			Help("Response format: auto (pretty on TTY, json on pipe), pretty, json, yaml, or text (tab-separated)."),
		cli.Strings("fields", "F").
			Help("Comma-separated fields to project (e.g. -F id,name). Composes with --output."),
		cli.Bool("quiet", "q").
			Help("Suppress response output on success (errors still print)."),
		cli.Strings("var", "").
			Help("${KEY} substitution applied to --file/@-file contents. Repeatable: --var ENV=prod."),
	)

	app.Use(authMiddleware())

	registerWorkerCommand(app)
	registerGitCredentialHelperCommand(app)
	registerAuthCommands(app)
	registerGeneratedCommands(app)
	registerPrincipalCreateCommand(app)
	registerArtifactUploadCommand(app)
	registerActionSecretCommands(app)
	registerReplaceOAuthReturnOriginsCommand(app)
	registerSkillImportCommands(app)

	return app
}

// clientFromContext builds a *mobius.Client from the credential resolved by
// authMiddleware. An empty key is accepted here; individual subcommands that
// require auth should attach the requireAuth middleware.
func clientFromContext(ctx *cli.Context) (*mobius.Client, error) {
	auth := authFor(ctx)
	logger := newLogger(ctx.String("log-level"))
	return mobius.NewClient(
		mobius.WithBaseURL(auth.APIURL),
		mobius.WithAPIKey(auth.APIKey),
		mobius.WithLogger(logger),
	)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
