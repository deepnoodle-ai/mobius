package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/google/uuid"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
)

// registerWorkerCommand attaches the `mobius worker` subcommand, which claims
// and executes jobs from one or more queues in the configured project. The
// worker ships with every stock action from github.com/deepnoodle-ai/mobius/mobius/action
// registered, so it can run trivial and test workflows out of the box.
func registerWorkerCommand(app *cli.App) {
	app.Command("worker").
		Description("Run a Mobius worker that executes queued runs").
		Use(cli.RequireFlags("api-key")).
		Flags(
			cli.String("name", "").
				Default("mobius-worker").
				Help("Worker name reported to Mobius"),
			cli.String("worker-version", "").
				Default(buildVersion()).
				Help("Worker version reported to Mobius"),
			cli.Strings("queues", "q").
				Env("MOBIUS_QUEUES").
				Help("Queue names to poll; empty = all queues in the project"),
			cli.Int("concurrency", "").
				Default(10).
				Help("Maximum runs to execute in parallel"),
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
			workerID := fmt.Sprintf("%s_%s", name, uuid.New().String()[:8])
			queues := ctx.Strings("queues")

			worker := client.NewWorker(mobius.WorkerConfig{
				WorkerID:        workerID,
				Name:            name,
				Version:         ctx.String("worker-version"),
				Queues:          queues,
				Concurrency:     ctx.Int("concurrency"),
				PollWaitSeconds: ctx.Int("poll-wait"),
				Logger:          logger,
			})

			registerStockActions(worker)

			logger.Info("starting worker",
				"worker_id", workerID,
				"api_url", ctx.String("api-url"),
				"project", ctx.String("project"),
				"queues", queues,
				"concurrency", ctx.Int("concurrency"),
			)

			if err := worker.Run(ctx.Context()); err != nil && !isContextCanceled(err) {
				return fmt.Errorf("worker exited: %w", err)
			}
			logger.Info("worker stopped")
			return nil
		})
}

// registerStockActions attaches every general-purpose action from
// github.com/deepnoodle-ai/mobius/mobius/action to the worker. These cover most trivial and
// test workflows without requiring custom code.
func registerStockActions(w *mobius.Worker) {
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
