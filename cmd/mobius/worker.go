package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
)

// registerWorkerCommand attaches the `mobius worker` subcommand, which claims
// and executes jobs from one or more queues in the configured project. The
// worker ships with every stock action from github.com/deepnoodle-ai/mobius/mobius/action
// registered, so it can run trivial and test workflows out of the box.
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
		Use(cli.RequireFlags("api-key")).
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
			cli.Int("concurrency", "").
				Default(1).
				Help("Max in-flight jobs for one worker process (default 1)"),
			cli.Int("workers", "").
				Default(0).
				Help("Run N independent worker instances (advanced; mutually exclusive with --concurrency)"),
			cli.Int("poll-wait", "").
				Default(20).
				Help("Long-poll window in seconds (0-30)"),
		).
		Run(func(ctx *cli.Context) error {
			client, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			logger := newLogger(ctx.String("log-level"))

			name := ctx.String("name")
			instanceID := ctx.String("instance-id")
			queues := ctx.Strings("queues")
			concurrency := ctx.Int("concurrency")
			workers := ctx.Int("workers")

			if workers > 0 && concurrency > 1 {
				return fmt.Errorf(
					"--workers and --concurrency are mutually exclusive: " +
						"--workers spawns N independent presence rows, " +
						"--concurrency keeps one row serving N concurrent jobs — " +
						"pick whichever matches your operational model",
				)
			}

			baseConfig := mobius.WorkerConfig{
				Name:             name,
				WorkerInstanceID: instanceID,
				Version:          ctx.String("worker-version"),
				Queues:           queues,
				Concurrency:      concurrency,
				PollWaitSeconds:  ctx.Int("poll-wait"),
				Logger:           logger,
			}

			logger.Info("starting worker",
				"name", name,
				"api_url", ctx.String("api-url"),
				"project", ctx.String("project"),
				"queues", queues,
				"concurrency", concurrency,
				"workers", workers,
			)

			if workers > 0 {
				pool := client.NewWorkerPool(mobius.WorkerPoolConfig{
					WorkerConfig: baseConfig,
					Count:        workers,
				})
				registerStockActions(pool)
				if err := pool.Run(ctx.Context()); err != nil && !isContextCanceled(err) {
					return fmt.Errorf("worker pool exited: %w", err)
				}
			} else {
				worker := client.NewWorker(baseConfig)
				registerStockActions(worker)
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
// test workflows without requiring custom code.
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
	for _, a := range stock {
		w.Register(a)
	}
}

func isContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
