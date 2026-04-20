// Command mobius is the general-purpose CLI for Mobius, built on the Go SDK.
//
// The root `mobius` command exposes subcommands for interacting with Mobius API
// resources (workflows, runs, triggers, …) plus a `worker` subcommand that
// claims and executes jobs from one or more queues.
//
// Global flags — `--api-url`, `--api-key`, `--project`, `--log-level` — are
// shared by every subcommand and fall back to the matching MOBIUS_* environment
// variables.
//
// Example:
//
//	mobius worker --queues default
//	mobius workflows list
//	mobius runs get run_abc123
package main

import (
	"os"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/env"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
)

func main() {
	if _, err := os.Stat(".env"); err == nil {
		_ = env.LoadEnvFile(".env")
	}

	// Resolve the saved browser-based login credential into the same
	// environment variables the global flags already honor, so commands
	// using `cli.RequireFlags("api-key")` succeed without forcing the user
	// to re-specify anything. Explicit --api-key / MOBIUS_API_KEY /
	// MOBIUS_API_URL still win because they reach the flag parser first.
	applySavedCredential()

	app := newApp()
	if err := app.Execute(); err != nil {
		app.PrintError(err)
		os.Exit(cli.GetExitCode(err))
	}
}

// applySavedCredential fills in MOBIUS_API_KEY / MOBIUS_API_URL from the
// persisted browser-login credential when those variables are not already
// set, so the rest of the CLI can treat saved and explicit credentials
// identically. Errors are intentionally swallowed — an unreadable
// credentials file must not block commands that do not need auth (e.g.
// `mobius auth login`, `mobius --help`).
func applySavedCredential() {
	if _, ok := os.LookupEnv("MOBIUS_API_KEY"); ok {
		return
	}
	cred, err := authstore.Load()
	if err != nil || cred == nil || cred.Token == "" {
		return
	}
	os.Setenv("MOBIUS_API_KEY", cred.Token)
	if cred.APIURL != "" {
		if _, ok := os.LookupEnv("MOBIUS_API_URL"); !ok {
			os.Setenv("MOBIUS_API_URL", cred.APIURL)
		}
	}
}
