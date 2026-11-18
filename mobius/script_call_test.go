package mobius

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A script call can run an action for minutes before it answers, so its
// client sets no response-header timeout, and it never retries: a repeated
// POST could run the action twice.
func TestCreateScriptCallWaitsAndNeverRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Mobius-Lease-Token") != "lease_1" {
			t.Errorf("lease header = %q", r.Header.Get("X-Mobius-Lease-Token"))
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, err := NewClient(WithBaseURL(srv.URL), WithAPIKey("mbx_test"))
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.callClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("call transport = %T, want a plain *http.Transport with no retry wrapper", c.callClient.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 || c.callClient.Timeout != 0 {
		t.Fatalf("call client timeouts = %v, %v; want none", transport.ResponseHeaderTimeout, c.callClient.Timeout)
	}
	if _, err := c.CreateScriptCall(context.Background(), "env_1", "lease_1", ScriptCallRequest{Action: "a.b"}); err == nil {
		t.Fatal("want an error for a 502")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}
