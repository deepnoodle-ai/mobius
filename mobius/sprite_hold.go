package mobius

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	// spriteAPISocketPath is the Sprites.dev management socket, present only
	// inside a Sprite microVM. Its existence is how the worker detects that it
	// is running inside a Sprite.
	spriteAPISocketPath = "/.sprite/api.sock"
	// spriteTaskPrefix prefixes the Tasks-API hold each worker maintains. The
	// full task name is "mobius-worker-<worker_instance_id>": task names are
	// global to the Sprite, so scoping them per instance keeps two workers on
	// one Sprite (a pool, or two processes) from releasing each other's hold.
	spriteTaskPrefix = "mobius-worker"
	// spriteTaskExpire is the task's lifetime; refreshed on spriteHeartbeat-
	// Interval. Short enough that a crashed worker's hold releases on its own
	// (the Sprite then pauses), long enough to cover a missed refresh or two.
	spriteTaskExpire        = "5m"
	spriteHeartbeatInterval = 60 * time.Second
	spriteRequestTimeout    = 10 * time.Second
	// spriteRequiredRetryDelay separates the in-process attempts a required
	// hold makes before giving up. A single transient Tasks-API blip must not
	// bounce the whole worker through its Sprite Service supervisor.
	spriteRequiredRetryDelay    = 1 * time.Second
	spriteRequiredRetryAttempts = 3
)

var _ hold = (*spriteHold)(nil)

// spriteHold is the [hold] implementation for a Sprites.dev microVM.
//
// A Sprite pauses when idle. That freezes any long-running outbound operation a
// job is performing — a `git clone` is the canonical victim: compute stops
// mid-flight (the process's own clock included), the idle TCP/TLS connection to
// the remote eventually drops, and the clone fails ~3 minutes later with a
// misleading transport error ("the TLS connection was non-properly terminated").
//
// The Sprites Tasks API exposes a "task": a hold that keeps the VM running while
// it is live, reachable only from inside the VM over the management socket.
// While at least one job runs we maintain such a task with a short expiry,
// refreshed on an interval; once the worker has been idle for the configured
// keep-warm window we release it so the Sprite is free to pause again (and stop
// billing). The window is the single dial between the keep-warm modes: 0
// releases the instant the last job finishes (on-demand), a positive window
// bridges an agent's inter-tool-call think-gaps (recent-work), and the worker
// pins a baseline acquire for its whole lifetime in forever mode.
type spriteHold struct {
	client       *http.Client
	taskName     string
	logger       *slog.Logger
	interval     time.Duration
	releaseGrace time.Duration
	// retryDelay separates required-mode establish/refresh retries; a field so
	// tests can shrink it.
	retryDelay time.Duration

	mu       sync.Mutex
	count    int
	degraded bool          // last refresh failed; tracked to log transitions once
	refresh  bool          // poke(): re-establish immediately (environment may have resumed from a pause)
	wake     chan struct{} // coalesced signal that count changed
}

// newSpriteHold returns a [spriteHold] when the process is running inside a
// Sprite, or nil otherwise (the management socket is absent). The nil result is
// what [detectHold] uses to fall back to a no-op hold.
func newSpriteHold(logger *slog.Logger, taskName string, window time.Duration) *spriteHold {
	return newSpriteHoldWithPath(spriteAPISocketPath, logger, taskName, window)
}

func newSpriteHoldWithPath(socketPath string, logger *slog.Logger, taskName string, window time.Duration) *spriteHold {
	if !isSpriteSocket(socketPath) {
		return nil // not inside a Sprite
	}
	if logger == nil {
		logger = slog.Default()
	}
	if taskName == "" {
		taskName = spriteTaskPrefix
	}
	if window < 0 {
		window = 0
	}
	logger.Info("sprite keep-warm enabled", "socket", socketPath, "task", taskName, "window", window.String())
	return &spriteHold{
		client: &http.Client{
			Timeout: spriteRequestTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
		taskName:     taskName,
		logger:       logger,
		interval:     spriteHeartbeatInterval,
		releaseGrace: window,
		retryDelay:   spriteRequiredRetryDelay,
		wake:         make(chan struct{}, 1),
	}
}

// isSpriteSocket reports whether path is a Unix domain socket. A plain stat
// "exists" check is not enough — only a socket is the Sprites management API.
func isSpriteSocket(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

// acquire records one more in-flight job and nudges the maintainer to ensure the
// hold is established.
func (h *spriteHold) acquire() {
	h.mu.Lock()
	h.count++
	h.mu.Unlock()
	h.signal()
}

// release records one fewer in-flight job and nudges the maintainer to release
// the hold once the worker is idle.
func (h *spriteHold) release() {
	h.mu.Lock()
	if h.count > 0 {
		h.count--
	}
	h.mu.Unlock()
	h.signal()
}

func (h *spriteHold) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count > 0
}

func (h *spriteHold) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// poke flags that the environment may have just resumed from a pause. A task
// created before the pause may have expired while the VM was suspended (its
// expiry kept running on the host), so the maintainer must re-PUT it now
// rather than waiting for the next scheduled refresh.
func (h *spriteHold) poke() {
	h.mu.Lock()
	h.refresh = true
	h.mu.Unlock()
	h.signal()
}

func (h *spriteHold) pins() bool { return true }

// takeRefresh consumes the poke flag.
func (h *spriteHold) takeRefresh() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.refresh
	h.refresh = false
	return r
}

