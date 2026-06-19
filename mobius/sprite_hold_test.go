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
}

func (r *taskRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.methods = append(r.methods, req.Method)
	r.mu.Unlock()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	// A job starts: the hold must establish the task (PUT).
	h.acquire()
	waitFor(t, func() bool { return rec.seen(http.MethodPut) }, "PUT on acquire")

	// The job finishes: the worker is idle, so the hold must be released (DELETE).
	h.release()
	waitFor(t, func() bool { return rec.seen(http.MethodDelete) }, "DELETE on release")
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
	h.run(ctx) // returns immediately
}
