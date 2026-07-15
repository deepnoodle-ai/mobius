package mobius

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

type stubResponse struct {
	status int
	body   string
}

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errorReadCloser struct {
	err error
}

func (r *errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *errorReadCloser) Close() error             { return nil }

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

func TestRetryingTransport_ReplaySafePostReusesBodyAndKeyAcrossNetworkAnd503Failures(t *testing.T) {
	var calls int
	var bodies []string
	var keys []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)
		bodies = append(bodies, string(body))
		keys = append(keys, req.Header.Get("Idempotency-Key"))
		switch calls {
		case 1:
			return nil, errors.New("connection reset by peer")
		case 2:
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Retry-After": []string{"0"}},
				Body:       http.NoBody,
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"accepted":true}`)),
			}, nil
		}
	})
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodPost, "http://example.test/invoke", `{"message":"same"}`)
	req.Header.Set("Idempotency-Key", "k1")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, calls, 3)
	assert.Equal(t, bodies, []string{`{"message":"same"}`, `{"message":"same"}`, `{"message":"same"}`})
	assert.Equal(t, keys, []string{"k1", "k1", "k1"})
}

func TestRetryingTransport_RetriesUnreadableAndInvalidReplaySafeJSONAcknowledgements(t *testing.T) {
	var calls int
	base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls++
		body := io.ReadCloser(io.NopCloser(strings.NewReader(`{"accepted":true}`)))
		if calls == 1 {
			body = &errorReadCloser{err: errors.New("unexpected EOF")}
		} else if calls == 2 {
			body = io.NopCloser(strings.NewReader(`{`))
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}, nil
	})
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 2, sleep: rec.sleep}
	req := newRequest(t, http.MethodPost, "http://example.test/invoke", `{}`)
	req.Header.Set("Idempotency-Key", "k1")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, string(body), `{"accepted":true}`)
	assert.Equal(t, calls, 3)
	assert.Equal(t, len(rec.calls), 2)
}

func TestRetryingTransport_DoesNotReadOrReinvokeSSEAfterResponseStart(t *testing.T) {
	var calls int
	streamErr := errors.New("stream disconnected")
	base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &errorReadCloser{err: streamErr},
		}, nil
	})
	tp := &RetryingTransport{Base: base, MaxRetries: 3, sleep: (&recordingSleep{}).sleep}
	req := newRequest(t, http.MethodPost, "http://example.test/invoke", `{}`)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Idempotency-Key", "k1")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	assert.ErrorIs(t, err, streamErr)
	_ = resp.Body.Close()
	assert.Equal(t, calls, 1)
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

// scriptedTransport fails the first `failures` round trips with `err` (no HTTP
// response, modeling a transport-level fault such as a connection reset), then
// serves a 200 with a small JSON body.
type scriptedTransport struct {
	calls    atomic.Int32
	failures int
	err      error
}

func (s *scriptedTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	n := int(s.calls.Add(1))
	if n <= s.failures {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		Header:     make(http.Header),
	}, nil
}

func TestRetryingTransport_RetriesTransportErrorThenSucceeds(t *testing.T) {
	base := &scriptedTransport{failures: 2, err: errors.New("connection reset by peer")}
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 3, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, "http://example.test/runs/run_x", "")

	resp, err := tp.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, 200)
	_ = resp.Body.Close()
	assert.Equal(t, base.calls.Load(), int32(3))
	assert.Equal(t, len(rec.calls), 2)
	assert.Equal(t, rec.calls[0], 1*time.Second)
	assert.Equal(t, rec.calls[1], 2*time.Second)
}

func TestRetryingTransport_TransportErrorExhaustsBudget(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	base := &scriptedTransport{failures: 100, err: sentinel}
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 2, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, "http://example.test/runs/run_x", "")

	_, err := tp.RoundTrip(req)
	assert.ErrorIs(t, err, sentinel)
	// attempts 0 and 1 sleep then retry; attempt 2 is out of budget.
	assert.Equal(t, len(rec.calls), 2)
	assert.Equal(t, base.calls.Load(), int32(3))
}

func TestRetryingTransport_TransportErrorNotRetriedForNonIdempotent(t *testing.T) {
	sentinel := errors.New("unexpected EOF")
	base := &scriptedTransport{failures: 100, err: sentinel}
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 3, sleep: rec.sleep}
	// POST without an Idempotency-Key is not safe to replay.
	req := newRequest(t, http.MethodPost, "http://example.test/runs", `{"a":1}`)

	_, err := tp.RoundTrip(req)
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, len(rec.calls), 0)
	assert.Equal(t, base.calls.Load(), int32(1))
}

func TestRetryingTransport_TransportErrorMaxRetriesZero(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	base := &scriptedTransport{failures: 100, err: sentinel}
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 0, sleep: rec.sleep}
	req := newRequest(t, http.MethodGet, "http://example.test/runs/run_x", "")

	_, err := tp.RoundTrip(req)
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, len(rec.calls), 0)
	assert.Equal(t, base.calls.Load(), int32(1))
}

func TestRetryingTransport_TransportErrorContextDoneStops(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	base := &scriptedTransport{failures: 100, err: sentinel}
	rec := &recordingSleep{}
	tp := &RetryingTransport{Base: base, MaxRetries: 3, sleep: rec.sleep}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := newRequest(t, http.MethodGet, "http://example.test/runs/run_x", "")
	req = req.WithContext(ctx)

	_, err := tp.RoundTrip(req)
	// The underlying transport error surfaces; the done context stops retries
	// before any backoff sleep.
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, len(rec.calls), 0)
	assert.Equal(t, base.calls.Load(), int32(1))
}
