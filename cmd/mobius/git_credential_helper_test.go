package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

func TestParseGitCredentialRequest(t *testing.T) {
	in := "protocol=https\nhost=github.com\npath=deepnoodle-ai/mobius.git\n\ntrailing=ignored\n"
	req, err := parseGitCredentialRequest(strings.NewReader(in))
	assert.NoError(t, err)
	assert.Equal(t, "https", req.Protocol)
	assert.Equal(t, "github.com", req.Host)
	assert.Equal(t, "deepnoodle-ai/mobius.git", req.Path)
}

func TestGithubRepoFromCredentialRequest(t *testing.T) {
	cases := []struct {
		name string
		req  gitCredentialRequest
		want string
		ok   bool
	}{
		{"https github with .git path", gitCredentialRequest{Host: "github.com", Path: "owner/repo.git"}, "owner/repo", true},
		{"leading slash path", gitCredentialRequest{Host: "github.com", Path: "/owner/repo"}, "owner/repo", true},
		{"host case-insensitive", gitCredentialRequest{Host: "GitHub.com", Path: "owner/repo"}, "owner/repo", true},
		{"non-github host", gitCredentialRequest{Host: "gitlab.com", Path: "owner/repo"}, "", false},
		{"missing path", gitCredentialRequest{Host: "github.com"}, "", false},
		{"path without repo", gitCredentialRequest{Host: "github.com", Path: "owner"}, "", false},
		{"nested path rejected", gitCredentialRequest{Host: "github.com", Path: "owner/repo/extra"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := githubRepoFromCredentialRequest(tc.req)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

type fakeBroker struct {
	calledRepo string
	calledOp   string
	cred       *mobius.EnvironmentGitCredential
	err        error
}

func (f *fakeBroker) CreateEnvironmentGitCredential(_ context.Context, _ string, req mobius.EnvironmentGitCredentialRequest) (*mobius.EnvironmentGitCredential, error) {
	f.calledRepo = req.RepoFullName
	f.calledOp = req.Operation
	return f.cred, f.err
}

func TestRunGitCredentialHelperBrokersAndWrites(t *testing.T) {
	expiry := time.Unix(1_900_000_000, 0).UTC()
	broker := &fakeBroker{cred: &mobius.EnvironmentGitCredential{
		Username:  "x-access-token",
		Token:     "ghs_prototype",
		ExpiresAt: expiry,
	}}
	var out strings.Builder
	err := runGitCredentialHelper(context.Background(), &out, broker, "env_123", gitCredentialRequest{Host: "github.com", Path: "owner/repo.git"})
	assert.NoError(t, err)
	// The helper always requests push; the broker decides read vs write.
	assert.Equal(t, "owner/repo", broker.calledRepo)
	assert.Equal(t, "push", broker.calledOp)
	got := out.String()
	assert.True(t, strings.Contains(got, "username=x-access-token\n"))
	assert.True(t, strings.Contains(got, "password=ghs_prototype\n"))
	assert.True(t, strings.Contains(got, "password_expiry_utc=1900000000\n"))
}

func TestRunGitCredentialHelperIgnoresNonGitHub(t *testing.T) {
	broker := &fakeBroker{}
	var out strings.Builder
	err := runGitCredentialHelper(context.Background(), &out, broker, "env_123", gitCredentialRequest{Host: "example.com", Path: "owner/repo"})
	assert.NoError(t, err)
	// No brokering and no output, so git falls back to its other sources.
	assert.Equal(t, "", broker.calledRepo)
	assert.Equal(t, "", out.String())
}

func TestGitCredentialHelperCommandIsRegisteredAndHidden(t *testing.T) {
	// store/erase are silent no-ops; exercise the command end-to-end for erase.
	result := newApp().Test(t, cli.TestArgs("git-credential-helper", "erase", "--api-key", "mbx_test", "--environment", "env_123"))
	if !result.Success() {
		t.Fatalf("git-credential-helper erase failed: %v\nstderr:\n%s", result.Err, result.Stderr)
	}
	// Hidden from the root help listing.
	help := newApp().Test(t, cli.TestArgs("--help"))
	assert.True(t, !strings.Contains(help.Stdout+help.Stderr, "git-credential-helper"))
}
