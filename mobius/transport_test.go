package mobius

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

// --- transport fixtures ----------------------------------------------------

// sequencedServer returns stubs from a list on each successive request to
// the matching path. Extra requests past the end 500.
func sequencedServer(t *testing.T, path string, responses []stubResponse) *httptest.Server {
	t.Helper()
	var idx atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		i := idx.Add(1) - 1
		if int(i) >= len(responses) {
			w.WriteHeader(500)
			return
		}
		resp := responses[i]
		if resp.body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, r)
	assert.NoError(t, err)
	return req
}

type recordingSleep struct {
	calls []time.Duration
}

func (r *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	r.calls = append(r.calls, d)
	return nil
}

// --- tests -----------------------------------------------------------------

func TestRetryingTransport_RetriesThenSucceeds(t *testing.T) {
	srv := sequencedServer(t, "/claim", []stubResponse{
		{status: 429, body: ""},
		{status: 200, body: `{"ok":true}`},
	})

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/claim", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 200)
	_ = resp.Body.Close()
	assert.Equal(t, len(rec.calls), 1)
	// Default backoff on first attempt with no Retry-After is 1s.
	assert.Equal(t, rec.calls[0], 1*time.Second)
}

func TestRetryingTransport_HonorsRetryAfterSeconds(t *testing.T) {
	var idx atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := idx.Add(1)
		if i == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 200)
	_ = resp.Body.Close()
	assert.Equal(t, len(rec.calls), 1)
	assert.Equal(t, rec.calls[0], 7*time.Second)
}

func TestRetryingTransport_ClampsRetryAfter(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9999")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 1, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	_, err := tp.RoundTrip(req)
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.Equal(t, len(rec.calls), 1)
	assert.Equal(t, rec.calls[0], MaxRetryBackoff)
}

func TestRetryingTransport_PostWithoutIdempotencyKey_SurfacesError(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", "3")
		w.Header().Set("X-RateLimit-Limit", "10000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.Header().Set("X-RateLimit-Scope", "key")
		w.Header().Set("X-RateLimit-Policy", "10000;w=18000")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 5, sleep: rec.sleep}
	req := newRequest(t, http.MethodPost, srv.URL+"/x", `{"a":1}`)

	_, err := tp.RoundTrip(req)
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.True(t, errors.Is(err, ErrRateLimited))
	assert.Equal(t, rle.RetryAfter, 3*time.Second)
	assert.Equal(t, rle.Limit, 10000)
	assert.Equal(t, rle.Remaining, 0)
	assert.Equal(t, rle.Scope, "key")
	assert.Equal(t, rle.Policy, "10000;w=18000")
	// Must not have retried, and must not have slept.
	assert.Equal(t, count.Load(), int32(1))
	assert.Equal(t, len(rec.calls), 0)
}

func TestRetryingTransport_PostWithIdempotencyKey_IsRetried(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodPost, srv.URL+"/x", `{"a":1}`)
	req.Header.Set("Idempotency-Key", "k1")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 200)
	_ = resp.Body.Close()
	assert.Equal(t, count.Load(), int32(2))
	assert.Equal(t, len(rec.calls), 1)
}

func TestRetryingTransport_MaxRetriesZero_SurfacesImmediately(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("X-RateLimit-Scope", "org")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 0, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	_, err := tp.RoundTrip(req)
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.Equal(t, rle.Scope, "org")
	assert.Equal(t, count.Load(), int32(1))
	assert.Equal(t, len(rec.calls), 0)
}

func TestRetryingTransport_ExhaustsBudget_ReturnsRateLimitError(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 2, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	_, err := tp.RoundTrip(req)
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.Equal(t, count.Load(), int32(3)) // initial + 2 retries
	assert.Equal(t, len(rec.calls), 2)
}

func TestRetryingTransport_503Retried_FinalPassesThrough(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(503)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 2, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 503)
	_ = resp.Body.Close()
	assert.Equal(t, count.Load(), int32(3))
	assert.Equal(t, len(rec.calls), 2)
}

func TestRetryingTransport_NonRetryableStatusPassesThrough(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(500)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 500)
	_ = resp.Body.Close()
	assert.Equal(t, count.Load(), int32(1))
	assert.Equal(t, len(rec.calls), 0)
}

func TestRetryingTransport_HTTPDateRetryAfter(t *testing.T) {
	fixedNow := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	future := fixedNow.Add(9 * time.Second).UTC().Format(http.TimeFormat)

	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", future)
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{
		MaxRetries: 3,
		sleep:      rec.sleep,
		now:        func() time.Time { return fixedNow },
	}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 200)
	_ = resp.Body.Close()
	assert.Equal(t, len(rec.calls), 1)
	assert.Equal(t, rec.calls[0], 9*time.Second)
}

func TestRetryingTransport_ExpBackoffWithoutHeader(t *testing.T) {
	var count atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	rec := &recordingSleep{}
	tp := &RetryingTransport{MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")

	_, err := tp.RoundTrip(req)
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.Equal(t, len(rec.calls), 3)
	assert.Equal(t, rec.calls[0], 1*time.Second)
	assert.Equal(t, rec.calls[1], 2*time.Second)
	assert.Equal(t, rec.calls[2], 4*time.Second)
}

func TestRetryingTransport_ContextCancelledDuringSleep(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	tp := &RetryingTransport{MaxRetries: 3}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := newRequest(t, http.MethodGet, srv.URL+"/x", "")
	req = req.WithContext(ctx)

	_, err := tp.RoundTrip(req)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRateLimitError_UnwrapsToSentinel(t *testing.T) {
	e := &RateLimitError{Scope: "key"}
	assert.True(t, errors.Is(e, ErrRateLimited))
}

// --- integration with Client: runtimeEmitEvents produces RateLimitError ---

func TestClient_EmitEvents_Returns_RateLimitError_WithHeaders(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-RateLimit-Limit", "100")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		w.Header().Set("X-RateLimit-Scope", "key")
		w.Header().Set("X-RateLimit-Policy", "100;w=60")
		w.WriteHeader(429)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"), WithProjectHandle("p1"), WithRetry(0))
	assert.NoError(t, err)
	err = c.runtimeEmitEvents(context.Background(), &runtimeJob{JobID: "j1", WorkerID: "w1", Attempt: 1}, []jobEventEntry{
		{Type: "x", Payload: map[string]any{"k": "v"}},
	})
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrRateLimited))
	var rle *RateLimitError
	assert.True(t, errors.As(err, &rle))
	assert.Equal(t, rle.Limit, 100)
	assert.Equal(t, rle.Scope, "key")
	assert.Equal(t, rle.RetryAfter, 5*time.Second)
}
