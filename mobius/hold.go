package mobius

import (
	"context"
	"log/slog"
	"time"
)

// hold keeps the worker's execution environment in an active ("warm") state
// while jobs are in flight, so a long-running operation isn't frozen by the
// environment pausing underneath it. Implementations are environment-specific
// (e.g. a Sprites.dev microVM); detectHold picks the right one at startup and
// falls back to a no-op when the host needs no such hold.
//
// The lifecycle is: run in its own goroutine for the worker's lifetime, and
// bracket each job with acquire/release. Implementations refcount concurrent
// jobs and establish the hold on the first, releasing it once the worker has
// been idle for the configured keep-warm window. Holds are best-effort by
// default; a required hold reports establish/refresh failures so the worker
// can fail closed under its supervisor.
type hold interface {
	// acquire records one more in-flight job.
	acquire()
	// release records one fewer in-flight job.
	release()
	// run maintains the hold until ctx is cancelled or a required hold fails.
	run(ctx context.Context, opts holdRunOptions) error
	// ensure synchronously establishes the hold now (best-effort) and reports
	// whether it is held. Used at startup for a lifetime-pinned hold so the
	// environment is warm before the worker begins claiming work, closing the
	// gap before the maintainer goroutine's first asynchronous refresh. The
	// no-op hold reports true.
	ensure(ctx context.Context) bool
	// poke tells the maintainer the environment may have just resumed from a
	// pause: a held task could have expired while the VM was suspended, so the
	// hold must be re-established immediately rather than on the next
	// scheduled refresh.
	poke()
	// pins reports whether this hold actually pins an environment. The no-op
	// hold reports false so keep-warm telemetry (the "keep-warm:established"
	// capability) is only advertised when a real hold exists — a worker
	// running off-Sprite must not claim it pinned anything.
	pins() bool
}

type holdRunOptions struct {
	Required bool
}

// detectHold selects the keep-warm strategy for the current environment,
// returning a no-op hold when the host doesn't pause (local dev, CI, customer
// self-hosted, or any non-Sprite host). taskName scopes the hold to this
// worker instance; window is how long the hold persists after the last job.
func detectHold(logger *slog.Logger, taskName string, window time.Duration) hold {
	if h := newSpriteHold(logger, taskName, window); h != nil {
		return h
	}
	return noopHold{}
}

// noopHold is the hold used when the environment needs no keep-warm behaviour.
type noopHold struct{}

func (noopHold) acquire() {}
func (noopHold) release() {}
func (noopHold) run(ctx context.Context, _ holdRunOptions) error {
	<-ctx.Done()
	return ctx.Err()
}
func (noopHold) ensure(context.Context) bool { return true }
func (noopHold) poke()                       {}
func (noopHold) pins() bool                  { return false }
