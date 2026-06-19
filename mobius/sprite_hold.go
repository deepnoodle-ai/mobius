package mobius

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// spriteAPISocketPath is the Sprites.dev management socket, present only
	// inside a Sprite microVM. Its existence is how the worker detects that it
	// is running inside a Sprite.
	spriteAPISocketPath = "/.sprite/api.sock"
	// spriteTaskName is the single Tasks-API hold the worker maintains. A task
	// is a named hold; we refcount in-process and keep exactly one task alive.
	spriteTaskName = "mobius-worker"
	// spriteTaskExpire is the task's lifetime; refreshed on spriteHeartbeat-
	// Interval. Short enough that a crashed worker's hold releases on its own
	// (the Sprite then pauses), long enough to cover a missed refresh or two.
	spriteTaskExpire        = "5m"
	spriteHeartbeatInterval = 60 * time.Second
	spriteRequestTimeout    = 10 * time.Second
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
// refreshed on an interval; when the worker goes idle we release it so the
// Sprite is free to pause again (and stop billing). This scopes the hold to
// active work rather than the worker's whole lifetime, so reused (lease/explicit
// lifetime) environments still hibernate between jobs.
type spriteHold struct {
	client   *http.Client
	taskName string
	logger   *slog.Logger
	interval time.Duration

	mu    sync.Mutex
	count int
	wake  chan struct{} // coalesced signal that count changed
}

// newSpriteHold returns a [spriteHold] when the process is running inside a
// Sprite, or nil otherwise (the management socket is absent). The nil result is
// what [detectHold] uses to fall back to a no-op hold.
func newSpriteHold(logger *slog.Logger) *spriteHold {
	return newSpriteHoldWithPath(spriteAPISocketPath, logger)
}

func newSpriteHoldWithPath(socketPath string, logger *slog.Logger) *spriteHold {
	if !isSpriteSocket(socketPath) {
		return nil // not inside a Sprite
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("sprite keep-warm enabled", "socket", socketPath, "task", spriteTaskName)
	return &spriteHold{
		client: &http.Client{
			Timeout: spriteRequestTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
		taskName: spriteTaskName,
		logger:   logger,
		interval: spriteHeartbeatInterval,
		wake:     make(chan struct{}, 1),
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

// run maintains the Sprite task for the worker's lifetime. A single goroutine
// owns all task I/O, so there is no create/delete race between concurrent jobs:
// it creates the task when work first appears, refreshes it on h.interval, and
// deletes it when the worker goes idle or ctx is cancelled.
func (h *spriteHold) run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	held := false
	defer func() {
		if held {
			h.deleteTask()
		}
	}()
	for {
		switch active := h.active(); {
		case active && !held:
			held = h.putTask(ctx)
		case !active && held:
			h.deleteTask()
			held = false
		}
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-t.C:
			if held {
				h.putTask(ctx)
			}
		}
	}
}

// putTask creates or refreshes the hold. Best-effort: a failure is logged at
// debug and retried on the next tick, never surfaced to job execution.
func (h *spriteHold) putTask(ctx context.Context) bool {
	body := fmt.Sprintf(`{"expire":%q}`, spriteTaskExpire)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://sprite/v1/tasks/"+h.taskName, bytes.NewReader([]byte(body)))
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
	defer resp.Body.Close()
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
		"http://sprite/v1/tasks/"+h.taskName, nil)
	if err != nil {
		h.logger.Debug("sprite keep-warm: build DELETE failed", "error", err)
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Debug("sprite keep-warm: DELETE failed", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}
