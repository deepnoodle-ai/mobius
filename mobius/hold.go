package mobius

import (
	"context"
	"log/slog"
)

// hold keeps the worker's execution environment in an active ("warm") state
// while jobs are in flight, so a long-running operation isn't frozen by the
// environment pausing underneath it. Implementations are environment-specific
// (e.g. a Sprites.dev microVM); detectHold picks the right one at startup and
// falls back to a no-op when the host needs no such hold.
//
// The lifecycle is: run in its own goroutine for the worker's lifetime, and
// bracket each job with acquire/release. Implementations refcount concurrent
// jobs and establish the hold on the first, releasing it once the worker is
// idle. All methods are safe for concurrent use and best-effort: a hold failure
// must never surface to job execution.
type hold interface {
	// acquire records one more in-flight job.
	acquire()
	// release records one fewer in-flight job.
	release()
	// run maintains the hold until ctx is cancelled.
	run(ctx context.Context)
}

// detectHold selects the keep-warm strategy for the current environment,
// returning a no-op hold when the host doesn't pause (local dev, CI, customer
// self-hosted, or any non-Sprite host).
func detectHold(logger *slog.Logger) hold {
	if h := newSpriteHold(logger); h != nil {
		return h
	}
	return noopHold{}
}

// noopHold is the hold used when the environment needs no keep-warm behaviour.
type noopHold struct{}

func (noopHold) acquire()            {}
func (noopHold) release()            {}
func (noopHold) run(context.Context) {}
