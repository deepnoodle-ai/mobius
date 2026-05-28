// Command mobius is the general-purpose CLI for Mobius, built on the Go SDK.
//
// The root `mobius` command exposes subcommands for interacting with Mobius API
// resources (automations, runs, triggers, …) plus a `worker` subcommand that
// connects to Mobius Cloud and executes action or generation jobs.
//
// Global flags — `--api-url`, `--api-key`, `--project`, `--log-level` — are
// shared by every subcommand and fall back to the matching MOBIUS_* environment
// variables.
//
// Example:
//
//	mobius worker --queues default
//	mobius automations list
//	mobius runs get run_abc123
package main

import (
	"os"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/env"
)

func main() {
	if _, err := os.Stat(".env"); err == nil {
		_ = env.LoadEnvFile(".env")
	}

	app := newApp()
	if err := app.Execute(); err != nil {
		app.PrintError(err)
		os.Exit(cli.GetExitCode(err))
	}
}
