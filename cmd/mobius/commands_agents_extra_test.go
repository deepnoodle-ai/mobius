package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
)

// agentsTestServer wires up the three endpoints `agents create
// --install-credentials` and `agents issue-key` consume so we can run the
// hand-written commands end-to-end against an in-process httptest server.
//
// Each test driver gets its own server instance so behavior knobs can be
// flipped per test (e.g. forcing the api-keys POST to fail to verify the
// "agent created but key minting failed" path).
type agentsTestServer struct {
	URL                  string
	createAgentHits      atomic.Int32
	createAPIKeyHits     atomic.Int32
	getAgentHits         atomic.Int32
	lastAPIKeyBody       []byte
	lastAPIKeyQuery      string
	apiKeyResponseStatus int
}

func newAgentsTestServer(t *testing.T, agent map[string]any) *agentsTestServer {
	t.Helper()
	s := &agentsTestServer{apiKeyResponseStatus: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/agents") && r.Method == http.MethodPost:
			s.createAgentHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(agent)
		case strings.Contains(path, "/agents/") && r.Method == http.MethodGet:
			s.getAgentHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(agent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		s.createAPIKeyHits.Add(1)
		s.lastAPIKeyQuery = r.URL.RawQuery
		s.lastAPIKeyBody, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			t.Errorf("api-keys unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if s.apiKeyResponseStatus != http.StatusCreated {
			w.WriteHeader(s.apiKeyResponseStatus)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"nope"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                 "apk_minted",
			"name":               "claude-sub-cli",
			"key_prefix":         "mbx_abcd",
			"scope":              "org",
			"key":                "mbx_secrettoken.alpha",
			"service_account_id": "sa_abc",
			"project_id":         "prj_alpha",
		})
	})
	srv := httptest.NewServer(mux)
	s.URL = srv.URL
	t.Cleanup(srv.Close)
	return s
}

const fakeAgentJSON = `{"id":"agt_abc","name":"claude-sub","service_account_id":"sa_abc","status":"active","presence":"offline","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`

func newAgentResponse() map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(fakeAgentJSON), &out)
	return out
}

// TestAgentsCreateInstallCredentialsWritesProfile is the happy path: create
// an agent, mint a key, save the profile, and confirm all the right calls
// fired with the right shape.
func TestAgentsCreateInstallCredentialsWritesProfile(t *testing.T) {
	s := newAgentsTestServer(t, newAgentResponse())

	cfg := t.TempDir()
	t.Setenv("MOBIUS_CONFIG_DIR", cfg)
	resetActiveAuth(t)

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "create",
			"--api-url", s.URL,
			"--api-key", "mbx_user.alpha",
			"--project", "alpha",
			"--name", "claude-sub",
			"--install-credentials",
		),
	)
	if !result.Success() {
		t.Fatalf("agents create --install-credentials failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := s.createAgentHits.Load(); got != 1 {
		t.Fatalf("createAgent hits = %d, want 1", got)
	}
	if got := s.createAPIKeyHits.Load(); got != 1 {
		t.Fatalf("createAPIKey hits = %d, want 1", got)
	}
	if !strings.Contains(s.lastAPIKeyQuery, "project_id=alpha") {
		t.Fatalf("api-keys query missing project_id=alpha: %q", s.lastAPIKeyQuery)
	}

	var apiKeyBody map[string]any
	if err := json.Unmarshal(s.lastAPIKeyBody, &apiKeyBody); err != nil {
		t.Fatalf("api-keys body not JSON: %v\n%s", err, s.lastAPIKeyBody)
	}
	if got := apiKeyBody["service_account_id"]; got != "sa_abc" {
		t.Fatalf("api-keys body service_account_id = %v, want sa_abc", got)
	}
	if got := apiKeyBody["name"]; got != "claude-sub-cli" {
		t.Fatalf("api-keys body name = %v, want claude-sub-cli", got)
	}

	store, err := authstore.LoadStore()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	prof, ok := store.Profiles["claude-sub"]
	if !ok {
		t.Fatalf("expected profile %q, got %v", "claude-sub", profileNames(store))
	}
	if prof.AgentID != "agt_abc" || prof.AgentName != "claude-sub" {
		t.Fatalf("profile agent fields = (%q,%q), want (agt_abc,claude-sub)", prof.AgentID, prof.AgentName)
	}
	if prof.ServiceAccountID != "sa_abc" {
		t.Fatalf("profile service_account_id = %q, want sa_abc", prof.ServiceAccountID)
	}
	if prof.Token != "mbx_secrettoken.alpha" {
		t.Fatalf("profile token = %q, want mbx_secrettoken.alpha", prof.Token)
	}
	if prof.Source != authstore.SourceAgentInstall {
		t.Fatalf("profile source = %q, want %q", prof.Source, authstore.SourceAgentInstall)
	}
}