// run maintains the Sprite task for the worker's lifetime. A single goroutine
// owns all task I/O, so there is no create/delete race between concurrent jobs:
// it creates the task when work first appears, refreshes it on h.interval, and
// releases it once the worker has been idle for the keep-warm window (so it
// bridges the inter-tool-call think-gap) or when ctx is cancelled. If
// opts.Required is set, a failed create/refresh (after in-process retries) is
// fatal so the worker exits under its Sprite Service supervisor instead of
// silently letting the Sprite pause.
func (h *spriteHold) run(ctx context.Context, opts holdRunOptions) error {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	var grace *time.Timer
	var graceC <-chan time.Time
	stopGrace := func() {
		if grace != nil {
			grace.Stop()
			grace = nil
			graceC = nil
		}
	}
	held := false
	releaseOnExit := true
	defer func() {
		stopGrace()
		if held && releaseOnExit {
			h.deleteTask()
		}
	}()
	for {
		if h.takeRefresh() && held {
			// The environment resumed from a pause: the task may have expired
			// mid-suspend. Re-establish it before anything else.
			if ok := h.putTaskRetrying(ctx, opts); !ok && opts.Required {
				releaseOnExit = false
				return fmt.Errorf("mobius: required Sprite keep-warm hold could not be re-established after resume for task %q", h.taskName)
			}
		}
		switch active := h.active(); {
		case active && !held:
			stopGrace()
			held = h.putTaskRetrying(ctx, opts)
			if !held && opts.Required {
				releaseOnExit = false
				return fmt.Errorf("mobius: required Sprite keep-warm hold could not be established for task %q", h.taskName)
			}
		case active && held:
			// Work resumed (or never stopped) within the grace window — keep the
			// hold and cancel any pending release.
			stopGrace()
		case !active && held && grace == nil:
			if h.releaseGrace <= 0 {
				// On-demand mode: no window; drop the hold the moment the worker
				// goes idle so the Sprite is free to pause.
				h.deleteTask()
				held = false
				continue
			}
			// Idle: arm the release grace instead of dropping the hold now, so a
			// next-tool-call job arriving in the think-gap still finds it warm.
			grace = time.NewTimer(h.releaseGrace)
			graceC = grace.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.wake:
		case <-t.C:
			if held {
				if ok := h.putTaskRetrying(ctx, opts); !ok && opts.Required {
					// A failed refresh does not prove the already-created task is
					// gone. Leave any remaining expiry in place while the Sprite
					// Service restarts the worker and reacquires the hold.
					releaseOnExit = false
					return fmt.Errorf("mobius: required Sprite keep-warm hold refresh failed for task %q", h.taskName)
				}
			}
		case <-graceC:
			stopGrace()
			if !h.active() && held {
				h.deleteTask()
				held = false
			}
		}
	}
}

// ensure synchronously establishes the hold now and reports whether it is held.
// The caller (worker startup, for a lifetime-pinned hold) has already recorded
// its job via acquire; this lands the Tasks-API PUT before the worker begins
// claiming work, rather than waiting for the maintainer goroutine's first
// asynchronous pass. Best-effort: a failure is surfaced by putTask and retried
// on the interval.
func (h *spriteHold) ensure(ctx context.Context) bool {
	return h.putTask(ctx)
}

// putTaskRetrying is putTask plus, in required mode, a couple of quick
// in-process retries. Fail-closed is right for a required hold, but a single
// transient Tasks-API blip should not bounce the worker through its
// supervisor restart loop.
func (h *spriteHold) putTaskRetrying(ctx context.Context, opts holdRunOptions) bool {
	attempts := 1
	if opts.Required {
		attempts = spriteRequiredRetryAttempts
	}
	for i := 0; ; i++ {
		if h.putTask(ctx) {
			return true
		}
		if i >= attempts-1 || ctx.Err() != nil {
			return false
		}
		if err := sleepContext(ctx, h.retryDelay); err != nil {
			return false
		}
	}
}

// putTask creates or refreshes the hold and records whether it succeeded so the
// degraded↔healthy transition is logged once (not on every interval). A failure
// means the Sprite can pause mid-work, so it is surfaced at Warn — but never to
// job execution. The job keeps running; the hold is retried on the next tick.
func (h *spriteHold) putTask(ctx context.Context) bool {
	ok := h.doPutTask(ctx)
	h.mu.Lock()
	switch {
	case !ok && !h.degraded:
		h.degraded = true
		h.logger.Warn("sprite keep-warm: hold refresh failing; the environment may pause mid-work", "task", h.taskName)
	case ok && h.degraded:
		h.degraded = false
		h.logger.Info("sprite keep-warm: hold refresh recovered", "task", h.taskName)
	}
	h.mu.Unlock()
	return ok
}

func (h *spriteHold) taskURL() string {
	return "http://sprite/v1/tasks/" + url.PathEscape(h.taskName)
}

func (h *spriteHold) doPutTask(ctx context.Context) bool {
	body := fmt.Sprintf(`{"expire":%q}`, spriteTaskExpire)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		h.taskURL(), bytes.NewReader([]byte(body)))
	if err != nil {
		h.logger.Debug("sprite keep-warm: build PUT failed", "error", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Debug("sprite keep-warm: PUT failed", "error", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		h.logger.Debug("sprite keep-warm: PUT unexpected status", "status", resp.StatusCode)
		return false
	}
	return true
}

// deleteTask releases the hold. It uses a fresh context so teardown still runs
// when the worker's context has already been cancelled. A 404 (already gone) is
// fine; all outcomes are best-effort.
func (h *spriteHold) deleteTask() {
	ctx, cancel := context.WithTimeout(context.Background(), spriteRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		h.taskURL(), nil)
	if err != nil {
		h.logger.Debug("sprite keep-warm: build DELETE failed", "error", err)
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Debug("sprite keep-warm: DELETE failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}
