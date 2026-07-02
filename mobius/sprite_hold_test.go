package mobius

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// taskRecorder is a stand-in for the Sprites management socket that records the
// HTTP methods (and method+path pairs) the hold sends to the Tasks API.
type taskRecorder struct {
	mu       sync.Mutex
	methods  []string
	requests []string // "METHOD /path"
	status   int
}

func (r *taskRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.methods = append(r.methods, req.Method)
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	status := r.status
	r.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	switch req.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (r *taskRecorder) seen(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.methods {
		if m == method {
			return true
		}
	}
	return false
}

func (r *taskRecorder) seenRequest(method, path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := method + " " + path
	for _, req := range r.requests {
		if req == want {
			return true
		}
	}
	return false
}

func (r *taskRecorder) setStatus(status int) {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
}

// serveTaskSocket starts an HTTP server on a Unix socket and returns its path.
func serveTaskSocket(t *testing.T, rec *taskRecorder) string {
	t.Helper()
	// Keep the path short — Unix socket paths are capped (~104 bytes on macOS).
	dir, err := os.MkdirTemp("/tmp", "sphold")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: rec}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSpriteHoldEstablishesAndReleases(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = 50 * time.Millisecond
	h.releaseGrace = 20 * time.Millisecond // release promptly once idle

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.run(ctx, holdRunOptions{}) }()

	// A job starts: the hold must establish the task (PUT).
	h.acquire()
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "PUT on acquire")

	// The job finishes: after the release grace elapses with no new work, the
	// hold must be released (DELETE).
	h.release()
	waitFor(t, func() bool { return rec.seen(http.MethodDelete) }, "DELETE after release grace")
}

// TestSpriteHoldGraceBridgesInterJobGap proves the H-2 fix: a job finishing does
// not immediately drop the hold. A next job arriving within the release grace
// keeps the Sprite warm (no DELETE), so a reused environment doesn't pause in
// the agent's inter-tool-call think-gap and strand the next job.
func TestSpriteHoldGraceBridgesInterJobGap(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = time.Hour       // don't let refresh interfere
	h.releaseGrace = time.Second // long enough to re-acquire within

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.run(ctx, holdRunOptions{}) }()

	h.acquire() // job 1 starts
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "PUT on first acquire")

	h.release() // job 1 finishes; grace window opens
	h.acquire() // job 2 (next tool call) arrives within the grace
	h.release() // job 2 finishes

	// The hold must NOT have been released while work kept arriving within grace.
	time.Sleep(200 * time.Millisecond)
	if rec.seen(http.MethodDelete) {
		t.Fatal("hold was released during the inter-job gap; the Sprite could pause and strand the next job")
	}
}

func TestSpriteHoldRequiredFailsClosedWhenTaskCannotBeEstablished(t *testing.T) {
	rec := &taskRecorder{status: http.StatusInternalServerError}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.retryDelay = time.Millisecond // keep the required-mode retries fast

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.acquire()
	err := h.run(ctx, holdRunOptions{Required: true})
	if err == nil {
		t.Fatal("required hold returned nil error")
	}
	if !rec.seen(http.MethodPut) {
		t.Fatal("required hold did not try to establish the Sprite task")
	}
}

func TestSpriteHoldRequiredRefreshFailureDoesNotDeleteExistingTask(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = 20 * time.Millisecond
	h.retryDelay = time.Millisecond // keep the required-mode retries fast

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.run(ctx, holdRunOptions{Required: true}) }()

	h.acquire()
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "initial PUT")
	rec.setStatus(http.StatusInternalServerError)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("required hold returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for required refresh failure")
	}
	if rec.seen(http.MethodDelete) {
		t.Fatal("required refresh failure deleted an existing task instead of leaving its remaining expiry for service restart")
	}
}

// TestSpriteHoldPerInstanceTaskNamesAreIndependent is the regression for the
// shared-task-name hazard: two workers on one Sprite (a pool, or two
// processes) each maintain their own task, so one going idle must release only
// its own hold — not yank the task out from under the other mid-job.
func TestSpriteHoldPerInstanceTaskNamesAreIndependent(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	a := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-a", DefaultKeepWarmWindow)
	b := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-b", DefaultKeepWarmWindow)
	if a == nil || b == nil {
		t.Fatal("expected holds for a real socket")
	}
	a.interval, b.interval = time.Hour, time.Hour
	a.releaseGrace = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.run(ctx, holdRunOptions{}) }()
	go func() { _ = b.run(ctx, holdRunOptions{}) }()

	a.acquire() // worker A starts a job
	b.acquire() // worker B starts a longer job
	waitFor(t, func() bool {
		return rec.seenRequest(http.MethodPut, "/v1/tasks/mobius-worker-a") &&
			rec.seenRequest(http.MethodPut, "/v1/tasks/mobius-worker-b")
	}, "both per-instance tasks established")

	a.release() // worker A goes idle; its grace elapses
	waitFor(t, func() bool {
		return rec.seenRequest(http.MethodDelete, "/v1/tasks/mobius-worker-a")
	}, "worker A's task released")

	if rec.seenRequest(http.MethodDelete, "/v1/tasks/mobius-worker-b") {
		t.Fatal("worker A's idle release deleted worker B's task; B's in-flight job can be paused mid-work")
	}
}

// TestSpriteHoldOnDemandWindowReleasesImmediately covers keep-warm mode B: a
// zero window drops the hold the moment the worker goes idle, with no grace.
func TestSpriteHoldOnDemandWindowReleasesImmediately(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", 0)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.run(ctx, holdRunOptions{}) }()

	h.acquire()
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "PUT on acquire")
	h.release()
	waitFor(t, func() bool { return rec.seen(http.MethodDelete) }, "immediate DELETE on idle")
}

// TestSpriteHoldPokeReestablishesHeldTask covers the resume-from-pause path: a
// held task may have expired while the VM was suspended, so poke() must force
// an immediate re-PUT rather than waiting for the next scheduled refresh.
func TestSpriteHoldPokeReestablishesHeldTask(t *testing.T) {
	rec := &taskRecorder{}
	sock := serveTaskSocket(t, rec)

	h := newSpriteHoldWithPath(sock, slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = time.Hour // no scheduled refresh; only poke can re-PUT

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.run(ctx, holdRunOptions{}) }()

	h.acquire()
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "PUT on acquire")
	before := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.requests)
	}()

	h.poke()
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.requests) > before
	}, "re-PUT after poke")
}

func TestNewSpriteHoldOffSprite(t *testing.T) {
	// A path that isn't a Unix socket means we're not inside a Sprite: the
	// constructor returns nil so detectHold falls back to a no-op.
	regular := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := newSpriteHoldWithPath(regular, nil, "mobius-worker-test", DefaultKeepWarmWindow); h != nil {
		t.Fatalf("expected nil hold for a regular file, got %v", h)
	}
	if h := newSpriteHoldWithPath(filepath.Join(t.TempDir(), "missing.sock"), nil, "mobius-worker-test", DefaultKeepWarmWindow); h != nil {
		t.Fatalf("expected nil hold for a missing path, got %v", h)
	}
}

func TestDetectHoldDefaultsToNoop(t *testing.T) {
	// In the test environment there is no Sprite management socket, so detectHold
	// must return a usable no-op hold whose methods don't panic.
	h := detectHold(slog.Default(), "mobius-worker-test", DefaultKeepWarmWindow)
	if _, ok := h.(noopHold); !ok {
		t.Skipf("unexpected hold %T (a Sprite socket exists on this host?)", h)
	}
	h.acquire()
	h.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = h.run(ctx, holdRunOptions{}) // returns immediately
}
