package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
)

func TestAuthStatusReportsSavedCredentialAfterInjection(t *testing.T) {
	unsetEnv(t, "MOBIUS_API_KEY")
	unsetEnv(t, "MOBIUS_API_URL")
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())
	resetAppliedSavedCredential(t)
	srv := newAuthProbeServer(t, "mbc_saved.default", "/v1/projects/default/workflows", http.StatusOK)
	defer srv.Close()

	err := authstore.Save(&authstore.Credential{
		Source:       authstore.SourceBrowserLogin,
		APIURL:       srv.URL,
		Token:        "mbc_saved.default",
		CredentialID: "cred_123",
		OrgName:      "Example Org",
		UserEmail:    "user@example.invalid",
	})
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}

	applySavedCredential()
	result := newApp().Test(t, cli.TestArgs("auth", "status"))
	if !result.Success() {
		t.Fatalf("auth status failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "API URL: "+srv.URL) {
		t.Fatalf("stdout missing saved API URL:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Auth source: saved browser credential") {
		t.Fatalf("stdout missing saved credential source:\n%s", result.Stdout)
	}
	if strings.Contains(result.Stdout, "MOBIUS_API_KEY environment variable") {
		t.Fatalf("stdout incorrectly reported synthetic env var:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Auth check: GET /v1/projects/default/workflows -> HTTP 200") {
		t.Fatalf("stdout missing auth check:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Authenticated: yes (browser-based CLI credential verified)") {
		t.Fatalf("stdout missing browser credential auth status:\n%s", result.Stdout)
	}
}

func TestAuthStatusReportsRealAPIKeyEnv(t *testing.T) {
	srv := newAuthProbeServer(t, "mbx_env", "/v1/projects", http.StatusOK)
	defer srv.Close()
	t.Setenv("MOBIUS_API_KEY", "mbx_env")
	t.Setenv("MOBIUS_API_URL", srv.URL)
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())
	resetAppliedSavedCredential(t)

	err := authstore.Save(&authstore.Credential{
		Source: authstore.SourceBrowserLogin,
		APIURL: "https://saved.example.invalid",
		Token:  "mbx_saved",
	})
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}

	applySavedCredential()
	result := newApp().Test(t, cli.TestArgs("auth", "status"))
	if !result.Success() {
		t.Fatalf("auth status failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "API URL: "+srv.URL) {
		t.Fatalf("stdout missing env API URL:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Auth source: MOBIUS_API_KEY environment variable") {
		t.Fatalf("stdout missing env auth source:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Auth check: GET /v1/projects -> HTTP 200") {
		t.Fatalf("stdout missing auth check:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Authenticated: yes (raw API key verified)") {
		t.Fatalf("stdout missing raw API key auth status:\n%s", result.Stdout)
	}
}

func TestAuthStatusReportsRejectedCredential(t *testing.T) {
	srv := newAuthProbeServer(t, "mbx_env", "/v1/projects", http.StatusUnauthorized)
	defer srv.Close()
	t.Setenv("MOBIUS_API_KEY", "mbx_env")
	t.Setenv("MOBIUS_API_URL", srv.URL)
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())
	resetAppliedSavedCredential(t)

	result := newApp().Test(t, cli.TestArgs("auth", "status"))
	if !result.Success() {
		t.Fatalf("auth status failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Auth check: GET /v1/projects -> HTTP 401") {
		t.Fatalf("stdout missing rejected auth check:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Authenticated: no (raw API key rejected)") {
		t.Fatalf("stdout missing rejected auth status:\n%s", result.Stdout)
	}
}

func TestAuthProbePathUsesProjectScopedEndpointForPinnedCLIToken(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "raw api key", key: "mbx_env", want: "/v1/projects"},
		{name: "org scoped cli token", key: "mbc_token", want: "/v1/projects"},
		{name: "project pinned cli token", key: "mbc_token.my-project", want: "/v1/projects/my-project/workflows"},
		{name: "invalid project suffix", key: "mbc_token.BadProject", want: "/v1/projects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authProbePath(tt.key); got != tt.want {
				t.Fatalf("authProbePath(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func newAuthProbeServer(t *testing.T, wantToken, wantPath string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != wantPath {
			t.Errorf("unexpected probe request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+wantToken)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func resetAppliedSavedCredential(t *testing.T) {
	t.Helper()
	old := appliedSavedCredential
	appliedSavedCredential = nil
	t.Cleanup(func() {
		appliedSavedCredential = old
	})
}
