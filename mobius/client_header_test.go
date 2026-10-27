package mobius

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime/debug"
	"testing"

	"github.com/gorilla/websocket"
)

// The server's X-Mobius-Client rules: a name of up to 32 [a-z0-9._-], then an
// optional version of up to 64 [A-Za-z0-9._+-].
var serverClientValue = regexp.MustCompile(`^[a-z0-9._-]{1,32}(/[A-Za-z0-9._+-]{1,64})?$`)

func TestClientSendsClientHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(ClientHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		opts []Option
		want string
	}{
		{name: "default", want: "sdk-go/" + sdkVersion()},
		{name: "override", opts: []Option{WithClientName("worker", "v0.10.0")}, want: "worker/v0.10.0"},
		{name: "no version", opts: []Option{WithClientName("cli", "")}, want: "cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			c, err := NewClient(append([]Option{WithBaseURL(srv.URL), WithAPIKey("k")}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			// A generated-client call and a hand-written request.
			if _, err := c.ac.GetOrgContextWithResponse(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.doWithHeaders(context.Background(), c.httpClient, http.MethodGet, "/v1/x", "", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0] != tc.want || got[1] != tc.want {
				t.Fatalf("X-Mobius-Client = %q, want %q on both requests", got, tc.want)
			}
			if !serverClientValue.MatchString(tc.want) {
				t.Fatalf("%q does not match the server's rules", tc.want)
			}
		})
	}
}

func TestWorkerSocketSendsClientHeader(t *testing.T) {
	headers := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name    string
		opts    []Option
		version string
		want    string
	}{
		{name: "sdk worker names itself", version: "v1.2.3", want: "worker/v1.2.3"},
		{name: "sdk worker without a version", want: "worker/" + sdkVersion()},
		{name: "program name wins", opts: []Option{WithClientName("worker", "v0.10.0")}, version: "ignored", want: "worker/v0.10.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(append([]Option{WithBaseURL(srv.URL), WithAPIKey("k")}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			socket, _, err := c.dialWorkerSocket(context.Background(), tc.version)
			if err != nil {
				t.Fatal(err)
			}
			_ = socket.conn.Close()
			h := <-headers
			if got := h.Get(ClientHeader); got != tc.want {
				t.Fatalf("X-Mobius-Client = %q, want %q", got, tc.want)
			}
			if got := h.Get("Authorization"); got != "Bearer k" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}
}

func TestModuleVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{"dependency", debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.5.0"}}}, "v0.5.0"},
		{"replaced", debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.5.0", Replace: &debug.Module{Version: "v0.5.1"}}}}, "v0.5.1"},
		{"main module", debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.6.0"}}, "v0.6.0"},
		{"local checkout", debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "dev"},
		{"absent", debug.BuildInfo{Main: debug.Module{Path: "example.com/app", Version: "v1.0.0"}}, "dev"},
	} {
		if got := moduleVersion(&tc.info); got != tc.want {
			t.Errorf("%s: moduleVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
