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
		Description("CLI for the Mobius workflow orchestration platform").
		Version(cliVersion()).
		ExpandGroups(true).
		AddCompletionCommand()

	app.GlobalFlags(
		cli.String("api-url", "").
			Env("MOBIUS_API_URL").
			Default(mobius.DefaultBaseURL).
			Help("Mobius API base URL"),
		cli.String("api-key", "").
			Env("MOBIUS_API_KEY").
			Help("API key (mbx_...)"),
		cli.String("project", "").
			Env("MOBIUS_PROJECT").
			Default("default").
			Help("Project handle"),
		cli.String("log-level", "").
			Env("MOBIUS_LOG_LEVEL").
			Default("info").
			Enum("debug", "info", "warn", "error").
			Help("Log level"),
	)

	registerWorkerCommand(app)
	registerAuthCommands(app)
	registerGeneratedCommands(app)

	return app
}

// clientFromContext builds a *mobius.Client from the global flags on ctx. An
// empty --api-key is accepted here; individual subcommands that require auth
// should declare it via RequireFlags middleware or check explicitly.
func clientFromContext(ctx *cli.Context) *mobius.Client {
	logger := newLogger(ctx.String("log-level"))
	return mobius.NewClient(
		mobius.WithBaseURL(ctx.String("api-url")),
		mobius.WithAPIKey(ctx.String("api-key")),
		mobius.WithProjectHandle(ctx.String("project")),
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
