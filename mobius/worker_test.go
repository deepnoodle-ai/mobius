package mobius

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

// fakeHold is a counting [hold] used to observe acquire/release bracketing.
type fakeHold struct {
	mu       sync.Mutex
	acquires int
	releases int
	ran      chan struct{}
}

func newFakeHold() *fakeHold { return &fakeHold{ran: make(chan struct{}, 1)} }

func (h *fakeHold) acquire() {
	h.mu.Lock()
	h.acquires++
	h.mu.Unlock()
}

func (h *fakeHold) release() {
	h.mu.Lock()
	h.releases++
	h.mu.Unlock()
}

func (h *fakeHold) run(ctx context.Context) {
	select {
	case h.ran <- struct{}{}:
	default:
	}
	<-ctx.Done()
}

func (h *fakeHold) acquireCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acquires
}

func newTestWorker(t *testing.T, cfg WorkerConfig) (*Worker, *fakeHold) {
	t.Helper()
	c, err := NewClient(WithBaseURL("https://api.example.invalid"), WithAPIKey("mbx_test"), WithProjectHandle("test-project"))
	assert.NoError(t, err)
	w := c.NewWorker(cfg)
	fake := newFakeHold()
	w.keepWarm = fake
	return w, fake
}

// runUntilReturn runs the worker with an already-cancelled context. The
// keep-warm pin is established synchronously before any socket I/O, so Run
// returns promptly (the dial fails under the cancelled context) and the pin
// state is observable regardless.
func runUntilReturn(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker Run did not return under a cancelled context")
	}
}

func TestWorker_KeepWarmForLifetime_PinsHold(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	runUntilReturn(t, w)
	assert.Equal(t, 1, fake.acquireCount()) // one baseline pin, never released mid-run
}

func TestWorker_KeepWarmForLifetime_DefaultLeavesHoldPerJob(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{})
	runUntilReturn(t, w)
	assert.Equal(t, 0, fake.acquireCount()) // no baseline pin; hold stays per-job
}
