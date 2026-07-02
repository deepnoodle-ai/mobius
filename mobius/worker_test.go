package mobius

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

// fakeHold is a counting [hold] used to observe acquire/release bracketing.
type fakeHold struct {
	mu       sync.Mutex
	acquires int
	releases int
	ensures  int
	ran      chan struct{}
	ensureOK bool
	runErr   error
}

func newFakeHold() *fakeHold { return &fakeHold{ran: make(chan struct{}, 1), ensureOK: true} }

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

func (h *fakeHold) run(ctx context.Context, _ holdRunOptions) error {
	select {
	case h.ran <- struct{}{}:
	default:
	}
	if h.runErr != nil {
		return h.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (h *fakeHold) ensure(context.Context) bool {
	h.mu.Lock()
	h.ensures++
	ok := h.ensureOK
	h.mu.Unlock()
	return ok
}

func (h *fakeHold) poke() {}

func (h *fakeHold) pins() bool { return true }

func (h *fakeHold) acquireCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acquires
}

func (h *fakeHold) ensureCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ensures
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

func TestWorker_Capabilities_NoneWithoutKeepWarm(t *testing.T) {
	w, _ := newTestWorker(t, WorkerConfig{})
	assert.Equal(t, 0, len(w.capabilities()))
}

func TestWorker_Capabilities_LifetimeAdvertisedBeforeEstablished(t *testing.T) {
	w, _ := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	// Before the hold is established, only the posture (not the confirmation) is
	// advertised.
	assert.Equal(t, []string{capabilityKeepWarmLifetime}, w.capabilities())
}

func TestWorker_Capabilities_EstablishedAfterRun(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	runUntilReturn(t, w)
	assert.Equal(t, 1, fake.ensureCount())
	assert.Equal(t, []string{capabilityKeepWarmLifetime, capabilityKeepWarmEstablished}, w.capabilities())
}

func TestWorker_KeepWarmForLifetime_PinsHold(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	runUntilReturn(t, w)
	assert.Equal(t, 1, fake.acquireCount()) // one baseline pin, never released mid-run
	assert.Equal(t, 1, fake.ensureCount())  // established synchronously at startup
}

func TestWorker_KeepWarmForLifetime_DefaultLeavesHoldPerJob(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{})
	runUntilReturn(t, w)
	assert.Equal(t, 0, fake.acquireCount()) // no baseline pin; hold stays per-job
	assert.Equal(t, 0, fake.ensureCount())  // no synchronous pin without lifetime keep-warm
}

func TestWorker_KeepWarmForLifetime_FailsWhenInitialHoldMissing(t *testing.T) {
	w, fake := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	w.holdRetryDelay = time.Millisecond // keep the startup retries fast
	fake.ensureOK = false

	err := w.Run(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "required keep-warm hold")
	assert.Equal(t, 1, fake.acquireCount())
	// A required hold gets a few in-process attempts before failing closed, so
	// a single transient Tasks-API blip doesn't bounce the whole process.
	assert.Equal(t, 3, fake.ensureCount())
}

func TestWorker_KeepWarmForLifetime_ReturnsRequiredHoldFailure(t *testing.T) {
	holdErr := errors.New("required hold refresh failed")
	w, fake := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	fake.runErr = holdErr

	err := w.Run(context.Background())
	assert.ErrorIs(t, err, holdErr)
	assert.Equal(t, 1, fake.acquireCount())
	assert.Equal(t, 1, fake.ensureCount())
}

func TestWorkerConfig_KeepWarmWindowMapping(t *testing.T) {
	// The deprecated bool maps onto the forever window and stays fail-closed.
	w, _ := newTestWorker(t, WorkerConfig{KeepWarmForLifetime: true})
	assert.True(t, w.config.keepWarmLifetime())
	assert.True(t, w.config.keepWarmRequired())
	assert.Equal(t, []string{capabilityKeepWarmLifetime}, w.capabilities())

	// KeepWarmForever is the same mode, expressed on the one dial.
	w2, _ := newTestWorker(t, WorkerConfig{KeepWarmWindow: KeepWarmForever})
	assert.True(t, w2.config.keepWarmLifetime())
	assert.Equal(t, []string{capabilityKeepWarmLifetime}, w2.capabilities())

	// Window → release-grace mapping: zero means the default window, the
	// on-demand sentinel means release immediately, anything else passes
	// through. Only forever mode is implicitly required.
	cfg := WorkerConfig{}
	assert.Equal(t, DefaultKeepWarmWindow, cfg.effectiveKeepWarmWindow())
	cfg.KeepWarmWindow = KeepWarmOnDemand
	assert.Equal(t, time.Duration(0), cfg.effectiveKeepWarmWindow())
	cfg.KeepWarmWindow = 5 * time.Minute
	assert.Equal(t, 5*time.Minute, cfg.effectiveKeepWarmWindow())
	assert.True(t, !cfg.keepWarmLifetime())
	assert.True(t, !cfg.keepWarmRequired())
	cfg.KeepWarmRequired = true
	assert.True(t, cfg.keepWarmRequired())
}

