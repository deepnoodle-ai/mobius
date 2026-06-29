package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
)

// registerWorkerCommand attaches the `mobius worker` subcommand, which claims
// and executes jobs from one or more queues in the configured project. The
// worker ships with every stock action from github.com/deepnoodle-ai/mobius/mobius/action
// registered, so it can run trivial and test automations out of the box.
//
// The default mode is one worker process serving up to `--concurrency` jobs
// concurrently — that's the right shape for most operators because it
// surfaces as a single row on the workers page with a saturation bar.
// `--workers` runs N independent worker instances (separate presence rows)
// and is reserved for cases where each child needs to drain or isolate
// independently. The two flags are mutually exclusive.
func registerWorkerCommand(app *cli.App) {
	app.Command("worker").
		Description("Run a Mobius worker that executes queued jobs").
		Use(requireAuth()).
		Flags(
			cli.String("name", "").
				Default("mobius-worker").
				Help("Worker name reported to Mobius"),
			cli.String("instance-id", "").
				Help("Override the auto-detected worker_instance_id (advanced; rare)"),
			cli.String("worker-version", "").
				Default(buildVersion()).
				Help("Worker version reported to Mobius"),
			cli.Strings("queues", "q").
				Env("MOBIUS_QUEUES").
				Help("Queue names to poll; empty = all queues in the project"),
			cli.Strings("actions", "a").
				Env("MOBIUS_WORKER_ACTION_NAMES").
				Help("Action names to advertise; empty = every registered action"),
			cli.String("environment-id", "").
				Env("MOBIUS_WORKER_ENVIRONMENT_ID").
				Help("Managed environment ID this worker is executing inside"),
			cli.Int("concurrency", "").
				Default(1).
				Help("Max in-flight jobs for one worker process (default 1)"),
			cli.Int("workers", "").
				Default(0).
				Help("Run N independent worker instances (advanced; mutually exclusive with --concurrency)"),
			cli.Bool("keep-warm-for-lifetime", "").
				Env("MOBIUS_WORKER_KEEP_WARM").
				Help("Keep the environment warm for the worker's whole lifetime, not just while a job runs (set for run-scoped environments so the VM doesn't hibernate between an agent's tool calls)"),
			cli.Bool("ollama", "").
				Env("MOBIUS_WORKER_OLLAMA").
				Help("Serve local Ollama models: advertise installed models to Mobius and handle llm_generation jobs from a local Ollama server"),
			cli.String("ollama-url", "").
				Default(defaultOllamaURL).
				Env("MOBIUS_OLLAMA_URL").
				Help("Base URL of the local Ollama server"),
		).
		Run(func(ctx *cli.Context) error {
			client, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			logger := newLogger(ctx.String("log-level"))

			name := ctx.String("name")
			instanceID := ctx.String("instance-id")
			queues := firstNonEmptyStrings(ctx.Strings("queues"), splitEnv("MOBIUS_WORKER_QUEUES"))
			actions := firstNonEmptyStrings(ctx.Strings("actions"), splitEnv("MOBIUS_WORKER_ACTION_NAMES"))
			environmentID := ctx.String("environment-id")
			concurrency := ctx.Int("concurrency")
			workers := ctx.Int("workers")
			keepWarmForLifetime := ctx.Bool("keep-warm-for-lifetime")
			ollamaEnabled := ctx.Bool("ollama")
			ollamaURL := ctx.String("ollama-url")

			if workers > 0 && concurrency > 1 {
				return fmt.Errorf(
					"--workers and --concurrency are mutually exclusive: " +
						"--workers spawns N independent presence rows, " +
						"--concurrency keeps one row serving N concurrent jobs — " +
						"pick whichever matches your operational model",
				)
			}

			baseConfig := mobius.WorkerConfig{
				Name:                name,
				WorkerInstanceID:    instanceID,
				Version:             ctx.String("worker-version"),
				EnvironmentID:       environmentID,
				Queues:              queues,
				Actions:             actions,
				Concurrency:         concurrency,
				KeepWarmForLifetime: keepWarmForLifetime,
				Logger:              logger,
			}

			// When --ollama is set, advertise the models the local Ollama
			// server currently has installed so they appear in the worker-model
			// catalog and become routable. The generation handler is registered
			// on the worker/pool below.
			if ollamaEnabled {
				discovered, err := discoverOllamaModels(ctx.Context(), ollamaURL)
				if err != nil {
					return fmt.Errorf("ollama: %w", err)
				}
				baseConfig.Models = append(baseConfig.Models, discovered...)
				if len(discovered) == 0 {
					logger.Warn("ollama enabled but no models are installed; run `ollama pull <model>`", "ollama_url", ollamaURL)
				} else {
					logger.Info("ollama generation enabled", "ollama_url", ollamaURL, "models", ollamaModelNames(discovered))
				}
			}

			auth := authFor(ctx)
			logger.Info("starting worker",
				"name", name,
				"api_url", auth.APIURL,
				"project", auth.Project,
				"environment_id", environmentID,
				"queues", queues,
				"actions", actions,
				"concurrency", concurrency,
				"workers", workers,
				"keep_warm_for_lifetime", keepWarmForLifetime,
			)

			if workers > 0 {
				pool := client.NewWorkerPool(mobius.WorkerPoolConfig{
					WorkerConfig: baseConfig,
					Count:        workers,
				})
				registerStockActions(pool)
				if ollamaEnabled {
					pool.RegisterGenerator(ollamaProvider, "*", newOllamaGenerator(ollamaURL))
				}
				if err := pool.Run(ctx.Context()); err != nil && !isContextCanceled(err) {
					return fmt.Errorf("worker pool exited: %w", err)
				}
			} else {
				worker := client.NewWorker(baseConfig)
				registerStockActions(worker)
				if ollamaEnabled {
					worker.RegisterGenerator(ollamaProvider, "*", newOllamaGenerator(ollamaURL))
				}
				if err := worker.Run(ctx.Context()); err != nil && !isContextCanceled(err) {
					return fmt.Errorf("worker exited: %w", err)
				}
			}
			logger.Info("worker stopped")
			return nil
		})
}

// registerStockActions attaches every general-purpose action from
// github.com/deepnoodle-ai/mobius/mobius/action to the worker. These cover most trivial and
// test automations without requiring custom code.
type actionRegistrar interface {
	Register(mobius.Action)
}

func registerStockActions(w actionRegistrar) {
	stock := []mobius.Action{
		action.NewPrintAction(),
		action.NewFailAction(),
		action.NewJSONAction(),
		action.NewTimeAction(),
		action.NewRandomAction(),
	}
	stock = append(stock, action.EnvironmentActions()...)
	for _, a := range stock {
		w.Register(a)
	}
}

func splitEnv(name string) []string {
	raw := os.Getenv(name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmptyStrings(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func isContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
