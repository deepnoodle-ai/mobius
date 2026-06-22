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
// HTTP methods the hold sends to the Tasks API.
type taskRecorder struct {
	mu      sync.Mutex
	methods []string
	status  int
}

func (r *taskRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.methods = append(r.methods, req.Method)
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

	h := newSpriteHoldWithPath(sock, slog.Default())
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

	h := newSpriteHoldWithPath(sock, slog.Default())
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

	h := newSpriteHoldWithPath(sock, slog.Default())
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}

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

	h := newSpriteHoldWithPath(sock, slog.Default())
	if h == nil {
		t.Fatal("expected a hold for a real socket, got nil")
	}
	h.interval = 20 * time.Millisecond

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

func TestNewSpriteHoldOffSprite(t *testing.T) {
	// A path that isn't a Unix socket means we're not inside a Sprite: the
	// constructor returns nil so detectHold falls back to a no-op.
	regular := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := newSpriteHoldWithPath(regular, nil); h != nil {
		t.Fatalf("expected nil hold for a regular file, got %v", h)
	}
	if h := newSpriteHoldWithPath(filepath.Join(t.TempDir(), "missing.sock"), nil); h != nil {
		t.Fatalf("expected nil hold for a missing path, got %v", h)
	}
}

func TestDetectHoldDefaultsToNoop(t *testing.T) {
	// In the test environment there is no Sprite management socket, so detectHold
	// must return a usable no-op hold whose methods don't panic.
	h := detectHold(slog.Default())
	if _, ok := h.(noopHold); !ok {
		t.Skipf("unexpected hold %T (a Sprite socket exists on this host?)", h)
	}
	h.acquire()
	h.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = h.run(ctx, holdRunOptions{}) // returns immediately
}