func TestReconnectBackoff_GrowsAndCaps(t *testing.T) {
	base := 2 * time.Second
	within := func(d, target time.Duration) bool {
		return d >= time.Duration(float64(target)*0.8) && d <= time.Duration(float64(target)*1.2)
	}
	if d := reconnectBackoff(base, 1); !within(d, base) {
		t.Fatalf("first retry should be ~base (±20%% jitter), got %s", d)
	}
	if d := reconnectBackoff(base, 3); !within(d, 8*time.Second) {
		t.Fatalf("fourth consecutive failure should be ~8s, got %s", d)
	}
	if d := reconnectBackoff(base, 50); !within(d, maxWorkerReconnectDelay) {
		t.Fatalf("backoff should cap at ~%s, got %s", maxWorkerReconnectDelay, d)
	}
}

func TestApplyLeaseConfig_DerivesClaimTimeout(t *testing.T) {
	// The claim deadline must straddle the server's long-poll window (its
	// heartbeat cadence) and the reaper TTL; derive it instead of hoping the
	// constant fits.
	w, _ := newTestWorker(t, WorkerConfig{})
	w.applyLeaseConfig(api.WorkerSocketLeaseConfig{HeartbeatCadenceSeconds: 45})
	assert.Equal(t, 90*time.Second, w.claimResponseTimeout)

	// A tiny cadence keeps a sane floor.
	w2, _ := newTestWorker(t, WorkerConfig{})
	w2.applyLeaseConfig(api.WorkerSocketLeaseConfig{HeartbeatCadenceSeconds: 5})
	assert.Equal(t, 30*time.Second, w2.claimResponseTimeout)

	// An explicit override is left alone.
	w3, _ := newTestWorker(t, WorkerConfig{})
	w3.claimResponseTimeout = 100 * time.Millisecond
	w3.applyLeaseConfig(api.WorkerSocketLeaseConfig{HeartbeatCadenceSeconds: 45})
	assert.Equal(t, 100*time.Millisecond, w3.claimResponseTimeout)

	// No cadence reported: keep the default.
	w4, _ := newTestWorker(t, WorkerConfig{})
	w4.applyLeaseConfig(api.WorkerSocketLeaseConfig{})
	assert.Equal(t, defaultClaimResponseTimeout, w4.claimResponseTimeout)
}

func noopGenerator(Context, GenerationJob, GenerationEmitter) (map[string]any, error) {
	return nil, nil
}

func TestWorker_AdvertisedModels_RegisteredGeneratorIsAdvertised(t *testing.T) {
	// A registered concrete generator is advertised even without a matching
	// WorkerConfig.Models entry, so the catalog can never list a model the
	// worker can't serve.
	w, _ := newTestWorker(t, WorkerConfig{})
	w.RegisterGenerator("ollama", "llama3", noopGenerator)
	assert.Equal(t, []ModelCapability{{Provider: "ollama", Model: "llama3"}}, w.advertisedModels())
}

func TestWorker_AdvertisedModels_MergesConfigAndDedupes(t *testing.T) {
	// config.Models comes first; a generator for the same pair does not produce
	// a duplicate, and a distinct registered generator is appended.
	w, _ := newTestWorker(t, WorkerConfig{
		Models: []ModelCapability{{Provider: "ollama", Model: "llama3"}},
	})
	w.RegisterGenerator("ollama", "llama3", noopGenerator)
	w.RegisterGenerator("ollama", "qwen2", noopGenerator)
	assert.Equal(t, []ModelCapability{
		{Provider: "ollama", Model: "llama3"},
		{Provider: "ollama", Model: "qwen2"},
	}, w.advertisedModels())
}

func TestWorker_AdvertisedModels_WildcardNotAdvertised(t *testing.T) {
	// A "*" wildcard generator has no concrete model id to advertise, so it is
	// excluded; only the explicit config entry is announced.
	w, _ := newTestWorker(t, WorkerConfig{
		Models: []ModelCapability{{Provider: "ollama", Model: "llama3"}},
	})
	w.RegisterGenerator("ollama", "*", noopGenerator)
	assert.Equal(t, []ModelCapability{{Provider: "ollama", Model: "llama3"}}, w.advertisedModels())
}
