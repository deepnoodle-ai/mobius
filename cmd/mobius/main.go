// Command mobius is the general-purpose CLI for Mobius, built on the Go SDK.
//
// The root `mobius` command exposes subcommands for interacting with Mobius API
// resources (loops, runs, agents, …) plus a `worker` subcommand that
// connects to Mobius Cloud and executes action or generation jobs.
//
// Global flags — `--api-url`, `--api-key`, `--project`, `--log-level` — are
// shared by every subcommand and fall back to the matching MOBIUS_* environment
// variables.
//
// Example:
//
//	mobius worker --queues default
//	mobius loops list
//	mobius runs get run_abc123
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/env"
)

func main() {
	if _, err := os.Stat(".env"); err == nil {
		_ = env.LoadEnvFile(".env")
	}

	// Cancel the command context on SIGINT/SIGTERM so long-running commands
	// (notably `mobius worker`) shut down gracefully: in-flight jobs are
	// cancelled and report terminal status, and the Sprite keep-warm task is
	// released instead of pinning the microVM until its expiry. A second
	// signal kills the process immediately (signal.NotifyContext restores
	// default handling once the context is cancelled).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := newApp()
	if err := app.ExecuteContext(ctx, os.Args[1:]); err != nil {
		app.PrintError(err)
		os.Exit(cli.GetExitCode(err))
	}
}
