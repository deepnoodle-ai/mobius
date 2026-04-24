package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
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
		Source:        authstore.SourceBrowserLogin,
		APIURL:        srv.URL,
		Token:         "mbc_saved",
		CredentialID:  "cred_123",
		OrgName:       "Example Org",
		ProjectID:     "prj_123",
		ProjectHandle: "default",
		UserEmail:     "user@example.invalid",
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
	if !strings.Contains(result.Stdout, "Auth source: saved profile") {
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

func TestAddDeviceCodeClientInfo(t *testing.T) {
	form := url.Values{}
	addDeviceCodeClientInfo(form)

	if got := form.Get("mobius_client"); got != "mobius-cli" {
		t.Fatalf("mobius_client = %q, want mobius-cli", got)
	}
	if got := form.Get("mobius_client_version"); got == "" {
		t.Fatalf("mobius_client_version is empty")
	}
	if got := form.Get("mobius_client_os"); got != runtime.GOOS {
		t.Fatalf("mobius_client_os = %q, want %q", got, runtime.GOOS)
	}
	if got := form.Get("mobius_client_arch"); got != runtime.GOARCH {
		t.Fatalf("mobius_client_arch = %q, want %q", got, runtime.GOARCH)
	}
}

func TestAuthLoginDoesNotRequestProjectFromEnvOrDefault(t *testing.T) {
	srv, requestedProject := newDeviceLoginServer(t)
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs("auth", "login", "--api-url", srv.URL, "--no-browser"),
		cli.TestEnv("MOBIUS_CONFIG_DIR", t.TempDir()),
		cli.TestEnv("MOBIUS_PROJECT", "env-project"),
	)
	if !result.Success() {
		t.Fatalf("auth login failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := <-requestedProject; got != "" {
		t.Fatalf("requested project = %q, want empty", got)
	}
}

func TestAuthLoginRequestsExplicitProject(t *testing.T) {
	srv, requestedProject := newDeviceLoginServer(t)
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs("auth", "login", "--api-url", srv.URL, "--no-browser", "--project", "cli-project"),
		cli.TestEnv("MOBIUS_CONFIG_DIR", t.TempDir()),
		cli.TestEnv("MOBIUS_PROJECT", "env-project"),
	)
	if !result.Success() {
		t.Fatalf("auth login failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := <-requestedProject; got != "cli-project" {
		t.Fatalf("requested project = %q, want cli-project", got)
	}
}

func TestAuthLoginDisplaysVerificationURLFromCustomAPIURL(t *testing.T) {
	srv, requestedProject := newDeviceLoginServer(t)
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs("auth", "login", "--api-url", srv.URL, "--no-browser"),
		cli.TestEnv("MOBIUS_CONFIG_DIR", t.TempDir()),
	)
	if !result.Success() {
		t.Fatalf("auth login failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Open this URL:          "+srv.URL+"/auth/device?code=ABCD-EFGH") {
		t.Fatalf("stdout missing custom verification URL:\n%s", result.Stdout)
	}
	if strings.Contains(result.Stdout, "https://example.invalid/auth/device") {
		t.Fatalf("stdout used server-provided verification origin:\n%s", result.Stdout)
	}
	if got := <-requestedProject; got != "" {
		t.Fatalf("requested project = %q, want empty", got)
	}
}

func TestGeneratedCommandUsesCustomAPIURL(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	result := newApp().Test(t,
		cli.TestArgs("projects", "list", "--api-url", srv.URL, "--api-key", "mbx_test"),
		cli.TestEnv("MOBIUS_CONFIG_DIR", t.TempDir()),
	)
	if !result.Success() {
		t.Fatalf("projects list failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := <-seen; got != "GET /v1/projects Bearer mbx_test" {
		t.Fatalf("request = %q, want custom server projects request", got)
	}
}

func TestDeviceVerificationURLKeepsDefaultServerURL(t *testing.T) {
	ch := &deviceCodeResponse{
		UserCode:                "ABCD-EFGH",
		VerificationURIComplete: "https://mobiusops.ai/auth/device?code=ABCD-EFGH",
	}
	if got := deviceVerificationURL("https://api.mobiusops.ai", ch); got != ch.VerificationURIComplete {
		t.Fatalf("deviceVerificationURL() = %q, want %q", got, ch.VerificationURIComplete)
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

func newDeviceLoginServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	requestedProject := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/code":
			if r.Method != http.MethodPost {
				t.Errorf("device code method = %s, want POST", r.Method)
				http.NotFound(w, r)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			requestedProject <- r.PostFormValue("mobius_requested_project_handle")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_code":"dev_123","user_code":"ABCD-EFGH","verification_uri":"https://example.invalid/auth/device","verification_uri_complete":"https://example.invalid/auth/device?code=ABCD-EFGH","expires_in":60,"interval":1}`))
		case "/v1/auth/device/token":
			if r.Method != http.MethodPost {
				t.Errorf("device token method = %s, want POST", r.Method)
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"mbc_login","token_type":"Bearer","credential_id":"cred_login"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return srv, requestedProject
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
