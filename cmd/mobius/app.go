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
		cli.String("project", "").
			Env("MOBIUS_PROJECT").
			Default("default").
			Help("Project handle"),
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
	registerAuthCommands(app)
	registerGeneratedCommands(app)
	registerConfigExtensions(app)
	registerWorkflowsExtras(app)
	registerAgentsExtras(app)

	return app
}

// clientFromContext builds a *mobius.Client from the credential resolved by
// authMiddleware. An empty key is accepted here; individual subcommands that
// require auth should attach the requireAuth middleware. A construction
// error — e.g. a conflict between the resolved project and the project
// suffix embedded in a project-pinned API key — surfaces here so the caller
// can fail the command before any HTTP request is sent.
//
// The project handle is forwarded when the user set --project / MOBIUS_PROJECT
// explicitly, when it came from a saved profile, or when the API key is
// org-scoped (no embedded suffix). For project-pinned keys the handle is
// extracted from the key itself; if a project is also resolved it must match
// (NewClient enforces this).
func clientFromContext(ctx *cli.Context) (*mobius.Client, error) {
	auth := authFor(ctx)
	logger := newLogger(ctx.String("log-level"))
	opts := []mobius.Option{
		mobius.WithBaseURL(auth.APIURL),
		mobius.WithAPIKey(auth.APIKey),
		mobius.WithLogger(logger),
	}
	orgScopedKey := auth.APIKey != "" && projectHandleFromCredential(auth.APIKey) == ""
	if forwardProject(ctx, auth) || orgScopedKey {
		opts = append(opts, mobius.WithProjectHandle(auth.Project))
	}
	return mobius.NewClient(opts...)
}

// forwardProject reports whether the resolved project handle should be sent
// to NewClient. We forward when the caller set --project / MOBIUS_PROJECT
// explicitly, or when the project came from a saved profile (a logged-in user
// with a project-scoped profile expects that scope to apply automatically).
func forwardProject(ctx *cli.Context, auth *resolvedAuth) bool {
	if ctx.IsSet("project") {
		return true
	}
	return auth.Source == authSourceProfile && auth.Profile != nil && auth.Profile.ProjectHandle != ""
}

func projectHandleFromCredential(key string) string {
	if project, ok := projectHandleFromCLIToken(key); ok {
		return project
	}
	if project, ok := mobius.ProjectHandleFromAPIKey(key); ok {
		return project
	}
	return ""
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