// TestAgentsCreateInstallCredentialsHardFailsOnProfileCollision pins the
// PRD's "do not silently overwrite" guarantee. The error must list the
// existing profile path and the recovery flags.
func TestAgentsCreateInstallCredentialsHardFailsOnProfileCollision(t *testing.T) {
	s := newAgentsTestServer(t, newAgentResponse())

	cfg := t.TempDir()
	t.Setenv("MOBIUS_CONFIG_DIR", cfg)
	resetActiveAuth(t)

	if err := authstore.PutProfile("claude-sub", authstore.Profile{
		Source: authstore.SourceBrowserLogin,
		APIURL: "http://elsewhere.invalid",
		Token:  "mbx_existing",
	}, false); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "create",
			"--api-url", s.URL,
			"--api-key", "mbx_user.alpha",
			"--project", "alpha",
			"--name", "claude-sub",
			"--install-credentials",
		),
	)
	if result.Success() {
		t.Fatalf("expected failure; got stdout: %s", result.Stdout)
	}
	if got := s.createAgentHits.Load(); got != 0 {
		t.Fatalf("createAgent hits = %d, want 0 — collision must be checked before the create call", got)
	}
	if got := s.createAPIKeyHits.Load(); got != 0 {
		t.Fatalf("createAPIKey hits = %d, want 0", got)
	}
	if result.Err == nil {
		t.Fatal("expected non-nil error from collision")
	}
	msg := result.Err.Error()
	if !strings.Contains(msg, "already exists") {
		t.Fatalf("expected 'already exists' in error: %s", msg)
	}
	if !strings.Contains(msg, "--profile-name") {
		t.Fatalf("expected recovery hint --profile-name in error: %s", msg)
	}
}

// TestAgentsCreateWithoutInstallSkipsKeyMint guards against accidentally
// minting a key when the user didn't ask for one.
func TestAgentsCreateWithoutInstallSkipsKeyMint(t *testing.T) {
	s := newAgentsTestServer(t, newAgentResponse())

	cfg := t.TempDir()
	t.Setenv("MOBIUS_CONFIG_DIR", cfg)
	resetActiveAuth(t)

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "create",
			"--api-url", s.URL,
			"--api-key", "mbx_user.alpha",
			"--project", "alpha",
			"--name", "claude-sub",
		),
	)
	if !result.Success() {
		t.Fatalf("agents create failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := s.createAgentHits.Load(); got != 1 {
		t.Fatalf("createAgent hits = %d, want 1", got)
	}
	if got := s.createAPIKeyHits.Load(); got != 0 {
		t.Fatalf("createAPIKey hits = %d, want 0", got)
	}
}

// TestAgentsIssueKeyPrintEmitsTokenAndSkipsProfile verifies --print is the
// off-machine alternative to --install-credentials and is mutually exclusive
// with it.
func TestAgentsIssueKeyPrintEmitsTokenAndSkipsProfile(t *testing.T) {
	s := newAgentsTestServer(t, newAgentResponse())

	cfg := t.TempDir()
	t.Setenv("MOBIUS_CONFIG_DIR", cfg)
	resetActiveAuth(t)

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "issue-key", "agt_abc",
			"--api-url", s.URL,
			"--api-key", "mbx_user.alpha",
			"--project", "alpha",
			"--print",
		),
	)
	if !result.Success() {
		t.Fatalf("agents issue-key --print failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	if got := s.getAgentHits.Load(); got != 1 {
		t.Fatalf("getAgent hits = %d, want 1", got)
	}
	if got := s.createAPIKeyHits.Load(); got != 1 {
		t.Fatalf("createAPIKey hits = %d, want 1", got)
	}
	if !strings.Contains(result.Stdout, "mbx_secrettoken.alpha") {
		t.Fatalf("expected raw token on stdout, got: %s", result.Stdout)
	}

	store, err := authstore.LoadStore()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if len(store.Profiles) != 0 {
		t.Fatalf("expected no profiles written for --print; got %v", profileNames(store))
	}
}

// TestAgentsIssueKeyExplicitProfileNameOverride confirms --profile-name
// supersedes the slugified-agent-name default.
func TestAgentsIssueKeyExplicitProfileNameOverride(t *testing.T) {
	s := newAgentsTestServer(t, newAgentResponse())

	cfg := t.TempDir()
	t.Setenv("MOBIUS_CONFIG_DIR", cfg)
	resetActiveAuth(t)

	result := newApp().Test(t,
		cli.TestArgs(
			"agents", "issue-key", "agt_abc",
			"--api-url", s.URL,
			"--api-key", "mbx_user.alpha",
			"--project", "alpha",
			"--profile-name", "claude-sub-laptop",
		),
	)
	if !result.Success() {
		t.Fatalf("agents issue-key failed: %v\nstderr: %s", result.Err, result.Stderr)
	}
	store, err := authstore.LoadStore()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if _, ok := store.Profiles["claude-sub-laptop"]; !ok {
		t.Fatalf("expected profile %q, got %v", "claude-sub-laptop", profileNames(store))
	}
	if _, ok := store.Profiles["claude-sub"]; ok {
		t.Fatalf("default-named profile should not have been written when --profile-name is set")
	}
}

func TestSlugifyProfileName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude-sub", "claude-sub"},
		{"Claude Sub", "claude-sub"},
		{"my agent / v2", "my-agent-v2"},
		{"   ", "agent"},
		{"---", "agent"},
		{"Owner.Bot", "owner-bot"},
		{"snake_case", "snake_case"},
	}
	for _, tc := range cases {
		if got := slugifyProfileName(tc.in); got != tc.want {
			t.Errorf("slugifyProfileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func profileNames(s *authstore.Store) []string {
	out := make([]string, 0, len(s.Profiles))
	for n := range s.Profiles {
		out = append(out, n)
	}
	return out
}
